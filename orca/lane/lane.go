// Package lane defines the Lane domain entity, phase report format, and the
// LaneDriver worker execution interface.
package lane

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Status represents where a lane is in its lifecycle.
type Status string

const (
	StatusPlanned Status = "planned"
	StatusRunning Status = "running"
	StatusBlocked Status = "blocked"
	StatusFailed  Status = "failed"
	StatusDone    Status = "done"
)

// Terminal reports whether s is a terminal lane status (done or failed).
func (s Status) Terminal() bool {
	return s == StatusDone || s == StatusFailed
}

func (s Status) Valid() bool {
	switch s {
	case StatusPlanned, StatusRunning, StatusBlocked, StatusFailed, StatusDone:
		return true
	default:
		return false
	}
}

// Lane is one unit of work moving through a recipe's phases.
type Lane struct {
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
	LastEventAt   time.Time         `json:"last_event_at"`
	Sessions      map[string]string `json:"sessions,omitempty"`
	Terminals     map[string]string `json:"terminals,omitempty"`
	ID            string            `json:"id"`
	RunID         string            `json:"run_id"`
	ItemID        string            `json:"item_id,omitempty"`
	Title         string            `json:"title,omitempty"`
	Branch        string            `json:"branch,omitempty"`
	BaseBranch    string            `json:"base_branch,omitempty"`
	BaseCommit    string            `json:"base_commit,omitempty"`
	Worktree      string            `json:"worktree,omitempty"`
	Status        Status            `json:"status"`
	Phase         string            `json:"phase,omitempty"`
	RecipeName    string            `json:"recipe_name,omitempty"`
	SessionID     string            `json:"session_id,omitempty"`
	TerminalID    string            `json:"terminal_id,omitempty"`
	PRHead        string            `json:"pr_head,omitempty"`
	ApprovalSHA   string            `json:"approval_sha,omitempty"`
	MergeSHA      string            `json:"merge_sha,omitempty"`
	Checks        string            `json:"checks,omitempty"`
	LastEventID   string            `json:"last_event_id,omitempty"`
	Round         int               `json:"round,omitempty"`
	Attempt       int               `json:"attempt,omitempty"`
	RecipeVersion int               `json:"recipe_version,omitempty"`
	PR            int               `json:"pr,omitempty"`
}

func (l Lane) Clone() Lane {
	out := l
	if l.Sessions != nil {
		out.Sessions = maps.Clone(l.Sessions)
	}
	if l.Terminals != nil {
		out.Terminals = maps.Clone(l.Terminals)
	}
	return out
}

// PhaseRoundKey returns the canonical attempt key for (phase, round).
// Round <= 1 uses the bare phase name; round >= 2 appends the round number.
func PhaseRoundKey(phase string, round int) string {
	if phase == "" {
		return ""
	}
	if round <= 1 {
		return phase
	}
	return fmt.Sprintf("%s%d", phase, round)
}

// SplitPhaseRound splits a PhaseRoundKey into its base phase name and round.
func SplitPhaseRound(key string) (string, int) {
	if key == "" {
		return "", 0
	}
	i := len(key)
	for i > 0 && key[i-1] >= '0' && key[i-1] <= '9' {
		i--
	}
	if i == len(key) {
		return key, 1
	}
	n, err := strconv.Atoi(key[i:])
	if err != nil || n < 1 {
		return key, 1
	}
	return key[:i], n
}

// AttemptKey returns the PhaseRoundKey for the lane's current Phase and Round.
func (l *Lane) AttemptKey() string {
	return PhaseRoundKey(l.Phase, l.Round)
}

// MaxRound returns the highest recorded round for phase across l.Sessions and
// l.Terminals, or 0 if phase has never been recorded.
func (l *Lane) MaxRound(phase string) int {
	maxR := 0
	for k := range l.Sessions {
		if p, r := SplitPhaseRound(k); p == phase && r > maxR {
			maxR = r
		}
	}
	for k := range l.Terminals {
		if p, r := SplitPhaseRound(k); p == phase && r > maxR {
			maxR = r
		}
	}
	return maxR
}

// RecordAttempt records sessionID and terminalID for (phase, round) and sets
// the lane's active Phase, Round, SessionID, and TerminalID.
func (l *Lane) RecordAttempt(phase string, round int, sessionID, terminalID string) {
	if round < 1 {
		round = 1
	}
	l.Phase = phase
	l.Round = round
	l.SessionID = sessionID
	l.TerminalID = terminalID
	key := PhaseRoundKey(phase, round)
	if sessionID != "" {
		if l.Sessions == nil {
			l.Sessions = make(map[string]string)
		}
		l.Sessions[key] = sessionID
	}
	if terminalID != "" {
		if l.Terminals == nil {
			l.Terminals = make(map[string]string)
		}
		l.Terminals[key] = terminalID
	}
}

// ApprovalStale reports whether the recorded review approval does not match
// the current PR head SHA. Missing values are treated as stale.
func (l *Lane) ApprovalStale() bool {
	if l.ApprovalSHA == "" || l.PRHead == "" {
		return true
	}
	return l.ApprovalSHA != l.PRHead
}

// Validate checks that l has valid identity and status fields.
func (l *Lane) Validate() error {
	if strings.TrimSpace(l.ID) == "" {
		return fmt.Errorf("lane id is required")
	}
	if strings.ContainsRune(l.ID, '.') {
		return fmt.Errorf("lane %q: id must not contain '.'", l.ID)
	}
	if strings.TrimSpace(l.RunID) == "" {
		return fmt.Errorf("lane %q: run_id is required", l.ID)
	}
	if !l.Status.Valid() {
		return fmt.Errorf("lane %q: invalid status %q", l.ID, l.Status)
	}
	if l.Round < 0 {
		return fmt.Errorf("lane %q: round must be >= 0", l.ID)
	}
	if l.RecipeVersion < 0 {
		return fmt.Errorf("lane %q: recipe_version must be >= 0", l.ID)
	}
	return nil
}

// ReportOutcome is the outcome reported by a worker at the end of a phase turn.
type ReportOutcome string

const (
	OutcomeDone       ReportOutcome = "done"
	OutcomeNoChange   ReportOutcome = "no-change"
	OutcomeNeedsSWE   ReportOutcome = "needs-swe"
	OutcomeBlocked    ReportOutcome = "blocked"
	OutcomeNeedsInput ReportOutcome = "needs-input"
)

func (o ReportOutcome) Valid() bool {
	switch o {
	case OutcomeDone, OutcomeNoChange, OutcomeNeedsSWE, OutcomeBlocked, OutcomeNeedsInput:
		return true
	default:
		return false
	}
}

// PhaseReport is the structured phase completion report submitted by a lane terminal.
type PhaseReport struct {
	ReportedAt    time.Time     `json:"reported_at"`
	LaneID        string        `json:"lane_id"`
	TerminalID    string        `json:"terminal_id"`
	Phase         string        `json:"phase"`
	Outcome       ReportOutcome `json:"outcome"`
	HeadSHA       string        `json:"head_sha,omitempty"`
	Summary       string        `json:"summary,omitempty"`
	Reason        string        `json:"reason,omitempty"`
	For           string        `json:"for,omitempty"`
	Ask           string        `json:"ask,omitempty"`
	Breaks        string        `json:"breaks,omitempty"`
	RiskiestStep  string        `json:"riskiest_step,omitempty"`
	Observed      string        `json:"observed,omitempty"`
	Verdict       string        `json:"verdict,omitempty"`
	Reach         string        `json:"reach,omitempty"`
	ReachObserved string        `json:"reach_observed,omitempty"`
	Change        string        `json:"change,omitempty"`
	Raw           string        `json:"raw,omitempty"`
	Round         int           `json:"round,omitempty"`
	PR            int           `json:"pr,omitempty"`
}

// ParsePhaseReport parses a structured Nebula phase report markdown payload.
func ParsePhaseReport(raw string) (*PhaseReport, error) {
	rep := &PhaseReport{Raw: raw}
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	var body []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if k, v, ok := splitHeader(trimmed); ok && applyReportHeader(rep, k, v) {
			continue
		}
		if trimmed != "" {
			body = append(body, trimmed)
		}
	}
	rep.Summary = strings.Join(body, "\n")
	if !rep.Outcome.Valid() {
		return nil, fmt.Errorf("invalid or missing phase report Outcome %q", rep.Outcome)
	}
	return rep, nil
}

func applyReportHeader(rep *PhaseReport, key, val string) bool {
	switch strings.ToLower(key) {
	case "outcome":
		rep.Outcome = ReportOutcome(val)
	case "head":
		rep.HeadSHA = val
	case "pr":
		if n, err := strconv.Atoi(strings.TrimPrefix(val, "#")); err == nil {
			rep.PR = n
		}
	case "reason":
		rep.Reason = val
	case "for":
		rep.For = val
	case "ask":
		rep.Ask = val
	case "breaks":
		rep.Breaks = val
	case "riskiest step":
		rep.RiskiestStep = val
	case "observed":
		rep.Observed = val
	case "verdict":
		rep.Verdict = val
	case "reach":
		rep.Reach = val
	case "reach observed":
		rep.ReachObserved = val
	case "change":
		rep.Change = val
	default:
		return false
	}
	return true
}

func splitHeader(line string) (string, string, bool) {
	k, v, ok := strings.Cut(line, ":")
	if !ok {
		return "", "", false
	}
	k = strings.TrimSpace(k)
	if k == "" {
		return "", "", false
	}
	return k, strings.TrimSpace(v), true
}

// SpawnRequest holds the parameters to spawn a new worker session for a lane.
type SpawnRequest struct {
	RunDir     string            `json:"run_dir,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	RunID      string            `json:"run_id"`
	LaneID     string            `json:"lane_id"`
	Phase      string            `json:"phase"`
	Harness    string            `json:"harness"`
	Model      string            `json:"model"`
	Effort     string            `json:"effort,omitempty"`
	Brief      string            `json:"brief"`
	Branch     string            `json:"branch,omitempty"`
	BaseBranch string            `json:"base_branch,omitempty"`
	Worktree   string            `json:"worktree,omitempty"`
	Round      int               `json:"round"`
	SharedHome bool              `json:"shared_home,omitempty"`
}

// SpawnResult describes a newly spawned worker session and initial phase terminal.
type SpawnResult struct {
	StartedAt  time.Time `json:"started_at"`
	SessionID  string    `json:"session_id"`
	TerminalID string    `json:"terminal_id"`
	// BaseCommit is the immutable commit the backend actually started from.
	// Drivers should populate this from the created checkout/backend response.
	BaseCommit string `json:"base_commit,omitempty"`
}

// StartPhaseRequest starts a phase terminal on an existing or new worker session.
type StartPhaseRequest struct {
	RunDir    string            `json:"run_dir,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	RunID     string            `json:"run_id"`
	LaneID    string            `json:"lane_id"`
	SessionID string            `json:"session_id,omitempty"`
	Phase     string            `json:"phase"`
	Harness   string            `json:"harness"`
	Model     string            `json:"model"`
	Effort    string            `json:"effort,omitempty"`
	Brief     string            `json:"brief"`
	Round     int               `json:"round"`
}

// StartPhaseResult describes the started phase terminal.
type StartPhaseResult struct {
	StartedAt  time.Time `json:"started_at"`
	SessionID  string    `json:"session_id"`
	TerminalID string    `json:"terminal_id"`
}

// SteerRequest injects a steering note into a running lane terminal/session.
type SteerRequest struct {
	RunID      string `json:"run_id"`
	LaneID     string `json:"lane_id"`
	SessionID  string `json:"session_id,omitempty"`
	TerminalID string `json:"terminal_id,omitempty"`
	Message    string `json:"message"`
}

// StopRequest terminates a running lane session/terminal.
type StopRequest struct {
	RunID      string `json:"run_id"`
	LaneID     string `json:"lane_id"`
	SessionID  string `json:"session_id,omitempty"`
	TerminalID string `json:"terminal_id,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// EventKind identifies a uniform worker lifecycle signal.
type EventKind string

const (
	EventPhaseReported     EventKind = "phase_reported"
	EventTurnEndedNoReport EventKind = "turn_ended_no_report"
	EventWaitingForInput   EventKind = "waiting_for_input"
	EventExited            EventKind = "exited"
	EventQuotaFailure      EventKind = "quota_failure"
	EventAuthFailure       EventKind = "auth_failure"
	EventNoActivity        EventKind = "no_activity"
)

// Event is the backend-independent signal consumed by the controller.
type Event struct { //nolint:govet // wire-shaped event prioritizes readable fields
	Kind                                        EventKind
	RunID, LaneID, SessionID, TerminalID, Phase string
	Report                                      *PhaseReport
	Text                                        string
	Since                                       time.Time
	At                                          time.Time
}

// LaneDriver abstracts worker session lifecycle operations across backends.
type LaneDriver interface {
	// ReportInstructions returns the backend-specific contract a terminal must
	// follow before it considers a phase complete.
	ReportInstructions(runID, laneID, phase string, round int) string
	PhaseSkill(phase string) string
	SpawnSession(ctx context.Context, req SpawnRequest) (SpawnResult, error)
	StartPhaseTerminal(ctx context.Context, req StartPhaseRequest) (StartPhaseResult, error)
	Steer(ctx context.Context, req SteerRequest) error
	Stop(ctx context.Context, req StopRequest) error
	ReadReport(ctx context.Context, runID, laneID, terminalID string) (*PhaseReport, error)
	Watch(ctx context.Context, runID string) <-chan Event
}

type Driver = LaneDriver

// FakeDriver is an in-memory LaneDriver implementation for deterministic unit tests.
type FakeDriver struct { //nolint:govet // test double state is intentionally grouped
	reports         map[string]*PhaseReport
	Spawns          []SpawnRequest
	PhaseStarts     []StartPhaseRequest
	Steers          []SteerRequest
	Stops           []StopRequest
	SpawnBaseCommit string
	nextSeq         int
	subs            map[int]chan Event
	nextSub         int
	mu              sync.Mutex
}

func NewFakeDriver() *FakeDriver {
	return &FakeDriver{
		reports: make(map[string]*PhaseReport),
		subs:    make(map[int]chan Event),
	}
}

func (f *FakeDriver) ReportInstructions(_, laneID, phase string, round int) string {
	return fmt.Sprintf("File the phase report for lane %s phase %s round %d using the fake driver's report input; the phase is not finished until the report is filed. Report outcome done, needs-swe, blocked, or needs-input, with HEAD and PR lines.", laneID, phase, round)
}

func (f *FakeDriver) PhaseSkill(phase string) string { return phase }

func (f *FakeDriver) SpawnSession(_ context.Context, req SpawnRequest) (SpawnResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextSeq++
	cp := req
	if req.Env != nil {
		cp.Env = maps.Clone(req.Env)
	}
	f.Spawns = append(f.Spawns, cp)
	return SpawnResult{
		SessionID:  fmt.Sprintf("sess-%s-%d", req.LaneID, f.nextSeq),
		TerminalID: fmt.Sprintf("term-%s-%s-%d", req.LaneID, PhaseRoundKey(req.Phase, req.Round), f.nextSeq),
		StartedAt:  time.Now().UTC(),
		BaseCommit: f.SpawnBaseCommit,
	}, nil
}

func (f *FakeDriver) StartPhaseTerminal(_ context.Context, req StartPhaseRequest) (StartPhaseResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextSeq++
	cp := req
	if req.Env != nil {
		cp.Env = maps.Clone(req.Env)
	}
	f.PhaseStarts = append(f.PhaseStarts, cp)
	sessID := req.SessionID
	if sessID == "" {
		sessID = fmt.Sprintf("sess-%s-%d", req.LaneID, f.nextSeq)
	}
	return StartPhaseResult{
		SessionID:  sessID,
		TerminalID: fmt.Sprintf("term-%s-%s-%d", req.LaneID, PhaseRoundKey(req.Phase, req.Round), f.nextSeq),
		StartedAt:  time.Now().UTC(),
	}, nil
}

func (f *FakeDriver) Steer(_ context.Context, req SteerRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Steers = append(f.Steers, req)
	return nil
}

func (f *FakeDriver) Stop(_ context.Context, req StopRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Stops = append(f.Stops, req)
	return nil
}

// SetReport scripts a phase report to be returned by ReadReport for terminalID.
func (f *FakeDriver) SetReport(terminalID string, rep PhaseReport) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := rep
	f.reports[terminalID] = &cp
}

func (f *FakeDriver) ReadReport(_ context.Context, _, _, terminalID string) (*PhaseReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rep, ok := f.reports[terminalID]
	if !ok {
		return nil, fmt.Errorf("no phase report for terminal %q", terminalID)
	}
	cp := *rep
	return &cp, nil
}

// Emit publishes an event to watchers.
func (f *FakeDriver) Emit(ev Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (f *FakeDriver) Watch(ctx context.Context, runID string) <-chan Event {
	raw := make(chan Event, 64)
	out := make(chan Event, 16)
	f.mu.Lock()
	f.nextSub++
	id := f.nextSub
	f.subs[id] = raw
	f.mu.Unlock()
	go func() {
		defer func() { f.mu.Lock(); delete(f.subs, id); f.mu.Unlock(); close(out) }()
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-raw:
				if runID == "" || ev.RunID == "" || ev.RunID == runID {
					select {
					case out <- ev:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	return out
}

// AppendReportInstructions preserves the mandatory handback contract while
// fitting ordinary brief text into the remaining byte budget.
func AppendReportInstructions(text, instructions string, maxBytes int) string {
	separator := ""
	if text != "" && instructions != "" {
		separator = "\n\n"
	}
	if maxBytes > 0 {
		available := max(0, maxBytes-len(instructions)-len(separator))
		if len(text) > available {
			text = text[:available]
			for text != "" && !utf8.ValidString(text) {
				text = text[:len(text)-1]
			}
		}
	}
	if text == "" {
		return instructions
	}
	return text + separator + instructions
}
