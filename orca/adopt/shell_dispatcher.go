package adopt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/store"
	"github.com/bazelment/yoloswe/orca/toolbox"
)

// adoptedSystemPrompt makes each wake one tick of the host's skill, read from
// the skill file itself rather than restated here.
func adoptedSystemPrompt(skill string) string {
	return `You are the orchestrator for this adopted swarm run.
The controller supervises your session and lease. You own orchestration judgment.
Each wake is one tick of the run's skill: read ` + skill + ` and run its section 3 "Each tick" steps as the host orchestrator does, against this run's OBJECTIVE.md, state.json, EVENTS, reports, waits, and holds.
The watcher moves lanes swe to clean to review only; verifying reports, integrating, terminal care, and funding are yours.
Make every write the tick calls for through the orca shell toolbox: orca lane start|steer|stop|hold <lane>, orca pr approve|ready|update|merge|rerun <pr>, orca ask, orca note. orca status and orca check <lane> read.
Pass --action-key for each mutation; reuse that key on retry.
Run a write tool on its own and read its result; only then write a note or ask about it. Never put a write tool and a note that reports it in the same batch.
The required-check and review rules are authoritative in the target repository's CLAUDE.md (Build and Test) and in the skill's standing-rules.md at ` + filepath.Join(filepath.Dir(skill), "references", "standing-rules.md") + ` (swarm rules); read those files rather than inventing a substitute. For a red or timed-out required check on the PR's exact head, run scripts/known_reds.py --match <log> when the repository has it; otherwise run orca pr rerun <pr> once, then decide from the result. A review needs-swe with an empty Breaks: is accepted when its failure is not reproduced and exact-head CI is green: use the existing approve, ready, merge path. Past round 2, a needs-swe review with follow-up-grade Breaks (test-only, cosmetic, or outside the lane's remit) can use orca pr approve --accept "<reason>" to record the follow-up; a real defect returns to swe.
Verifying a phase report includes its claims under the standing-rules.md "Decisions" rule: check that the stated cause explains every observed failure mode and that the diff covers each case the report says it fixes. If the evidence or diff does not support a claim, send it back to swe with a mission naming the gap, or get one cheap read-only fact first; do not advance it.
The controller arms the watcher and wakes you: skip the skill's watcher and loop scheduling, and end each tick with orca wait.
Terminal care is that tick's step 1 and the "Signals" table in ` + filepath.Join(filepath.Dir(skill), "references", "nebula-lanes.md") + `: run orca check <lane> for each running swe, clean or review lane; it shows the phase terminal's agent state, minutes since its last output, whether its phase report is filed, its last turn's final line, its screen's last lines, and background=yes|no|unknown read from them.
- An idle Claude terminal with a background shell still running (background=yes: "N shell still running" on screen) waits on its own gate; it is not stuck.
- Steer an idle terminal only when background=no. When background=unknown, read its screen (nebula term screen <terminal>) before deciding.
- Idle with report_filed=no after its last turn printed a verdict: orca lane steer the lane to file its phase report.
- One command running far past its expected time (a lint or prepush over 30 min with no output): orca lane steer the lane to interrupt that command and run a narrower one.
When OBJECTIVE.md or the ledger notes say every lane holds advance and a lane lacks <lane>.hold-advance, run orca lane hold <lane>.
Integrating by hand: when OBJECTIVE.md or the ledger notes say integration is by hand (such as "no auto-integrate"), merging is yours under the merge rules in ` + skill + ` section 2 ("Merge a PR outside a lane's gate") and ` + filepath.Join(filepath.Dir(skill), "references", "standing-rules.md") + ` "Decisions":
- once a review PASS is verified on the exact PR head and the ledger has no approval for that head, run orca pr approve <pr>, then orca pr ready <pr>;
- once a review PASS is verified, or once an EVENTS row meets a merge condition the owner stated, run orca pr ready <pr>. A condition the owner set is a condition, not a reservation for the owner;
- when ready refuses only on a stale base, run orca pr update <pr>, then wait for exact-head CI on the updated head;
- then orca pr merge <pr>.
Beyond terminal care, do not infer a stall or expiry from age. Read what the host has already handled.
Owner and chat messages arrive in .orca-inbox/events.jsonl.
In shadow, write tools record intent; direct file edits affect only the copy.
`
}

// HostModel is the host orchestrator's model. Adopted dispatchers run on it
// unless --dispatcher-model names a Claude model.
const HostModel = "claude-opus-5-5"

// ShellDispatcher supervises a persistent CLI session with the host's tools.
// The skill's watcher and the shell toolbox own lifecycle operations; this
// supervisor only relays owner messages and wakes the agent.
//
//nolint:govet // configuration precedes synchronized lifecycle state.
type ShellDispatcher struct {
	Store     store.Store
	Config    model.AgentSessionConfig
	Run       Run
	Shadow    bool
	IntentDir string

	mu        sync.Mutex
	workspace string
	// scripts is the run's skill scripts directory toolbox.SkillDir resolved.
	scripts  string
	sessions model.AgentSessionMap
	done     chan struct{}
	stop     context.CancelFunc
	relayed  int
}

// Judge is inert: controller event batches cannot act on host runs.
func (d *ShellDispatcher) Judge(context.Context, string, []store.Event) error { return nil }

// Prepare launches like the host: built-in tools, bypassed permissions, and
// the orca toolbox on PATH. Shadow differs only in its synced copy and intents.
func (d *ShellDispatcher) Prepare() error {
	d.workspace = d.Run.Dir
	if d.Shadow {
		d.workspace = d.Config.WorkDir
		if d.workspace == "" || d.IntentDir == "" {
			return fmt.Errorf("shadow session requires a copy and an intent directory")
		}
		if rel, err := filepath.Rel(d.Run.Dir, d.IntentDir); err != nil || !strings.HasPrefix(rel, "..") {
			return fmt.Errorf("shadow intent directory must be outside the live run")
		}
		if err := d.Refresh(); err != nil {
			return err
		}
	}
	// The prompt and the watcher use the skill the toolbox resolves for the
	// live run, so a run without one fails here, not in a dead prompt path.
	scripts, err := toolbox.SkillDir(context.Background(), os.Getenv("ORCA_SNL_DIR"), d.Run.Dir)
	if err != nil {
		return err
	}
	d.scripts = scripts
	if m := strings.ToLower(d.Config.Model); m == "" || strings.HasPrefix(m, "gpt-") || strings.HasPrefix(m, "openai") {
		d.Config.Model = HostModel
	}
	// One session for the run: it rotates on a crash, a failed resume, or the
	// context limit, never on a turn count.
	d.Config.MaxSessionTurns = math.MaxInt
	d.Config.Provider, d.Config.WorkDir, d.Config.AllowBuiltins, d.Config.SystemPrompt = "claude", d.workspace, true, adoptedSystemPrompt(filepath.Join(filepath.Dir(scripts), "SKILL.md"))
	binDir := filepath.Join(d.workspace, ".orca-bin")
	executable, err := os.Executable()
	if err == nil {
		err = os.MkdirAll(binDir, 0o700)
	}
	if err == nil {
		wrapper := "#!/bin/sh\nexec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' \"$@\"\n"
		err = os.WriteFile(filepath.Join(binDir, "orca"), []byte(wrapper), 0o700)
	}
	if err != nil {
		return err
	}
	// The session's toolbox uses the same skill: a shadow copy has no env.sh.
	d.Config.Env = map[string]string{"PATH": binDir + string(os.PathListSeparator) + os.Getenv("PATH"), "ORCA_TOOLBOX_DIR": d.workspace, "ORCA_SNL_DIR": scripts}
	if d.Shadow {
		d.Config.Env["ORCA_TOOLBOX_SHADOW"], d.Config.Env["ORCA_TOOLBOX_INTENT_DIR"] = "1", d.IntentDir
		return nil
	}
	// The skill's lease check stands down for adopted runs; skill calls pass
	// the exact handover so the toolbox's guard can admit only this lease.
	authority, err := toolbox.ReadLeaseAuthority(d.Run.Dir)
	d.Config.Env["ORCA_ADOPT_AUTH"] = authority
	return err
}

// Refresh re-syncs a shadow session's copy from the live run.
func (d *ShellDispatcher) Refresh() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.Shadow {
		return nil
	}
	return SyncWorkspace(d.Run.Dir, d.workspace)
}

func (d *ShellDispatcher) Supervise(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	d.mu.Lock()
	d.done, d.stop = make(chan struct{}), cancel
	d.mu.Unlock()
	watchDone := make(chan struct{})
	go func() { defer close(watchDone); d.watch(ctx) }()
	defer func() { cancel(); <-watchDone; _ = d.sessions.Close(); close(d.done) }()
	threadID := "dispatcher-" + d.Run.ID
	if _, err := d.Store.Threads().StartThread(ctx, store.Thread{ID: threadID, RunID: d.Run.ID, Role: store.ThreadRoleDispatcher, Goal: d.Run.Goal, Status: store.ThreadStatusIdle}); err != nil && !strings.Contains(err.Error(), "already") {
		slog.Error("start adopted dispatcher thread", "error", err)
		return
	}
	wake := "initial handover"
	for seq := 1; ctx.Err() == nil; seq++ {
		if err := d.Refresh(); err != nil {
			slog.Error("refresh adopted dispatcher workspace", "error", err)
			return
		}
		if err := d.syncEventFiles(ctx); err != nil {
			slog.Error("relay adopted events", "error", err)
			return
		}
		d.mu.Lock()
		relayed := d.relayed
		d.mu.Unlock()
		turnID := fmt.Sprintf("%s-shell-%d-%d", threadID, time.Now().UnixNano(), seq)
		if _, err := d.Store.Threads().StartTurn(ctx, store.Turn{ID: turnID, ThreadID: threadID, RunID: d.Run.ID, Input: wake}); err != nil {
			slog.Error("start adopted dispatcher turn", "error", err)
			return
		}
		prompt := fmt.Sprintf("Wake: %s. This run is in %q; all relative edits belong there. Run one tick of the skill now, then orca wait.", wake, d.workspace)
		res, err := store.RunRoleAgentTurn(ctx, d.Store.Threads(), &d.sessions, d.Run.ID, threadID, turnID, "dispatcher", d.Config, model.AgentTurnRequest{Prompt: prompt}, nil)
		status, usage, result := store.TurnStatusCompleted, model.Usage{}, ""
		if res != nil {
			usage, result = res.Usage, res.Text
		}
		if err != nil {
			status, result = store.TurnStatusFailed, err.Error()
		}
		_ = d.Store.Threads().CompleteTurn(context.WithoutCancel(ctx), threadID, turnID, status, usage, result)
		wake = "run evidence changed; reread current evidence"
		if err != nil {
			// A 429 or crash retries the resumable session after a backoff.
			slog.Warn("adopted CLI session restarting", "run", d.Run.ID, "error", err)
			select {
			case <-ctx.Done():
			case <-time.After(30 * time.Second):
			}
			wake = "session restart; reread current host evidence"
		} else if err := d.waitAfterTurn(ctx, relayed); err != nil && ctx.Err() == nil {
			slog.Warn("adopted wait failed", "run", d.Run.ID, "error", err)
		}
	}
}

// waitAfterTurn waits for new evidence, unless an owner message was relayed
// during the turn. The baseline is taken before that check: a relay writes the
// inbox and bumps the count under d.mu, so a relay racing the check either
// shows in the count or changes the inbox after the baseline.
func (d *ShellDispatcher) waitAfterTurn(ctx context.Context, relayed int) error {
	before, err := toolbox.Snapshot(d.workspace)
	if err != nil {
		return err
	}
	d.mu.Lock()
	missed := d.relayed != relayed
	d.mu.Unlock()
	if missed {
		return nil
	}
	_, _, err = toolbox.WaitSince(ctx, d.workspace, before)
	return err
}

// syncEventFiles relays owner and chat messages to the session's inbox file,
// rewritten whole so a restarted supervisor needs no cursor.
func (d *ShellDispatcher) syncEventFiles(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Store == nil || d.Store.Events() == nil {
		return nil
	}
	events, err := d.Store.Events().List(ctx, store.EventFilter{RunID: d.Run.ID, Kinds: []store.EventKind{store.EventMail, store.EventOwnerMessage}})
	if err != nil || len(events) == d.relayed {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i := range events {
		ev := &events[i]
		if err := enc.Encode(map[string]any{"seq": ev.Seq, "kind": ev.Kind, "lane": ev.LaneID, "source": ev.Source, "summary": ev.Summary, "time": ev.CreatedAt}); err != nil {
			return err
		}
	}
	inbox := filepath.Join(d.workspace, ".orca-inbox")
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(inbox, "events.jsonl"), buf.Bytes(), 0o600); err != nil {
		return err
	}
	d.relayed = len(events)
	return nil
}

func (d *ShellDispatcher) Close() error {
	d.mu.Lock()
	stop, done := d.stop, d.done
	d.mu.Unlock()
	if stop != nil {
		stop()
	}
	if done != nil {
		<-done
	}
	return d.sessions.Close()
}

// skillShim links the skill's scripts into dir, with a step.sh that adds the
// adopted lease guard after the skill's own, so the run's watch_lanes.sh runs
// unchanged under the current handover. No installed skill file is changed.
func skillShim(scripts, dir string) (string, error) {
	shim := filepath.Join(dir, "scripts")
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(shim, 0o700); err != nil {
		return "", err
	}
	if err := os.Symlink(filepath.Join(scripts, "..", "references"), filepath.Join(dir, "references")); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(scripts)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.Name() != "step.sh" {
			if err := os.Symlink(filepath.Join(scripts, e.Name()), filepath.Join(shim, e.Name())); err != nil {
				return "", err
			}
		}
	}
	step := ". '" + filepath.Join(scripts, "step.sh") + "'\n" + toolbox.LeaseGuard + "\n"
	return filepath.Join(shim, "watch_lanes.sh"), os.WriteFile(filepath.Join(shim, "step.sh"), []byte(step), 0o600)
}

// watch runs the run's own watch_lanes.sh under the current handover and
// restarts it when it exits. Shadow runs leave the live watcher to the host.
func (d *ShellDispatcher) watch(ctx context.Context) {
	if d.Shadow {
		return
	}
	watcher, err := skillShim(d.scripts, filepath.Join(d.workspace, ".orca-bin", "snl"))
	if err != nil {
		slog.Error("adopted watcher unavailable", "run", d.Run.ID, "error", err)
		return
	}
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, "bash", watcher, d.workspace)
		cmd.Env = append(os.Environ(), "WATCH_NO_REARM=1", "SNL_RUN_ID="+filepath.Base(d.workspace),
			"ORCA_TOOLBOX_DIR="+d.workspace, "ORCA_ADOPT_AUTH="+d.Config.Env["ORCA_ADOPT_AUTH"])
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = time.Second
		if err := cmd.Run(); err != nil && ctx.Err() == nil {
			slog.Warn("adopted watcher restarting", "run", d.Run.ID, "error", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// SyncWorkspace refreshes a shadow session's disposable copy with the run's
// evidence files. Symlinks, dot directories and env.sh are never copied.
func SyncWorkspace(source, dest string) error {
	source, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	if dest, err = filepath.Abs(dest); err != nil {
		return err
	}
	if source == dest || strings.HasPrefix(dest, source+"/") || strings.HasPrefix(source, dest+"/") {
		return fmt.Errorf("shadow workspace overlaps source")
	}
	seen := map[string]string{}
	err = filepath.WalkDir(source, func(path string, e fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(source, path)
		switch {
		case err != nil:
			return err
		case e.IsDir() && rel != "." && strings.HasPrefix(e.Name(), "."):
			return filepath.SkipDir
		case e.IsDir():
			return workspaceDir(filepath.Join(dest, rel))
		case e.Type()&fs.ModeSymlink != 0 || !workspaceEvidence(rel, e.Name()):
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		seen[rel] = hex.EncodeToString(sum[:])
		// Replace rather than truncate, so a link the session made is not followed.
		target := filepath.Join(dest, rel)
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return err
		}
		return os.WriteFile(target, raw, 0o600)
	})
	if err != nil {
		return err
	}
	// Evidence the host removed (a hold, a report) disappears from the copy.
	err = filepath.WalkDir(dest, func(path string, e fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(dest, path)
		switch {
		case err != nil:
			return err
		case e.IsDir() && rel != "." && strings.HasPrefix(e.Name(), "."):
			return filepath.SkipDir
		case !e.IsDir() && workspaceEvidence(rel, e.Name()) && seen[rel] == "":
			return os.Remove(path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Stamp by live content: this sync rewrites every file, and orca wait
	// must see only what the host changed.
	return toolbox.WriteJSON(filepath.Join(dest, toolbox.SyncManifest), seen)
}

func workspaceEvidence(rel, name string) bool {
	for _, suffix := range []string{".md", ".done", ".needs-swe", ".hold-advance", ".brief", ".signal"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	rel = filepath.ToSlash(rel)
	return name == "EVENTS" || name == "state.json" || name == "heartbeat" || strings.HasPrefix(rel, "mail/") || strings.HasPrefix(rel, "chat/")
}

// workspaceDir refuses a directory with a symlink in any parent, so a refresh
// cannot write into the live run through a link the session created.
func workspaceDir(dir string) error {
	for p := filepath.Clean(dir); ; p = filepath.Dir(p) {
		if info, err := os.Lstat(p); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("workspace directory contains a symlink")
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
		if filepath.Dir(p) == p {
			return os.MkdirAll(dir, 0o700)
		}
	}
}
