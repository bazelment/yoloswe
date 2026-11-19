package adopt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
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

const adoptedSystemPrompt = `You are the dispatcher for this adopted swarm run.
The controller supervises your session and lease. You own orchestration judgment.
Read the run's skill, OBJECTIVE.md, state.json, EVENTS, reports, waits, and holds.
At every wake read EVENTS or run orca status before making any write decision.
Act through the orca shell toolbox: orca wait, orca status, orca check <lane>,
orca lane start|steer|stop <lane>, orca pr ready|merge <pr>, orca ask, orca note.
Pass --action-key for each mutation; reuse that key on retry.
The watcher owns mechanical phase moves. Wait for it; inspect its current state.
Do not infer a stall or expiry from age. Read what the host has already handled.
Owner and chat messages arrive in .orca-inbox/events.jsonl.
In shadow, write tools record intent; direct file edits affect only the copy.
Loop: orca wait, read what changed, decide, act through the toolbox, then wait again.
`

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
	sessions  model.AgentSessionMap
	done      chan struct{}
	stop      context.CancelFunc
	relayed   int
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
	if m := strings.ToLower(d.Config.Model); strings.HasPrefix(m, "gpt-") || strings.HasPrefix(m, "openai") {
		d.Config.Model = "sonnet"
	}
	d.Config.Provider, d.Config.WorkDir, d.Config.AllowBuiltins, d.Config.SystemPrompt = "claude", d.workspace, true, adoptedSystemPrompt
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
	d.Config.Env = map[string]string{"PATH": binDir + string(os.PathListSeparator) + os.Getenv("PATH"), "ORCA_TOOLBOX_DIR": d.workspace}
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
		prompt := fmt.Sprintf("Wake: %s. This run is in %q; all relative edits belong there. Read the evidence and run orca status before deciding, act through the toolbox, then orca wait.", wake, d.workspace)
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
	_, err = toolbox.WaitSince(ctx, d.workspace, before)
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
	script := FindWatchScript()
	if d.Shadow || script == "" {
		return
	}
	watcher, err := skillShim(filepath.Dir(script), filepath.Join(d.workspace, ".orca-bin", "snl"))
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
	seen := map[string]bool{}
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
		seen[rel] = true
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
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
	return filepath.WalkDir(dest, func(path string, e fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(dest, path)
		switch {
		case err != nil:
			return err
		case e.IsDir() && rel != "." && strings.HasPrefix(e.Name(), "."):
			return filepath.SkipDir
		case !e.IsDir() && workspaceEvidence(rel, e.Name()) && !seen[rel]:
			return os.Remove(path)
		}
		return nil
	})
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
