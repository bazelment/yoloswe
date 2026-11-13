// Package bramble implements lane.LaneDriver using a local bramble TUI.
package bramble

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/orca/lane"
	br "github.com/bazelment/yoloswe/swarm-queen/bramble"
)

type Client interface {
	NewSession(context.Context, br.SpawnRequest) (br.SpawnResult, error)
	SendInput(context.Context, string, string) error
	SendKey(context.Context, string, string) error
	ListSessions(context.Context) ([]br.Session, error)
}
type Config struct {
	Client       Client
	RunDir       string
	Repo         string
	Parent       string
	PollInterval time.Duration
}
type Driver struct { //nolint:govet // compact driver configuration
	c      Client
	runDir string
	poll   time.Duration
	repo   string
	parent string
	phases map[string]string
	lanes  map[string]string
	runs   map[string]string
	mu     sync.RWMutex
}

var _ lane.LaneDriver = (*Driver)(nil)

func New(cfg Config) (*Driver, error) {
	if cfg.Client == nil {
		return nil, errors.New("bramble client is required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 250 * time.Millisecond
	}
	return &Driver{c: cfg.Client, runDir: cfg.RunDir, repo: cfg.Repo, parent: cfg.Parent, poll: cfg.PollInterval, phases: map[string]string{}, lanes: map[string]string{}, runs: map[string]string{}}, nil
}
func (d *Driver) SpawnSession(ctx context.Context, req lane.SpawnRequest) (lane.SpawnResult, error) {
	return d.start(ctx, req)
}
func (d *Driver) StartPhaseTerminal(ctx context.Context, req lane.StartPhaseRequest) (lane.StartPhaseResult, error) {
	r, err := d.start(ctx, lane.SpawnRequest{RunID: req.RunID, LaneID: req.LaneID, Phase: req.Phase, Harness: req.Harness, Model: req.Model, Effort: req.Effort, Brief: req.Brief, Round: req.Round, RunDir: d.runDir})
	return lane.StartPhaseResult{SessionID: r.SessionID, TerminalID: r.TerminalID, StartedAt: r.StartedAt}, err
}
func (d *Driver) start(ctx context.Context, req lane.SpawnRequest) (lane.SpawnResult, error) {
	prompt := req.Brief + "\n[orca run:" + req.RunID + " lane:" + req.LaneID + "]"
	r, err := d.c.NewSession(ctx, br.SpawnRequest{Worktree: req.Worktree, Repo: d.repo, Parent: d.parent, Model: req.Model, Backend: req.Harness, Effort: req.Effort, Prompt: prompt, CreateWorktree: req.Worktree == "", Branch: req.Branch, From: req.BaseBranch})
	if err != nil {
		return lane.SpawnResult{}, err
	}
	d.mu.Lock()
	d.phases[r.SessionID] = req.Phase
	d.lanes[r.SessionID] = req.LaneID
	d.runs[r.SessionID] = req.RunID
	d.mu.Unlock()
	return lane.SpawnResult{SessionID: r.SessionID, TerminalID: r.SessionID, StartedAt: time.Now().UTC()}, nil
}
func (d *Driver) Steer(ctx context.Context, req lane.SteerRequest) error {
	id := req.SessionID
	if id == "" {
		id = req.TerminalID
	}
	if err := d.c.SendInput(ctx, id, req.Message); err != nil {
		return err
	}
	return d.c.SendKey(ctx, req.SessionID, "Enter")
}
func (d *Driver) Stop(ctx context.Context, req lane.StopRequest) error {
	id := req.SessionID
	if id == "" {
		id = req.TerminalID
	}
	return d.c.SendKey(ctx, id, "C-c")
}
func (d *Driver) reportPath(runID, laneID, phase string) string {
	dir := d.runDir
	if dir == "" {
		dir = runID
	}
	return filepath.Join(dir, laneID+"."+phase+".report.md")
}
func (d *Driver) ReadReport(_ context.Context, runID, laneID, terminalID string) (*lane.PhaseReport, error) {
	d.mu.RLock()
	phase := d.phases[terminalID]
	d.mu.RUnlock()
	if phase == "" {
		return nil, errors.New("unknown bramble terminal")
	}
	b, err := os.ReadFile(d.reportPath(runID, laneID, phase))
	if err != nil {
		return nil, err
	}
	return lane.ParsePhaseReport(string(b))
}
func (d *Driver) Watch(ctx context.Context, runID string) <-chan lane.Event {
	out := make(chan lane.Event, 16)
	go func() {
		defer close(out)
		reported := map[string]bool{}
		exited := map[string]bool{}
		lastActivity := time.Now()
		t := time.NewTicker(d.poll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			ss, err := d.c.ListSessions(ctx)
			if err != nil {
				continue
			}
			for i := range ss {
				s := &ss[i]
				d.mu.RLock()
				owned := d.runs[s.ID] == runID
				phase := d.phases[s.ID]
				laneID := d.lanes[s.ID]
				d.mu.RUnlock()
				if !owned {
					continue
				}
				if !reported[s.ID] {
					if rep, err := d.ReadReport(ctx, runID, laneID, s.ID); err == nil {
						select {
						case out <- lane.Event{Kind: lane.EventPhaseReported, RunID: runID, LaneID: laneID, SessionID: s.ID, Phase: phase, Report: rep}:
						case <-ctx.Done():
							return
						}
						reported[s.ID] = true
						lastActivity = time.Now()
						continue
					}
				}
				if exited[s.ID] {
					continue
				}
				kind := lane.EventExited
				status := strings.ToLower(s.Status)
				terminal := strings.Contains(status, "exit") || strings.Contains(status, "dead") || strings.Contains(status, "failed") || strings.Contains(status, "done") || strings.Contains(status, "complete")
				if !terminal && (strings.Contains(status, "run") || strings.Contains(status, "active") || status == "") {
					continue
				}
				if strings.Contains(status, "wait") {
					kind = lane.EventWaitingForInput
				}
				if strings.Contains(status, "quota") || strings.Contains(status, "limit") {
					kind = lane.EventQuotaFailure
				}
				if strings.Contains(status, "auth") {
					kind = lane.EventAuthFailure
				}
				select {
				case out <- lane.Event{Kind: kind, RunID: runID, LaneID: laneID, SessionID: s.ID, Phase: phase, Text: s.Status}:
				case <-ctx.Done():
					return
				}
				exited[s.ID] = true
				lastActivity = time.Now()
			}
			if time.Since(lastActivity) >= time.Minute {
				select {
				case out <- lane.Event{Kind: lane.EventNoActivity, RunID: runID, Since: lastActivity}:
				case <-ctx.Done():
					return
				}
				lastActivity = time.Now()
			}
		}
	}()
	return out
}
