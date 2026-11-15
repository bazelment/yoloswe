// Package bramble implements lane.LaneDriver using a local bramble TUI.
package bramble

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	Client          Client
	RunDir          string
	Repo            string
	Parent          string
	PollInterval    time.Duration
	InactivityAfter time.Duration
}
type Driver struct { //nolint:govet // compact driver configuration
	c               Client
	runDir          string
	poll            time.Duration
	repo            string
	parent          string
	phases          map[string]string
	rounds          map[string]int
	lanes           map[string]string
	runs            map[string]string
	runDirs         map[string]string
	activeByLane    map[string]string
	stopped         map[string]bool
	inactivityAfter time.Duration
	mu              sync.RWMutex
}

func reportPath(runDir, runID, laneID, phase string, round int) string {
	if strings.TrimSpace(runDir) == "" {
		runDir = runID
	}
	return filepath.Join(runDir, laneID+"."+lane.PhaseRoundKey(phase, round)+".report.md")
}

var (
	_ lane.LaneDriver     = (*Driver)(nil)
	_ lane.GitStateReader = (*Driver)(nil)
)

func (d *Driver) ReportInstructions(runID, laneID, phase string, round int) string {
	d.mu.RLock()
	runDir := d.runDir
	d.mu.RUnlock()
	if runDir == "" {
		runDir = runID
	}
	path := reportPath(runDir, runID, laneID, phase, round)
	return "Write the phase report to " + path + "; the phase is not finished until that file is written. Start it with Outcome: done|no-change|needs-swe|blocked|needs-input, include HEAD and PR lines, and include a Reason: line when outcome is no-change; then include the report details."
}
func (d *Driver) PhaseSkill(phase string) string {
	switch phase {
	case "clean":
		return "/simplify"
	case "review":
		return "/pr-polish"
	}
	return ""
}

func New(cfg Config) (*Driver, error) {
	if cfg.Client == nil {
		return nil, errors.New("bramble client is required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 250 * time.Millisecond
	}
	if cfg.InactivityAfter <= 0 {
		cfg.InactivityAfter = time.Minute
	}
	return &Driver{
		c:               cfg.Client,
		runDir:          cfg.RunDir,
		repo:            cfg.Repo,
		parent:          cfg.Parent,
		poll:            cfg.PollInterval,
		inactivityAfter: cfg.InactivityAfter,
		phases:          map[string]string{},
		rounds:          map[string]int{},
		lanes:           map[string]string{},
		runs:            map[string]string{},
		runDirs:         map[string]string{},
		activeByLane:    map[string]string{},
		stopped:         map[string]bool{},
	}, nil
}
func (d *Driver) SpawnSession(ctx context.Context, req lane.SpawnRequest) (lane.SpawnResult, error) {
	return d.start(ctx, req)
}
func (d *Driver) StartPhaseTerminal(ctx context.Context, req lane.StartPhaseRequest) (lane.StartPhaseResult, error) {
	if req.SessionID != "" {
		prompt := req.Brief + "\n[orca run:" + req.RunID + " lane:" + req.LaneID + "]"
		if err := d.c.SendInput(ctx, req.SessionID, prompt); err != nil {
			return lane.StartPhaseResult{}, err
		}
		if err := d.c.SendKey(ctx, req.SessionID, "Enter"); err != nil {
			return lane.StartPhaseResult{}, err
		}
		laneKey := req.RunID + "/" + req.LaneID
		d.mu.Lock()
		prev := d.activeByLane[laneKey]
		if prev != "" && prev != req.SessionID {
			d.stopped[prev] = true
		}
		delete(d.stopped, req.SessionID)
		d.activeByLane[laneKey] = req.SessionID
		d.phases[req.SessionID] = req.Phase
		d.rounds[req.SessionID] = req.Round
		d.lanes[req.SessionID] = req.LaneID
		d.runs[req.SessionID] = req.RunID
		d.runDirs[req.SessionID] = req.RunDir
		d.mu.Unlock()
		if prev != "" && prev != req.SessionID {
			_ = d.c.SendKey(ctx, prev, "C-c")
		}
		return lane.StartPhaseResult{SessionID: req.SessionID, TerminalID: req.SessionID, StartedAt: time.Now().UTC()}, nil
	}
	r, err := d.start(ctx, lane.SpawnRequest{RunID: req.RunID, LaneID: req.LaneID, Phase: req.Phase, Harness: req.Harness, Model: req.Model, Effort: req.Effort, Brief: req.Brief, Round: req.Round, RunDir: req.RunDir})
	return lane.StartPhaseResult{SessionID: r.SessionID, TerminalID: r.TerminalID, StartedAt: r.StartedAt}, err
}
func (d *Driver) start(ctx context.Context, req lane.SpawnRequest) (lane.SpawnResult, error) {
	prompt := req.Brief + "\n[orca run:" + req.RunID + " lane:" + req.LaneID + "]"
	baseCommit := ""
	worktree := strings.TrimSpace(req.Worktree)
	if worktree == "" {
		worktree = d.repo
	}
	baseRef := strings.TrimSpace(req.BaseBranch)
	if baseRef == "" {
		baseRef = "origin/main"
	}
	if worktree != "" {
		if out, err := exec.CommandContext(ctx, "git", "-C", worktree, "rev-parse", baseRef).Output(); err == nil {
			baseCommit = strings.TrimSpace(string(out))
		}
	}
	r, err := d.c.NewSession(ctx, br.SpawnRequest{Worktree: req.Worktree, Repo: d.repo, Parent: d.parent, Model: req.Model, Backend: req.Harness, Effort: req.Effort, Prompt: prompt, CreateWorktree: req.Worktree == "", Branch: req.Branch, From: req.BaseBranch})
	if err != nil {
		return lane.SpawnResult{}, err
	}
	laneKey := req.RunID + "/" + req.LaneID
	d.mu.Lock()
	prev := d.activeByLane[laneKey]
	if prev != "" && prev != r.SessionID {
		d.stopped[prev] = true
	}
	delete(d.stopped, r.SessionID)
	d.activeByLane[laneKey] = r.SessionID
	d.phases[r.SessionID] = req.Phase
	d.rounds[r.SessionID] = req.Round
	d.lanes[r.SessionID] = req.LaneID
	d.runs[r.SessionID] = req.RunID
	d.runDirs[r.SessionID] = req.RunDir
	d.mu.Unlock()
	if prev != "" && prev != r.SessionID {
		_ = d.c.SendKey(ctx, prev, "C-c")
	}
	return lane.SpawnResult{SessionID: r.SessionID, TerminalID: r.SessionID, StartedAt: time.Now().UTC(), BaseCommit: baseCommit}, nil
}
func (d *Driver) Steer(ctx context.Context, req lane.SteerRequest) error {
	id := req.SessionID
	if id == "" {
		id = req.TerminalID
	}
	d.mu.RLock()
	stopped := d.stopped[id] || (req.TerminalID != "" && d.stopped[req.TerminalID])
	if !stopped && req.LaneID != "" {
		active := d.activeByLane[req.RunID+"/"+req.LaneID]
		if active != "" && id != "" && active != id {
			stopped = true
		}
	}
	d.mu.RUnlock()
	if stopped {
		return fmt.Errorf("%w: %s", lane.ErrStaleTerminal, id)
	}
	if err := d.c.SendInput(ctx, id, req.Message); err != nil {
		return err
	}
	return d.c.SendKey(ctx, id, "Enter")
}
func (d *Driver) Stop(ctx context.Context, req lane.StopRequest) error {
	id := req.SessionID
	if id == "" {
		id = req.TerminalID
	}
	if id != "" {
		d.mu.Lock()
		d.stopped[id] = true
		if req.TerminalID != "" {
			d.stopped[req.TerminalID] = true
		}
		d.mu.Unlock()
	}
	return d.c.SendKey(ctx, id, "C-c")
}

func (d *Driver) laneWorktree(ln lane.Lane) string {
	worktree := strings.TrimSpace(ln.Worktree)
	d.mu.RLock()
	if worktree == "" {
		worktree = strings.TrimSpace(d.runDirs[ln.SessionID])
	}
	if worktree == "" {
		worktree = d.repo
	}
	if worktree == "" {
		worktree = d.runDir
	}
	d.mu.RUnlock()
	return worktree
}

// LaneGitState inspects the local worktree for unpushed commits and dirty files.
func (d *Driver) LaneGitState(ctx context.Context, ln lane.Lane) (lane.GitState, error) {
	worktree := d.laneWorktree(ln)
	if worktree == "" {
		return lane.GitState{}, nil
	}
	base := strings.TrimPrefix(cmp.Or(strings.TrimSpace(ln.BaseBranch), "main"), "origin/")
	ref := "origin/" + base
	if branch := strings.TrimPrefix(strings.TrimSpace(ln.Branch), "origin/"); branch != "" {
		if err := exec.CommandContext(ctx, "git", "-C", worktree, "rev-parse", "--verify", "origin/"+branch).Run(); err == nil {
			ref = "origin/" + branch
		}
	}
	var st lane.GitState
	var oldest int64
	if out, err := exec.CommandContext(ctx, "git", "-C", worktree, "rev-list", "--count", ref+"..HEAD").Output(); err == nil {
		if n, convErr := strconv.Atoi(strings.TrimSpace(string(out))); convErr == nil && n > 0 {
			st.UnpushedCommits = n
		}
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", worktree, "log", "--reverse", "--format=%ct", ref+"..HEAD").Output(); err == nil {
		if first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n"); first != "" {
			if ts, convErr := strconv.ParseInt(strings.TrimSpace(first), 10, 64); convErr == nil && ts > 0 {
				oldest = ts
			}
		}
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", worktree, "status", "--porcelain").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if len(line) < 4 {
				continue
			}
			file := strings.TrimSpace(line[3:])
			if _, after, ok := strings.Cut(file, " -> "); ok {
				file = strings.TrimSpace(after)
			}
			if file == "" || file == ".claude/settings.local.json" {
				continue
			}
			st.DirtyFiles = append(st.DirtyFiles, file)
			if info, statErr := os.Stat(filepath.Join(worktree, file)); statErr == nil {
				if ts := info.ModTime().Unix(); ts > 0 && (oldest == 0 || ts < oldest) {
					oldest = ts
				}
			}
		}
	}
	if oldest > 0 {
		st.OldestUnpushedAt = time.Unix(oldest, 0).UTC()
	}
	return st, nil
}

// ReleaseSession stops the local bramble session after lane completion.
func (d *Driver) ReleaseSession(ctx context.Context, sessionID string, _ ...bool) error {
	return d.c.SendKey(ctx, strings.TrimSpace(sessionID), "C-c")
}

func (d *Driver) ReportOnlyNoWork(ctx context.Context, ln lane.Lane) (bool, error) {
	base := strings.TrimSpace(ln.BaseCommit)
	if base == "" {
		base = strings.TrimSpace(ln.BaseBranch)
	}
	branch := strings.TrimSpace(ln.Branch)
	if branch == "" || base == "" {
		return false, nil
	}
	worktree := strings.TrimSpace(ln.Worktree)
	d.mu.RLock()
	if worktree == "" {
		worktree = strings.TrimSpace(d.runDirs[ln.SessionID])
	}
	if worktree == "" {
		worktree = d.runDir
	}
	d.mu.RUnlock()
	if worktree == "" {
		worktree = "."
	}
	cmd := exec.CommandContext(ctx, "git", "-C", worktree, "rev-list", "--count", base+".."+branch)
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return false, err
	}
	return n == 0, nil
}
func (d *Driver) ReadReport(_ context.Context, runID, laneID, terminalID string) (*lane.PhaseReport, error) {
	d.mu.RLock()
	phase := d.phases[terminalID]
	round := d.rounds[terminalID]
	runDir := d.runDirs[terminalID]
	d.mu.RUnlock()
	if phase == "" {
		return nil, errors.New("unknown bramble terminal")
	}
	// Config.RunDir is the shared absolute report directory. Per-request
	// RunDir is retained for drivers without a configured shared directory.
	d.mu.RLock()
	configuredRunDir := d.runDir
	d.mu.RUnlock()
	if configuredRunDir != "" {
		runDir = configuredRunDir
	}
	if runDir == "" {
		runDir = runID
	}
	b, err := os.ReadFile(reportPath(runDir, runID, laneID, phase, round))
	if err != nil {
		return nil, err
	}
	return lane.ParsePhaseReport(string(b))
}
func (d *Driver) Watch(ctx context.Context, runID string) <-chan lane.Event {
	out := make(chan lane.Event, 16)
	go func() {
		defer close(out)
		reported := map[string]string{}
		lastStatus := map[string]string{}
		lastActivity := time.Now()
		quietEmitted := false
		quietSessions := map[string]bool{}
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
			hasWorking := false
			hasIdle := false
			hasUnreportedIdle := false
			var idleSessions []string
			for i := range ss {
				s := &ss[i]
				d.mu.RLock()
				owned := d.runs[s.ID] == runID
				phase := d.phases[s.ID]
				round := d.rounds[s.ID]
				laneID := d.lanes[s.ID]
				d.mu.RUnlock()
				if !owned {
					continue
				}
				reportKey := s.ID + ":" + lane.PhaseRoundKey(phase, round)
				if rep, err := d.ReadReport(ctx, runID, laneID, s.ID); err == nil && rep.Raw != reported[reportKey] {
					select {
					case out <- lane.Event{Kind: lane.EventPhaseReported, RunID: runID, LaneID: laneID, SessionID: s.ID, Phase: phase, Report: rep}:
					case <-ctx.Done():
						return
					}
					reported[reportKey] = rep.Raw
					// A phase report is a lifecycle transition even when the
					// backend keeps the same status (for example, waiting across
					// rounds). Re-arm status emission for the new round.
					delete(lastStatus, s.ID)
					lastActivity = time.Now()
					quietEmitted = false
					clear(quietSessions)
				}
				kind := lane.EventExited
				status := strings.ToLower(s.Status)
				switch status {
				case "working", "busy":
					hasWorking = true
					delete(quietSessions, s.ID)
					delete(lastStatus, s.ID)
					continue
				case "waiting", "wait", "waiting_for_input", "input.waiting":
					kind = lane.EventWaitingForInput
				case "quota", "quota_failure", "usage_limited", "rate_limited":
					kind = lane.EventQuotaFailure
				case "auth", "auth_failure", "unauthorized":
					kind = lane.EventAuthFailure
				case "turn_ended", "turn.ended", "ended":
					kind = lane.EventTurnEndedNoReport
				case "exited", "exit", "dead", "failed", "done", "complete", "completed", "stopped":
				default:
					hasIdle = true
					idleSessions = append(idleSessions, s.ID)
					if !quietSessions[s.ID] {
						hasUnreportedIdle = true
					}
					delete(lastStatus, s.ID)
					continue
				}
				if lastStatus[s.ID] == status {
					continue
				}
				select {
				case out <- lane.Event{Kind: kind, RunID: runID, LaneID: laneID, SessionID: s.ID, Phase: phase, Text: s.Status}:
				case <-ctx.Done():
					return
				}
				lastStatus[s.ID] = status
				lastActivity = time.Now()
				quietEmitted = false
				clear(quietSessions)
			}
			if hasWorking && !hasIdle {
				lastActivity = time.Now()
				quietEmitted = false
				clear(quietSessions)
				continue
			}
			if time.Since(lastActivity) >= d.inactivityAfter && (!quietEmitted || hasUnreportedIdle) {
				select {
				case out <- lane.Event{Kind: lane.EventNoActivity, RunID: runID, Since: lastActivity}:
				case <-ctx.Done():
					return
				}
				quietEmitted = true
				for _, id := range idleSessions {
					quietSessions[id] = true
				}
			}
		}
	}()
	return out
}
