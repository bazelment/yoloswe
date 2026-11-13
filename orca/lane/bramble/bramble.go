// Package bramble implements lane.LaneDriver using a local bramble TUI.
package bramble

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	PollInterval time.Duration
}
type Driver struct { //nolint:govet // compact driver configuration
	c      Client
	runDir string
	poll   time.Duration
	phases map[string]string
	lanes  map[string]string
}

var _ lane.LaneDriver = (*Driver)(nil)

func New(cfg Config) (*Driver, error) {
	if cfg.Client == nil {
		return nil, errors.New("bramble client is required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 250 * time.Millisecond
	}
	return &Driver{c: cfg.Client, runDir: cfg.RunDir, poll: cfg.PollInterval, phases: map[string]string{}, lanes: map[string]string{}}, nil
}
func (d *Driver) SpawnSession(ctx context.Context, req lane.SpawnRequest) (lane.SpawnResult, error) {
	return d.start(ctx, req)
}
func (d *Driver) StartPhaseTerminal(ctx context.Context, req lane.StartPhaseRequest) (lane.StartPhaseResult, error) {
	r, err := d.start(ctx, lane.SpawnRequest{RunID: req.RunID, LaneID: req.LaneID, Phase: req.Phase, Harness: req.Harness, Model: req.Model, Effort: req.Effort, Brief: req.Brief, Round: req.Round, RunDir: d.runDir})
	return lane.StartPhaseResult{SessionID: r.SessionID, TerminalID: r.TerminalID, StartedAt: r.StartedAt}, err
}
func (d *Driver) start(ctx context.Context, req lane.SpawnRequest) (lane.SpawnResult, error) {
	r, err := d.c.NewSession(ctx, br.SpawnRequest{Worktree: req.Worktree, Repo: "", Model: req.Model, Backend: req.Harness, Effort: req.Effort, Prompt: req.Brief, CreateWorktree: req.Worktree == "", Branch: req.Branch, From: req.BaseBranch})
	if err != nil {
		return lane.SpawnResult{}, err
	}
	d.phases[r.SessionID] = req.Phase
	d.lanes[r.SessionID] = req.LaneID
	return lane.SpawnResult{SessionID: r.SessionID, TerminalID: r.SessionID, StartedAt: time.Now().UTC()}, nil
}
func (d *Driver) Steer(ctx context.Context, req lane.SteerRequest) error {
	if err := d.c.SendInput(ctx, req.SessionID, req.Message); err != nil {
		return err
	}
	return d.c.SendKey(ctx, req.SessionID, "Enter")
}
func (d *Driver) Stop(ctx context.Context, req lane.StopRequest) error {
	return d.c.SendKey(ctx, req.SessionID, "C-c")
}
func (d *Driver) reportPath(runID, laneID, phase string) string {
	dir := d.runDir
	if dir == "" {
		dir = runID
	}
	return filepath.Join(dir, laneID+"."+phase+".report.md")
}
func (d *Driver) ReadReport(_ context.Context, runID, laneID, terminalID string) (*lane.PhaseReport, error) {
	b, err := os.ReadFile(d.reportPath(runID, laneID, d.phases[terminalID]))
	if err != nil {
		return nil, err
	}
	return lane.ParsePhaseReport(string(b))
}
func (d *Driver) Watch(ctx context.Context, runID string) <-chan lane.Event {
	out := make(chan lane.Event, 16)
	go func() {
		defer close(out)
		seen := map[string]bool{}
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
				if seen[s.ID] {
					continue
				}
				p := strings.TrimSpace(s.Prompt)
				if strings.Contains(p, runID) {
					if strings.EqualFold(s.Status, "failed") {
						out <- lane.Event{Kind: lane.EventExited, RunID: runID, SessionID: s.ID, Text: s.Status}
						seen[s.ID] = true
					}
				}
			}
		}
	}()
	return out
}

var _ = fmt.Sprintf
