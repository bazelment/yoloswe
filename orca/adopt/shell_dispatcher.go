package adopt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/orca/dispatcher"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/store"
	"github.com/bazelment/yoloswe/orca/toolbox"
)

// ShellDispatcher supervises a persistent CLI session. The host watcher and
// shell toolbox own lifecycle operations; this supervisor only wakes the agent.
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
	eventSeq  int64
}

// Judge is deliberately inert: controller event batches cannot act on host runs.
func (d *ShellDispatcher) Judge(context.Context, string, []store.Event) error { return nil }

func (d *ShellDispatcher) SetStore(st store.Store) { d.Store = st }

func (d *ShellDispatcher) Prepare() error {
	d.workspace = d.Run.Dir
	if d.Shadow {
		d.workspace = d.Config.WorkDir
		if d.workspace == "" || d.IntentDir == "" {
			return fmt.Errorf("shadow session requires a copy and an intent directory")
		}
		if err := d.Refresh(); err != nil {
			return err
		}
		live, err := filepath.Abs(d.Run.Dir)
		if err != nil {
			return err
		}
		intent, err := filepath.Abs(d.IntentDir)
		if err != nil || live == intent || strings.HasPrefix(intent, live+string(os.PathSeparator)) {
			return fmt.Errorf("shadow intent directory must be outside the live run")
		}
	}
	d.Config.Provider = "claude"
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(d.Config.Model)), "gpt-") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(d.Config.Model)), "openai") {
		d.Config.Model = "sonnet"
	}
	d.Config.WorkDir = d.workspace
	d.Config.AllowBuiltins = true
	d.Config.SystemPrompt = dispatcher.AdoptedSystemPrompt
	binDir := filepath.Join(d.workspace, ".orca-bin")
	if err := os.MkdirAll(binDir, 0700); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	wrapper := "#!/bin/sh\nexec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "orca"), []byte(wrapper), 0700); err != nil {
		return err
	}
	if d.Shadow {
		realClaude := d.Config.ClaudeBin
		if realClaude == "" {
			realClaude = "claude"
		}
		quoted := strings.ReplaceAll(realClaude, "'", "'\\''")
		// Hiding HOME and binding only the disposable workspace prevents direct
		// file writes from reaching the live run. Network remains available for
		// the configured model gateway; shell permissions below deny external
		// Nebula/GitHub mutations.
		sandbox := "#!/bin/sh\nexec bwrap --die-with-parent --ro-bind / / --tmpfs /home --dir /home/ubuntu --dir /home/ubuntu/.claude --ro-bind /home/ubuntu/.claude/skills /home/ubuntu/.claude/skills --bind \"$ORCA_TOOLBOX_DIR\" \"$ORCA_TOOLBOX_DIR\" --dev /dev --proc /proc --tmpfs /tmp --chdir \"$ORCA_TOOLBOX_DIR\" --setenv HOME /home/ubuntu '" + quoted + "' \"$@\"\n"
		claudeWrapper := filepath.Join(binDir, "claude-shadow")
		if err := os.WriteFile(claudeWrapper, []byte(sandbox), 0700); err != nil {
			return err
		}
		d.Config.ClaudeBin = claudeWrapper
	}
	d.Config.Env = map[string]string{
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"ORCA_TOOLBOX_DIR": d.workspace,
	}
	// The skill's normal lease guard rejects every operation once a run is
	// adopted. Toolbox calls are the lease holder's authorized mechanical
	// path, so pass the exact marker and let the adapter verify it before
	// overriding that guard.
	if !d.Shadow {
		authority, err := toolbox.ReadLeaseAuthority(d.Run.Dir)
		if err != nil {
			return err
		}
		d.Config.Env["ORCA_ADOPT_AUTH"] = authority
	}
	if d.Shadow {
		d.Config.Env["ORCA_TOOLBOX_SHADOW"] = "1"
		d.Config.Env["ORCA_TOOLBOX_INTENT_DIR"] = d.IntentDir
	}
	return nil
}

func (d *ShellDispatcher) Refresh() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.Shadow {
		return nil
	}
	return SyncWorkspace(d.Run.Dir, d.workspace)
}

func (d *ShellDispatcher) Supervise(ctx context.Context) {
	loopCtx, cancel := context.WithCancel(ctx)
	d.mu.Lock()
	d.done, d.stop = make(chan struct{}), cancel
	d.mu.Unlock()
	defer close(d.done)
	defer cancel()
	defer d.sessions.Close()
	watchDone := make(chan struct{})
	go func() { defer close(watchDone); d.watch(loopCtx) }()
	defer func() { cancel(); <-watchDone }()
	threadID := "dispatcher-" + d.Run.ID
	_, err := d.Store.Threads().StartThread(loopCtx, store.Thread{ID: threadID, RunID: d.Run.ID, Role: store.ThreadRoleDispatcher, Goal: d.Run.Goal, Status: store.ThreadStatusIdle})
	if err != nil && !strings.Contains(err.Error(), "already") {
		slog.Error("start adopted dispatcher thread", "error", err)
		return
	}
	wake := "initial handover"
	for seq := 1; loopCtx.Err() == nil; seq++ {
		if err := d.Refresh(); err != nil {
			slog.Error("refresh adopted dispatcher workspace", "error", err)
			return
		}
		if err := d.syncEventFiles(loopCtx); err != nil {
			slog.Error("relay adopted events", "error", err)
			return
		}
		turnID := fmt.Sprintf("%s-shell-%d-%d", threadID, time.Now().UnixNano(), seq)
		_, err := d.Store.Threads().StartTurn(loopCtx, store.Turn{ID: turnID, ThreadID: threadID, RunID: d.Run.ID, Input: wake})
		if err != nil {
			slog.Error("start adopted dispatcher turn", "error", err)
			return
		}
		prompt := fmt.Sprintf("Wake: %s. This run is in %q. Read OBJECTIVE.md, EVENTS, reports, waits and holds, and run orca status before deciding. Use shell toolbox commands with action keys. Loop with orca wait after actions. All relative edits belong in this directory.\n\nRun skill tick:\n%s", wake, d.workspace, adoptedSkillTick(d.workspace))
		res, turnErr := store.RunRoleAgentTurn(loopCtx, d.Store.Threads(), &d.sessions, d.Run.ID, threadID, turnID, "dispatcher", d.Config, model.AgentTurnRequest{Prompt: prompt}, nil)
		usage := model.Usage{}
		status, result := store.TurnStatusCompleted, ""
		if res != nil {
			usage, result = res.Usage, res.Text
		}
		if turnErr != nil {
			status, result = store.TurnStatusFailed, turnErr.Error()
		}
		_ = d.Store.Threads().CompleteTurn(context.WithoutCancel(loopCtx), threadID, turnID, status, usage, result)
		if loopCtx.Err() != nil {
			return
		}
		if turnErr != nil {
			// A 429/crash retries the resumable session after bounded backoff.
			slog.Warn("adopted CLI session restarting", "run", d.Run.ID, "error", turnErr)
			timer := time.NewTimer(30 * time.Second)
			select {
			case <-loopCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			wake = "session restart; reread current host evidence"
			continue
		}
		// A model may end its turn instead of issuing a wait. Keep the same
		// session and block here, using the identical CLI wait implementation.
		if err := toolbox.Run(loopCtx, []string{"wait", "--dir", d.workspace}, nil, io.Discard); err != nil && loopCtx.Err() != nil {
			return
		}
		wake = "run evidence changed or heartbeat; reread current evidence"
	}
}

func adoptedSkillTick(dir string) string {
	for _, name := range []string{"role.md", "TEAM_RECIPE", "skill.md"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil && strings.TrimSpace(string(b)) != "" {
			if len(b) > 12000 {
				b = b[:12000]
			}
			return strings.TrimSpace(string(b))
		}
	}
	return "(no run-local skill tick file; follow the adopted dispatcher contract)"
}

func (d *ShellDispatcher) syncEventFiles(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Store == nil || d.Store.Events() == nil {
		return nil
	}
	if d.eventSeq == 0 {
		// Resume after a supervisor restart without replaying the entire inbox.
		// The inbox is evidence, so derive the cursor from its persisted rows.
		path := filepath.Join(d.workspace, ".orca-inbox", "events.jsonl")
		if f, err := os.Open(path); err == nil {
			dec := json.NewDecoder(f)
			for {
				var row struct {
					Seq int64 `json:"seq"`
				}
				if err := dec.Decode(&row); err != nil {
					break
				}
				if row.Seq > d.eventSeq {
					d.eventSeq = row.Seq
				}
			}
			_ = f.Close()
		}
	}
	events, err := d.Store.Events().List(ctx, store.EventFilter{RunID: d.Run.ID, AfterSeq: d.eventSeq, Kinds: []store.EventKind{store.EventMail, store.EventOwnerMessage}})
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(d.workspace, ".orca-inbox"), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(d.workspace, ".orca-inbox", "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	for i := range events {
		ev := &events[i]
		b, marshalErr := json.Marshal(map[string]any{"seq": ev.Seq, "kind": ev.Kind, "lane": ev.LaneID, "source": ev.Source, "summary": ev.Summary, "time": ev.CreatedAt})
		if marshalErr != nil {
			return marshalErr
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			return err
		}
		if ev.Seq > d.eventSeq {
			d.eventSeq = ev.Seq
		}
	}
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
