// Package controller implements the deterministic swarm orchestrator state machine.
// Launch intents and completed effects are persisted separately for crash recovery.
package controller

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/mailbox"
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
)

// Default configuration values for Controller.
const (
	DefaultConsumer              = "controller"
	DefaultHolderID              = "controller-1"
	DefaultLeaseTTL              = 30 * time.Second
	DefaultHeartbeatInterval     = 10 * time.Second
	DefaultPollInterval          = 5 * time.Second
	DefaultJudgmentDebounce      = 2 * time.Minute
	DefaultStallTimeout          = 45 * time.Minute
	DefaultUnpushedWorkThreshold = 45 * time.Minute
	DefaultDiskPressurePercent   = 90
	ActorController              = "controller"
)

// TerminalStateLive reports whether a host terminal may still own work.
// Unknown states remain live until an explicit exit is observed.
func TerminalStateLive(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "exited", "stopped", "released", "ended", "done", "completed", "failed", "finished", "idle", "sleeping":
		return false
	default:
		return true
	}
}

// Tool names recorded in store.DecisionStore by the deterministic controller.
const (
	ToolPhaseStart       = "phase.start"
	ToolPhaseAdvance     = "phase.advance"
	ToolPhaseRework      = "phase.rework"
	ToolPhaseRetry       = "phase.retry"
	ToolLaneComplete     = "lane.complete"
	ToolLaneEscalate     = "lane.escalate"
	ToolLaneStall        = "lane.stall"
	ToolJudgmentDispatch = "judgment.dispatch"
	ToolOwnerAskDefault  = "owner.ask.default"
)

const (
	ownerAskRefPrefix = "owner_ask:"
	// NamedWaitRefPrefix is the directive Ref prefix for lane/run expected waits.
	NamedWaitRefPrefix           = "named-wait:"
	integrateRecoveryRefPrefix   = "integrate-recovery:"
	integrateRecoveryReadyPrefix = "integrate recovery ready:"
	integrateRecoveryMaxAttempts = 3
	integrateRecoveryBackoff     = time.Second
	eventFailureSource           = "controller.event.failure"
)

// ErrControllerStopped is returned when Step or Run is called on a killed controller.
var ErrControllerStopped = errors.New("orca/controller: controller stopped")

// LaneLocalError marks a driver failure scoped to one lane.
type LaneLocalError struct{ Err error }

func (e *LaneLocalError) Error() string { return e.Err.Error() }
func (e *LaneLocalError) Unwrap() error { return e.Err }

// PermanentError marks an event failure that can be durably skipped.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

func isIsolatedFailure(err error) bool {
	// Control and infrastructure errors take precedence even inside a wrapper.
	if errors.Is(err, store.ErrLeaseHeld) || errors.Is(err, store.ErrLeaseNotHeld) ||
		errors.Is(err, ErrControllerStopped) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var local *LaneLocalError
	var permanent *PermanentError
	return errors.As(err, &local) || errors.As(err, &permanent)
}

// laneDriverError classifies a driver failure as scoped to the current lane.
// Control and infrastructure errors remain unwrapped so Run can stop or retry
// the controller instead of treating them as a lane outcome.
func laneDriverError(err error) error {
	if err == nil || errors.Is(err, store.ErrLeaseHeld) || errors.Is(err, store.ErrLeaseNotHeld) ||
		errors.Is(err, ErrControllerStopped) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &LaneLocalError{Err: err}
}

// IsUnrecoverable reports whether an error means this controller can no longer
// make progress. Transport and provider failures are intentionally retryable;
// only lease ownership loss, a released run, cancellation, or an explicit
// controller stop terminate the loop.
func IsUnrecoverable(err error) bool {
	return err != nil && (errors.Is(err, store.ErrLeaseHeld) ||
		errors.Is(err, store.ErrLeaseNotHeld) || errors.Is(err, store.ErrNotFound) ||
		errors.Is(err, ErrControllerStopped) || errors.Is(err, context.Canceled))
}

// IsTransient reports provider/network failures that should be retried by an
// adopted controller instead of failing adoption itself.
func IsTransient(err error) bool {
	if err == nil || IsUnrecoverable(err) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, token := range []string{"timeout", "deadline exceeded", "connection reset", "connection refused", "temporarily unavailable", "temporary failure", "temporary", "service unavailable"} {
		if strings.Contains(s, token) {
			return true
		}
	}
	for _, token := range strings.FieldsFunc(s, func(r rune) bool {
		return r < '0' || r > '9'
	}) {
		if len(token) == 3 && token[0] == '5' {
			return true
		}
	}
	return false
}

// Judge handles a debounced batch of orchestration events that require LLM judgment.
type Judge interface {
	Judge(ctx context.Context, runID string, events []store.Event) error
}

// JudgeFunc adapts a function to the Judge interface.
type JudgeFunc func(ctx context.Context, runID string, events []store.Event) error

func (f JudgeFunc) Judge(ctx context.Context, runID string, events []store.Event) error {
	return f(ctx, runID, events)
}

// FakeJudge is a thread-safe in-memory Judge implementation for deterministic tests.
type FakeJudge struct {
	Err     error
	OnJudge func(ctx context.Context, runID string, events []store.Event) error
	Batches [][]store.Event
	mu      sync.Mutex
}

// NewFakeJudge creates a new FakeJudge.
func NewFakeJudge() *FakeJudge {
	return &FakeJudge{}
}

func (f *FakeJudge) Judge(ctx context.Context, runID string, events []store.Event) error {
	f.mu.Lock()
	cp := make([]store.Event, len(events))
	for i := range events {
		cp[i] = events[i].Clone()
	}
	f.Batches = append(f.Batches, cp)
	fn := f.OnJudge
	err := f.Err
	f.mu.Unlock()

	if fn != nil {
		if cbErr := fn(ctx, runID, cp); cbErr != nil {
			return cbErr
		}
	}
	return err
}

func (f *FakeJudge) BatchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Batches)
}

func (f *FakeJudge) AllEvents() []store.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Event
	for _, b := range f.Batches {
		for i := range b {
			out = append(out, b[i].Clone())
		}
	}
	return out
}

// PRStatus reports the live state of a lane's pull request on GitHub.
type PRStatus struct {
	State    string
	MergeSHA string
	Number   int
}

// PRProbe queries the live state of a lane's pull request on GitHub for runID.
type PRProbe func(ctx context.Context, runID, branch string, pr int) (PRStatus, error)

// Config configures a Controller instance for a single Run.
//
//nolint:govet // config fields are grouped by lifecycle concern.
type Config struct {
	// SkipLease disables controller lease acquisition and heartbeat. It is used
	// by shadow runs, which operate on an isolated recording store and must not
	// contend with the live run's lease.
	SkipLease bool
	// BeforeStep runs immediately before each Run loop step. Returning false
	// keeps the lease and watches alive while skipping lifecycle work.
	BeforeStep func(context.Context) (bool, error)
	// HostRefresh re-imports host-owned state immediately before each step of
	// an adopted run. It is deliberately read-only from the controller's point
	// of view so host phase/status changes win over stale local state.
	HostRefresh func(context.Context) error
	// StepError receives lane-local failures that were recorded without
	// aborting the rest of the controller pass.
	StepError func(error)
	// IsolateLaneErrors keeps provider failures local to a lane/event. Adopted
	// controllers enable this so one host lane cannot freeze the run.
	IsolateLaneErrors bool
	// AnswerQuestion delegates interpretation of task text and notes to the
	// dispatcher. An empty answer requests owner escalation.
	AnswerQuestion func(context.Context, string, string) (string, error)
	// PRProbe optionally queries a lane's PR state on GitHub before retry,
	// advance, or spawn.
	PRProbe           PRProbe
	RunID             string
	HolderID          string
	Consumer          string
	LeaseTTL          time.Duration
	HeartbeatInterval time.Duration
	PollInterval      time.Duration
	JudgmentDebounce  time.Duration
	StallTimeout      time.Duration
	// StallTimeoutSet distinguishes an explicit timeout from the default.
	StallTimeoutSet       bool
	ConcurrencyCap        int
	SubscriptionActors    []string
	MaxCostPerHourUSD     float64
	UnpushedWorkThreshold time.Duration
	DiskPressurePercent   int
	// HostRun marks a run adopted from a host orchestrator. Its CLI dispatcher
	// and the skill's watcher own every lifecycle judgment; the controller only
	// keeps the lease, heartbeat and host refresh.
	HostRun bool
}

func (c Config) withDefaults() Config {
	c.HolderID = cmp.Or(c.HolderID, DefaultHolderID)
	c.Consumer = cmp.Or(c.Consumer, DefaultConsumer)
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = DefaultLeaseTTL
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = min(DefaultHeartbeatInterval, max(c.LeaseTTL/3, time.Millisecond))
	}
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.JudgmentDebounce == 0 {
		c.JudgmentDebounce = DefaultJudgmentDebounce
	}
	if c.StallTimeout <= 0 {
		c.StallTimeout = DefaultStallTimeout
	}
	if c.UnpushedWorkThreshold <= 0 {
		c.UnpushedWorkThreshold = DefaultUnpushedWorkThreshold
	}
	if c.DiskPressurePercent <= 0 {
		c.DiskPressurePercent = DefaultDiskPressurePercent
	}
	return c
}

// OwnerAsk represents a decision question posed to the owner with a mandatory
// default option and deadline ("decide, don't ask").
type OwnerAsk struct {
	Deadline        time.Time `json:"deadline"`
	ID              string    `json:"id"`
	RunID           string    `json:"run_id,omitempty"`
	LaneID          string    `json:"lane_id,omitempty"`
	Question        string    `json:"question"`
	Default         string    `json:"default"`
	SteerNote       string    `json:"steer_note,omitempty"`
	SourceEventID   string    `json:"source_event_id,omitempty"`
	SourceMessageID string    `json:"source_message_id,omitempty"`
	Options         []string  `json:"options"`
}

// Validate checks that the OwnerAsk satisfies the "decide, don't ask" policy.
func (a *OwnerAsk) Validate() error {
	if strings.TrimSpace(a.ID) == "" {
		return fmt.Errorf("%w: owner ask id is required", store.ErrInvalidArgument)
	}
	if strings.TrimSpace(a.Question) == "" {
		return fmt.Errorf("%w: owner ask %q requires question", store.ErrInvalidArgument, a.ID)
	}
	if len(a.Options) == 0 {
		return fmt.Errorf("%w: owner ask %q requires options", store.ErrInvalidArgument, a.ID)
	}
	if strings.TrimSpace(a.Default) == "" {
		return fmt.Errorf("%w: owner ask %q requires default option", store.ErrInvalidArgument, a.ID)
	}
	if !slices.Contains(a.Options, a.Default) {
		return fmt.Errorf("%w: owner ask %q default %q must be one of options %v", store.ErrInvalidArgument, a.ID, a.Default, a.Options)
	}
	if a.Deadline.IsZero() {
		return fmt.Errorf("%w: owner ask %q requires deadline", store.ErrInvalidArgument, a.ID)
	}
	return nil
}

// CIPayload is the JSON payload for store.EventCI events.
type CIPayload struct {
	Green   *bool  `json:"green,omitempty"`
	HeadSHA string `json:"head_sha,omitempty"`
	Status  string `json:"status,omitempty"`
	// CheckName identifies an individual required check when a host reports
	// checks incrementally.  Aggregate statuses remain supported for native
	// runs; adopted merge gates require the named checks below.
	CheckName   string `json:"check_name,omitempty"`
	FailedCheck string `json:"failed_check,omitempty"`
	PR          int    `json:"pr,omitempty"`
}

// PhaseExitPayload is the JSON payload for store.EventPhaseExit events.
type PhaseExitPayload struct {
	Kind       lane.EventKind `json:"kind,omitempty"`
	TerminalID string         `json:"terminal_id,omitempty"`
	Phase      string         `json:"phase,omitempty"`
	Error      string         `json:"error,omitempty"`
	Round      int            `json:"round,omitempty"`
	ExitCode   int            `json:"exit_code,omitempty"`
}

// LaneReportPayload is the JSON payload for store.EventLaneReport events.
type LaneReportPayload struct {
	CIGreen *bool  `json:"ci_green,omitempty"`
	Head    string `json:"head,omitempty"`
	Checks  string `json:"checks,omitempty"`
	lane.PhaseReport
}

// DecisionArgs is the structured JSON payload stored in Decision.Args for
// controller actions and idempotency checks on restart.
type DecisionArgs struct {
	ActionKey        string   `json:"action_key"`
	TriggerEventID   string   `json:"trigger_event_id,omitempty"`
	EventID          string   `json:"event_id,omitempty"`
	FromPhase        string   `json:"from_phase,omitempty"`
	ToPhase          string   `json:"to_phase,omitempty"`
	Phase            string   `json:"phase,omitempty"`
	Verdict          string   `json:"verdict,omitempty"`
	Harness          string   `json:"harness,omitempty"`
	Model            string   `json:"model,omitempty"`
	Effort           string   `json:"effort,omitempty"`
	TerminalID       string   `json:"terminal_id,omitempty"`
	SessionID        string   `json:"session_id,omitempty"`
	AskID            string   `json:"ask_id,omitempty"`
	Default          string   `json:"default,omitempty"`
	EventIDs         []string `json:"event_ids,omitempty"`
	StoppedTerminals []string `json:"stopped_terminals,omitempty"`
	Round            int      `json:"round,omitempty"`
	Attempt          int      `json:"attempt,omitempty"`
}

type pendingVerdict struct {
	verdict string
	headSHA string
	key     string
}

type terminalStateCacheEntry struct {
	err   error
	state string
}

// Controller is the deterministic orchestration loop for a single Run.
//
//nolint:govet // state is grouped by synchronization and lifecycle concerns.
type Controller struct {
	batchFirstAt       time.Time
	store              store.Store
	driver             lane.LaneDriver
	clock              clock.Clock
	judge              Judge
	mailbox            mailbox.Mailbox
	decidedActions     map[string]store.Decision
	decidedEvents      map[string]store.Decision
	dispatchedEvents   map[string]store.Decision
	pendingVerdicts    map[string]pendingVerdict
	emittedStalls      map[string]bool
	emittedQuiet       map[string]string
	emittedAskExpiries map[string]bool
	stoppedTerminals   map[string]bool
	unpushedSince      map[string]time.Time
	releaseRetries     map[string]int
	pendingStops       map[string]int
	staleStopFailures  map[string]int
	sessionHealthCache map[string]lane.SessionHealth
	terminalStateCache map[string]terminalStateCacheEntry
	watchErrors        chan error
	wakeCh             chan struct{}
	cancelRun          context.CancelFunc
	cancelWatches      context.CancelFunc
	pendingJudgment    []store.Event
	cfg                Config
	lastDecisionSeq    int64
	mu                 sync.Mutex
	quietMu            sync.Mutex
	cacheMu            sync.Mutex
	leaseHeld          bool
	initialized        bool
	killed             bool
	watchStarted       bool
	stallTimeoutSet    bool
	runtimeSettingsSet bool
	runtimePaused      bool
	stepError          error
	laneFailures       map[string]time.Time
	laneFailureCounts  map[string]int
	eventRetries       map[string]store.Event
}

// SetRuntimeSettings applies live settings for subsequent controller ticks.
func (c *Controller) SetRuntimeSettings(settings store.RunSettings) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if settings.StallTimeout > 0 {
		c.cacheMu.Lock()
		c.cfg.StallTimeout = settings.StallTimeout
		c.stallTimeoutSet = true
		c.cacheMu.Unlock()
	}
	if settings.CostGuardDisabled {
		c.cfg.MaxCostPerHourUSD = 0
	} else if settings.MaxCostPerHourUSD > 0 {
		c.cfg.MaxCostPerHourUSD = settings.MaxCostPerHourUSD
	}
	c.runtimePaused = settings.Paused
	if settings.StallTimeout <= 0 && settings.ConcurrencyCap == 0 && settings.MaxCostPerHourUSD <= 0 && !settings.CostGuardDisabled && !settings.Paused {
		return
	}
	if settings.ConcurrencyCap >= 0 {
		c.cfg.ConcurrencyCap = settings.ConcurrencyCap
	}
	c.runtimeSettingsSet = true
}

// NotifyOwnerCostGuard sends the cost guard question with a stable idempotency
// key. Callers should invoke it only when a new breach transitions the run to
// paused.
func (c *Controller) NotifyOwnerCostGuard(ctx context.Context, msg string) {
	c.notifyOwnerCostGuard(ctx, msg)
}

// LastStepError returns the latest lane-local error recorded during a pass.
func (c *Controller) LastStepError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stepError
}

// New constructs a new deterministic Controller for cfg.RunID.
func New(cfg Config, st store.Store, drv lane.LaneDriver, clk clock.Clock, judge Judge, boxes ...mailbox.Mailbox) *Controller {
	if clk == nil {
		clk = clock.NewRealClock()
	}
	stallTimeoutSet := cfg.StallTimeoutSet
	c := &Controller{
		cfg:                cfg.withDefaults(),
		store:              st,
		driver:             drv,
		clock:              clk,
		judge:              judge,
		decidedActions:     make(map[string]store.Decision),
		decidedEvents:      make(map[string]store.Decision),
		dispatchedEvents:   make(map[string]store.Decision),
		pendingVerdicts:    make(map[string]pendingVerdict),
		emittedStalls:      make(map[string]bool),
		emittedQuiet:       make(map[string]string),
		emittedAskExpiries: make(map[string]bool),
		stoppedTerminals:   make(map[string]bool),
		unpushedSince:      make(map[string]time.Time),
		releaseRetries:     make(map[string]int),
		pendingStops:       make(map[string]int),
		laneFailures:       make(map[string]time.Time),
		laneFailureCounts:  make(map[string]int),
		eventRetries:       make(map[string]store.Event),
		staleStopFailures:  make(map[string]int),
		wakeCh:             make(chan struct{}, 1),
		watchErrors:        make(chan error, 16),
		stallTimeoutSet:    stallTimeoutSet,
	}
	if len(boxes) > 0 {
		c.mailbox = boxes[0]
	}
	return c
}

// StartWatches connects worker and mailbox streams to the controller.
func (c *Controller) StartWatches(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.killed {
		return ErrControllerStopped
	}
	if c.watchStarted {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	c.cancelWatches = cancel
	if c.mailbox != nil {
		ch, err := c.mailbox.Watch(ctx)
		// Connection failures retry in the forwarder, so lane Watch can start
		// even before the mailbox becomes available. Cancellation stops retries.
		if err != nil {
			c.reportWatchError(err)
			ch = nil
		}
		go func() {
			for {
				if ch == nil {
					if !c.watchRetry(ctx) {
						return
					}
					var err error
					ch, err = c.mailbox.Watch(ctx)
					if err != nil {
						c.reportWatchError(err)
						ch = nil
						continue
					}
				}
				select {
				case <-ctx.Done():
					return
				case _, ok := <-ch:
					if !ok {
						c.reportWatchError(errors.New("mailbox Watch closed; reconnecting"))
						ch = nil
						continue
					}
					c.Wake()
				}
			}
		}()
	}
	if c.driver != nil {
		ch := c.driver.Watch(ctx, c.cfg.RunID)
		go func() {
			for {
				c.forwardLaneEvents(ctx, ch)
				if ctx.Err() != nil {
					return
				}
				c.reportWatchError(errors.New("lane Watch closed; reconnecting"))
				if !c.watchRetry(ctx) {
					return
				}
				ch = c.driver.Watch(ctx, c.cfg.RunID)
			}
		}()
	}
	c.watchStarted = true
	go func() { <-ctx.Done(); c.mu.Lock(); c.watchStarted = false; c.mu.Unlock() }()
	return nil
}

func (c *Controller) forwardLaneEvents(ctx context.Context, ch <-chan lane.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok || !c.forwardLaneEvent(ctx, ev) {
				return
			}
		}
	}
}

func (c *Controller) forwardLaneEvent(ctx context.Context, ev lane.Event) bool {
	if ev.Kind == lane.EventWaitingForInput && lane.WorkingInputWaiting(ev.Text) {
		// agy can report input.waiting while its own timer/background task is
		// active. That is a working heartbeat, never a phase exit or stop cue.
		return true
	}
	// Drivers may report run-wide silence. Apply it only to lanes that were
	// already quiet at Since, retaining each terminal's identity for stale checks.
	if ev.Kind == lane.EventNoActivity && ev.LaneID == "" {
		var lanes []lane.Lane
		for {
			var err error
			lanes, err = c.store.Lanes().List(ctx, store.LaneFilter{RunID: c.cfg.RunID, Statuses: []lane.Status{lane.StatusRunning}})
			if err == nil {
				break
			}
			c.reportWatchError(err)
			if !c.watchRetry(ctx) {
				return false
			}
		}
		for i := range lanes {
			ln := &lanes[i]
			if !ev.Since.IsZero() && ln.LastEventAt.After(ev.Since) {
				continue
			}
			child := ev
			child.LaneID, child.TerminalID, child.Phase = ln.ID, ln.TerminalID, ln.Phase
			if !c.forwardLaneEvent(ctx, child) {
				return false
			}
		}
		return true
	}
	if ev.Kind == lane.EventNoActivity && ev.LaneID != "" {
		if ln, err := c.store.Lanes().Get(ctx, c.cfg.RunID, ev.LaneID); err == nil && ln != nil {
			if ln.Status != lane.StatusRunning {
				return true
			}
			if !ln.AwaitingInputSince.IsZero() {
				return true
			}
			now := c.clock.Now().UTC()
			if c.laneIsWorkingOrBusyUncached(ctx, *ln) || c.hasActiveExpectedWait(ctx, ln.ID, now) {
				return true
			}
			r, _ := c.resolveRecipe(ctx, ln)
			rung, waiting := c.quietLadderRungFromStore(ctx, ln, r, now)
			quietKey := fmt.Sprintf("%s:%d:%d:%s:%d:%d:%t:%d", ln.Phase, max(ln.Round, 1), max(ln.Attempt, 1), ln.TerminalID, ln.LastEventAt.UTC().UnixNano(), rung, waiting, ln.AnswerEpoch)
			c.quietMu.Lock()
			already := c.emittedQuiet[ln.ID] == quietKey
			if !already {
				c.emittedQuiet[ln.ID] = quietKey
			}
			c.quietMu.Unlock()
			if already {
				return true
			}
		}
	}
	kind := store.EventPhaseExit
	var payload any
	switch ev.Kind {
	case lane.EventPhaseReported:
		kind, payload = store.EventLaneReport, ev.Report
	case lane.EventTurnEndedNoReport, lane.EventWaitingForInput, lane.EventExited, lane.EventQuotaFailure, lane.EventAuthFailure, lane.EventNoActivity:
		payload = PhaseExitPayload{Kind: ev.Kind, TerminalID: ev.TerminalID, Phase: ev.Phase, Error: ev.Text}
	default:
		return true
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		c.reportWatchError(err)
		return true
	}
	// Producer timestamps and terminal identity must be retained on replay.
	// A stable event ID prevents a replayed pulse from consuming another rung.
	// For phase reports, strip wall-clock fallback timestamps so re-polling on
	// restart hashes to the same event ID and does not duplicate lane_report rows.
	idEv := ev
	if ev.Kind == lane.EventPhaseReported {
		idEv.At = time.Time{}
		if ev.Report != nil {
			repCopy := *ev.Report
			repCopy.ReportedAt = time.Time{}
			idEv.Report = &repCopy
		}
		if c.hasExistingLaneReport(ctx, ev) {
			return true
		}
	}
	identity, _ := json.Marshal(idEv)
	eventID := fmt.Sprintf("lane-watch-%x", sha256.Sum256(identity))
	summary := ev.Text
	urgent := ev.Kind == lane.EventPhaseReported && ev.Report != nil && (ev.Report.Outcome == lane.OutcomeNeedsInput || ev.Report.Outcome == lane.OutcomeBlocked)
	if ev.Kind == lane.EventWaitingForInput {
		tail := waitingInputTail(ev.Text)
		eventID = waitingInputEventID(ev.LaneID, ev.TerminalID, tail)
		summary = waitingInputSummary(ev.LaneID, tail)
		urgent = true
	}
	event := store.Event{ID: eventID, RunID: c.cfg.RunID, LaneID: ev.LaneID, Kind: kind, Source: "lane.watch", Payload: raw, Summary: summary, Urgent: urgent}
	for {
		if _, err := c.store.Events().Append(ctx, event); err == nil || errors.Is(err, store.ErrAlreadyExists) {
			break
		} else {
			c.reportWatchError(fmt.Errorf("persist lane Watch event: %w", err))
		}
		if !c.watchRetry(ctx) {
			return false
		}
	}
	c.Wake()
	return true
}

func (c *Controller) hasExistingLaneReport(ctx context.Context, ev lane.Event) bool {
	if ev.Report == nil || ev.LaneID == "" {
		return false
	}
	if ev.Report.Outcome == lane.OutcomeNeedsInput || ev.Report.Outcome == lane.OutcomeBlocked {
		if ln, err := c.store.Lanes().Get(ctx, c.cfg.RunID, ev.LaneID); err == nil && ln.DuplicateQuestion(ev.Report.QuestionText(), c.clock.Now()) {
			return true
		}
	}
	evs, err := c.store.Events().List(ctx, store.EventFilter{
		RunID:  c.cfg.RunID,
		LaneID: ev.LaneID,
		Kinds:  []store.EventKind{store.EventLaneReport},
	})
	if err != nil {
		return false
	}
	wantTerm := strings.TrimSpace(cmp.Or(ev.TerminalID, ev.Report.TerminalID))
	for i := range evs {
		existing := &evs[i]
		if existing.Source != "lane.watch" {
			continue
		}
		var rep lane.PhaseReport
		if len(existing.Payload) == 0 || json.Unmarshal(existing.Payload, &rep) != nil {
			continue
		}
		gotTerm := strings.TrimSpace(rep.TerminalID)
		if wantTerm != "" && gotTerm != "" && wantTerm == gotTerm &&
			rep.Outcome == ev.Report.Outcome &&
			rep.Phase == ev.Report.Phase &&
			rep.Round == ev.Report.Round &&
			rep.HeadSHA == ev.Report.HeadSHA &&
			strings.TrimSpace(rep.Summary) == strings.TrimSpace(ev.Report.Summary) &&
			strings.TrimSpace(rep.Raw) == strings.TrimSpace(ev.Report.Raw) {
			return true
		}
	}
	return false
}

var ansiEscapePattern = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[ -/]*[@-~])`) //nolint:gocritic // ECMA-48 defines space-to-slash as the intermediate-byte range.

const waitingInputTailBytes = 1536

func waitingInputTail(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = ansiEscapePattern.ReplaceAllString(text, "")
	text = strings.Trim(text, " \t\r\n")
	if text == "" {
		return "(no screen text)"
	}
	if len(text) <= waitingInputTailBytes {
		return text
	}
	text = text[len(text)-waitingInputTailBytes:]
	for text != "" && !utf8.ValidString(text) {
		text = text[1:]
	}
	return text
}

func waitingInputSummary(laneID, tail string) string {
	return fmt.Sprintf("lane %s waiting for input: %s", laneID, tail)
}

func waitingInputEventID(laneID, terminalID, tail string) string {
	identity := laneID + "\x00" + terminalID + "\x00" + tail
	return fmt.Sprintf("lane-waiting-%x", sha256.Sum256([]byte(identity)))
}

// WatchErrors exposes stream and persistence failures while the forwarder retries.
func (c *Controller) WatchErrors() <-chan error { return c.watchErrors }

func (c *Controller) reportWatchError(err error) {
	slog.Error("orca Watch failed; retrying", "error", err)
	select {
	case c.watchErrors <- err:
	default:
	}
}
func (c *Controller) watchRetry(ctx context.Context) bool {
	timer := c.clock.NewTimer(c.cfg.PollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C():
		return true
	}
}

// Wake signals a running Controller loop to execute a Step immediately.
func (c *Controller) Wake() {
	select {
	case c.wakeCh <- struct{}{}:
	default:
	}
}

// Kill abruptly stops the Controller without releasing its run lease in the
// Store, simulating a process crash. Another Controller instance can take over
// once the lease TTL expires.
func (c *Controller) Kill() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.killed = true
	if c.cancelWatches != nil {
		c.cancelWatches()
	}
	if c.cancelRun != nil {
		c.cancelRun()
	}
}

// ReleaseLease explicitly releases the run lease held by this Controller.
func (c *Controller) ReleaseLease(ctx context.Context) error {
	if c.cfg.SkipLease {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.store.Runs().ReleaseLease(ctx, c.cfg.RunID, c.cfg.HolderID)
	if err == nil {
		c.leaseHeld = false
	}
	return err
}

// Run starts the controller event loop until ctx is cancelled or Kill is called.
func (c *Controller) Run(ctx context.Context) error {
	c.mu.Lock()
	if c.killed {
		c.mu.Unlock()
		return ErrControllerStopped
	}
	runCtx, cancel := context.WithCancel(ctx)
	c.cancelRun = cancel
	c.mu.Unlock()
	defer cancel()
	if err := c.StartWatches(runCtx); err != nil {
		return err
	}
	// Acquire before starting the independent heartbeat. Step also renews the
	// lease, but a slow provider call must never block heartbeat progress.
	for {
		if c.cfg.SkipLease {
			break
		}
		c.mu.Lock()
		err := c.ensureLease(runCtx)
		c.mu.Unlock()
		if err == nil {
			break
		}
		if errors.Is(err, store.ErrLeaseHeld) || IsUnrecoverable(err) {
			return err
		}
		slog.Warn("orca controller lease acquisition failed; retrying", "run", c.cfg.RunID, "error", err)
		timer := c.clock.NewTimer(c.cfg.PollInterval)
		select {
		case <-runCtx.Done():
			timer.Stop()
			return nil
		case <-timer.C():
		}
	}
	heartbeatErr := make(chan error, 1)
	heartbeatDone := make(chan struct{})
	if !c.cfg.SkipLease {
		go c.runHeartbeat(runCtx, heartbeatErr, heartbeatDone)
	}
	defer close(heartbeatDone)

	if err := c.Step(runCtx); err != nil {
		if IsUnrecoverable(err) && !errors.Is(err, context.Canceled) {
			return err
		}
		slog.Warn("orca controller step failed; retrying", "run", c.cfg.RunID, "error", err)
	}

	pollTicker := c.clock.NewTicker(c.cfg.PollInterval)
	defer pollTicker.Stop()

	retryDelay := c.cfg.PollInterval

	for {
		select {
		case <-runCtx.Done():
			return nil
		case err := <-heartbeatErr:
			return err
		case <-pollTicker.C():
		case <-c.wakeCh:
		case <-c.watchErrors:
		}
		if err := c.Step(runCtx); err != nil {
			if IsUnrecoverable(err) {
				if errors.Is(err, context.Canceled) && runCtx.Err() != nil {
					return nil
				}
				return err
			}
			slog.Warn("orca controller step failed; retrying", "run", c.cfg.RunID, "error", err)
			timer := c.clock.NewTimer(retryDelay)
		retryWait:
			for {
				select {
				case <-runCtx.Done():
					timer.Stop()
					return nil
				case hbErr := <-heartbeatErr:
					timer.Stop()
					return hbErr
				case <-c.watchErrors:
					// The forwarder already logged the error and is retrying. Keep
					// waiting for the step backoff so watch flapping cannot defeat it.
					continue
				case <-timer.C():
					break retryWait
				}
			}
			retryDelay = min(retryDelay*2, 1*time.Minute)
			continue
		}
		retryDelay = c.cfg.PollInterval
	}
}

func (c *Controller) runHeartbeat(ctx context.Context, errs chan<- error, done <-chan struct{}) {
	ticker := c.clock.NewTicker(c.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C():
			_, err := c.store.Runs().HeartbeatLease(ctx, c.cfg.RunID, c.cfg.HolderID, c.cfg.LeaseTTL)
			if err == nil {
				continue
			}
			if errors.Is(err, store.ErrLeaseNotHeld) {
				run, getErr := c.store.Runs().Get(ctx, c.cfg.RunID)
				if getErr == nil && run != nil && run.Lease != nil && run.Lease.HolderID == c.cfg.HolderID && run.Lease.Expired(c.clock.Now()) {
					if _, reacquireErr := c.store.Runs().AcquireLease(ctx, c.cfg.RunID, c.cfg.HolderID, c.cfg.LeaseTTL); reacquireErr == nil {
						continue
					} else {
						err = reacquireErr
					}
				}
			}
			if errors.Is(err, store.ErrLeaseHeld) || errors.Is(err, store.ErrLeaseNotHeld) || errors.Is(err, store.ErrNotFound) {
				select {
				case errs <- err:
				default:
				}
				return
			}
			slog.Warn("orca controller heartbeat failed; retrying", "run", c.cfg.RunID, "error", err)
		}
	}
}

func (c *Controller) resetStepCaches(init bool) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if init {
		c.sessionHealthCache = make(map[string]lane.SessionHealth)
		c.terminalStateCache = make(map[string]terminalStateCacheEntry)
	} else {
		c.sessionHealthCache = nil
		c.terminalStateCache = nil
	}
}

// Step executes one deterministic pass of lease maintenance, stall/expiry detection, event consumption, and judgment flushing.
func (c *Controller) Step(ctx context.Context) error {
	if c.cfg.BeforeStep != nil {
		proceed, err := c.cfg.BeforeStep(ctx)
		if err != nil {
			return err
		}
		if !proceed {
			return nil
		}
	}
	if c.cfg.HostRefresh != nil {
		if err := c.cfg.HostRefresh(ctx); err != nil {
			return err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetStepCaches(true)
	defer c.resetStepCaches(false)

	if c.killed {
		return ErrControllerStopped
	}
	c.stepError = nil

	if err := c.ensureLease(ctx); err != nil {
		return err
	}
	if c.runtimePaused || c.cfg.HostRun {
		return nil
	}

	if err := c.syncDecisions(ctx); err != nil {
		return err
	}
	c.retryReleases(ctx)
	c.retryStops(ctx)

	if !c.initialized {
		if err := c.recoverPendingState(ctx); err != nil {
			return err
		}
		c.initialized = true
	}

	if err := c.recoverRecordedLaunches(ctx); err != nil {
		return err
	}
	if err := c.reconcilePlannedLanes(ctx); err != nil {
		return err
	}

	if err := c.checkLaneSessions(ctx); err != nil {
		return err
	}

	if err := c.detectStalls(ctx); err != nil {
		return err
	}

	if err := c.checkNamedWaits(ctx); err != nil {
		return err
	}
	if err := c.checkIntegrateRecovery(ctx); err != nil {
		return err
	}
	if err := c.checkOwnerAsks(ctx); err != nil {
		return err
	}

	if err := c.retryFailedEvents(ctx); err != nil {
		return err
	}
	if err := c.consumeEvents(ctx); err != nil {
		return err
	}
	return c.flushJudgmentIfDue(ctx)
}

// Wait registers a durable named condition. Its producer calls CompleteWait;
// an unresolved deadline emits a judgment event.
func (c *Controller) Wait(ctx context.Context, name, laneID string, deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(name) == "" || deadline.IsZero() {
		return fmt.Errorf("%w: wait requires name and deadline", store.ErrInvalidArgument)
	}
	if err := c.ensureLease(ctx); err != nil {
		return err
	}
	return c.store.Directives().Add(ctx, store.Directive{ID: "wait-" + name, RunID: c.cfg.RunID, Kind: store.DirectiveInstruction, Source: store.DirectiveSourceDispatcher, Scope: laneID, Ref: NamedWaitRefPrefix + name, Text: name, ExpiresAt: &deadline, Status: store.DirectiveActive})
}

func (c *Controller) CompleteWait(ctx context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureLease(ctx); err != nil {
		return err
	}
	return c.store.Directives().UpdateStatus(ctx, c.cfg.RunID, "wait-"+name, store.DirectiveHandled)
}

func (c *Controller) checkNamedWaits(ctx context.Context) error {
	dirs, err := c.store.Directives().List(ctx, store.DirectiveFilter{RunID: c.cfg.RunID, ActiveOnly: false})
	if err != nil {
		return err
	}
	for i := range dirs {
		dir := &dirs[i]
		if dir.Status != store.DirectiveActive || !strings.HasPrefix(dir.Ref, NamedWaitRefPrefix) || dir.ExpiresAt == nil || c.clock.Now().Before(*dir.ExpiresAt) {
			continue
		}
		if dir.Scope != "" {
			ln, err := c.store.Lanes().Get(ctx, c.cfg.RunID, dir.Scope)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if err == nil && ln.Status.Terminal() {
				if err := c.store.Directives().UpdateStatus(ctx, c.cfg.RunID, dir.ID, store.DirectiveHandled); err != nil {
					return err
				}
				continue
			}
		}
		_, err := c.store.Events().Append(ctx, store.Event{ID: "expired-" + dir.ID, RunID: c.cfg.RunID, LaneID: dir.Scope, Kind: store.EventTimer, Class: store.EventClassJudgment, Urgent: true, Summary: "wait deadline: " + dir.Text})
		if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			return err
		}
		if err := c.store.Directives().UpdateStatus(ctx, c.cfg.RunID, dir.ID, store.DirectiveHandled); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) checkIntegrateRecovery(ctx context.Context) error {
	dirs, err := c.store.Directives().List(ctx, store.DirectiveFilter{RunID: c.cfg.RunID, ActiveOnly: false})
	if err != nil {
		return err
	}
	now := c.clock.Now().UTC()
	for i := range dirs {
		dir := &dirs[i]
		if dir.Status != store.DirectiveActive || !strings.HasPrefix(dir.Ref, integrateRecoveryRefPrefix) || dir.ExpiresAt == nil || now.Before(*dir.ExpiresAt) {
			continue
		}
		var p struct {
			LaneID    string `json:"lane_id"`
			ActionKey string `json:"action_key"`
		}
		if json.Unmarshal([]byte(dir.Text), &p) != nil || strings.TrimSpace(p.LaneID) == "" {
			if err := c.store.Directives().UpdateStatus(ctx, c.cfg.RunID, dir.ID, store.DirectiveHandled); err != nil {
				return err
			}
			continue
		}
		payload, err := json.Marshal(p)
		if err != nil {
			return err
		}
		_, err = c.store.Events().Append(ctx, store.Event{ID: "expired-" + dir.ID, RunID: c.cfg.RunID, LaneID: p.LaneID, Kind: store.EventTimer, Class: store.EventClassAuto, Source: ActorController, Summary: integrateRecoveryReadyPrefix + p.ActionKey, Payload: payload})
		if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			return err
		}
		if err := c.store.Directives().UpdateStatus(ctx, c.cfg.RunID, dir.ID, store.DirectiveHandled); err != nil {
			return err
		}
	}
	return nil
}

// StartLane spawns a planned lane on its recipe's first phase.
func (c *Controller) StartLane(ctx context.Context, laneID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.killed {
		return ErrControllerStopped
	}
	if err := c.ensureLease(ctx); err != nil {
		return err
	}
	if err := c.syncDecisions(ctx); err != nil {
		return err
	}

	ln, err := c.store.Lanes().Get(ctx, c.cfg.RunID, laneID)
	if err != nil {
		return err
	}
	r, err := c.resolveRecipe(ctx, ln)
	if err != nil {
		return err
	}
	firstName := cmp.Or(ln.Phase, r.FirstPhase())
	firstPhase, ok := r.Phase(firstName)
	if !ok {
		return fmt.Errorf("orca/controller: recipe %q has no first phase", r.Name)
	}
	round := max(ln.Round, ln.MaxRound(firstName)+1)
	return c.startLanePhase(ctx, ln, r, firstPhase, round, 1, ToolPhaseStart, "", "spawn initial phase", "")
}

// AskOwner registers an owner ask whose default option is auto-applied at its deadline.
func (c *Controller) AskOwner(ctx context.Context, ask OwnerAsk) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.killed {
		return ErrControllerStopped
	}
	return c.registerOwnerAsk(ctx, ask, true)
}

func (c *Controller) registerOwnerAsk(ctx context.Context, ask OwnerAsk, block bool) error {
	ask.RunID = cmp.Or(ask.RunID, c.cfg.RunID)
	if err := ask.Validate(); err != nil {
		return err
	}
	if err := c.ensureLease(ctx); err != nil {
		return err
	}

	raw, err := json.Marshal(ask)
	if err != nil {
		return err
	}
	deadline := ask.Deadline.UTC()
	dir := store.Directive{
		ID:        ownerAskDirectiveID(ask.ID),
		RunID:     c.cfg.RunID,
		Kind:      store.DirectiveInstruction,
		Source:    store.DirectiveSourceDispatcher,
		Scope:     ask.LaneID,
		Ref:       ownerAskRefPrefix + ask.ID,
		Text:      string(raw),
		ExpiresAt: &deadline,
		Status:    store.DirectiveActive,
	}
	if err := c.store.Directives().Add(ctx, dir); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return err
	}

	if block && ask.LaneID != "" {
		if ln, err := c.store.Lanes().Get(ctx, c.cfg.RunID, ask.LaneID); err == nil && ln.Status == lane.StatusRunning {
			ln.Status = lane.StatusBlocked
			ln.LastEventAt = c.clock.Now().UTC()
			if err := c.store.Lanes().Upsert(ctx, *ln); err != nil {
				return err
			}
		}
	}
	return nil
}

// AnswerOwnerAsk records the owner's response to an open OwnerAsk before its deadline.
func (c *Controller) AnswerOwnerAsk(ctx context.Context, askID, chosenOption string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.killed {
		return ErrControllerStopped
	}
	return c.answerOwnerAsk(ctx, askID, chosenOption, false)
}

// AnswerOwnerAskText resolves an open ask with free-form owner text. It uses
// the same resolution path as mailbox replies, while allowing answers that are
// not one of the ask's listed options.
func (c *Controller) AnswerOwnerAskText(ctx context.Context, askID, answer string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.killed {
		return ErrControllerStopped
	}
	return c.answerOwnerAsk(ctx, askID, answer, true)
}

// AnswerOwnerAskTextFromJudge resolves an owner answer while the controller's
// judgment callback is running inside Step and already holds c.mu.
func (c *Controller) AnswerOwnerAskTextFromJudge(ctx context.Context, askID, answer string) error {
	if c.killed {
		return ErrControllerStopped
	}
	return c.answerOwnerAsk(ctx, askID, answer, true)
}

func (c *Controller) answerOwnerAsk(ctx context.Context, askID, chosenOption string, freeText bool) error {
	dir, err := c.store.Directives().Get(ctx, c.cfg.RunID, ownerAskDirectiveID(askID))
	if err != nil {
		return err
	}
	var ask OwnerAsk
	if err := json.Unmarshal([]byte(dir.Text), &ask); err != nil {
		return fmt.Errorf("orca/controller: invalid owner ask directive %q: %w", askID, err)
	}
	if strings.TrimSpace(chosenOption) == "" || (!freeText && !slices.Contains(ask.Options, chosenOption)) {
		return fmt.Errorf("%w: option %q not in ask %q options %v", store.ErrInvalidArgument, chosenOption, askID, ask.Options)
	}

	if dir.Status != store.DirectiveActive || !c.clock.Now().Before(ask.Deadline) {
		return nil
	}
	if err := c.applyOwnerAskResolution(ctx, ask, "ask-answer-"+ask.ID, chosenOption, chosenOption, ""); err != nil {
		return err
	}
	return c.store.Directives().UpdateStatus(ctx, c.cfg.RunID, dir.ID, store.DirectiveHandled)
}

// recordUndeliveredSteer preserves the fact that a steer was submitted even
// when the lane driver could not confirm that the agent resumed. The action
// key makes retries idempotent, while the urgent event lets the dispatcher
// decide how to handle the unconfirmed lane.
func (c *Controller) recordUndeliveredSteer(ctx context.Context, ln *lane.Lane, actionKey, tool, reason string, args DecisionArgs, err error) error {
	now := c.clock.Now().UTC()
	ln.AwaitingInputSince = now
	ln.AnswerAttemptedAt = now
	ln.AnswerConfirmedAt = time.Time{}
	if saveErr := c.store.Lanes().Upsert(ctx, *ln); saveErr != nil {
		return saveErr
	}
	eventID := "undelivered-" + actionKey
	ev, saveErr := c.store.Events().Append(ctx, store.Event{
		ID:      eventID,
		RunID:   c.cfg.RunID,
		LaneID:  ln.ID,
		Kind:    store.EventStall,
		Class:   store.EventClassJudgment,
		Source:  lane.AnswerDeliverySource,
		Urgent:  true,
		Summary: "answer not delivered: " + err.Error(),
	})
	if saveErr != nil && !errors.Is(saveErr, store.ErrAlreadyExists) {
		return saveErr
	}
	if saveErr == nil {
		c.enqueueJudgmentEvent(ev)
	}
	args.ActionKey = actionKey
	_, _, saveErr = c.recordDecision(ctx, tool, ln.ID, reason, err.Error(), args)
	return saveErr
}

func (c *Controller) applyOwnerAskResolution(ctx context.Context, ask OwnerAsk, directiveID, chosenOption, steerMsg, eventID string) error {
	resDir := store.Directive{
		ID:     directiveID,
		RunID:  c.cfg.RunID,
		Kind:   store.DirectiveInstruction,
		Source: store.DirectiveSourceOwner,
		Scope:  ask.LaneID,
		Ref:    ask.ID,
		Text:   chosenOption,
		Status: store.DirectiveActive,
	}
	if err := c.store.Directives().Add(ctx, resDir); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return err
	}
	if _, err := c.store.Notes().Append(ctx, store.RunNote{ID: "answer-" + ask.ID, RunID: c.cfg.RunID, TaskID: ask.LaneID, Text: "Question: " + ask.Question + "\nAnswer: " + chosenOption}); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return err
	}
	// A hold option is an explicit no-op for the blocked lane. It records the
	// owner's choice without steering or transitioning the lane to Running.
	if strings.HasPrefix(strings.TrimSpace(chosenOption), "hold:") {
		return nil
	}
	steerConfirmed := false
	if ask.LaneID != "" {
		if ln, err := c.store.Lanes().Get(ctx, c.cfg.RunID, ask.LaneID); err == nil {
			if !ln.Status.Terminal() && (ln.SessionID != "" || ln.TerminalID != "") {
				if c.hasActionDecision(directiveID) {
					// The prior submission is durable; do not send it again.
				} else if err := c.steerCurrentTerminal(ctx, ln, steerMsg); err != nil {
					var undelivered *lane.UndeliveredError
					if !errors.As(err, &undelivered) {
						return laneDriverError(err)
					}
					if err := c.recordUndeliveredSteer(ctx, ln, directiveID, "owner.answer", "owner answer submitted but not confirmed", DecisionArgs{AskID: ask.ID}, err); err != nil {
						return err
					}
				} else {
					steerConfirmed = true
				}
			} else {
				steerConfirmed = true
			}
			if ln.Status == lane.StatusBlocked && steerConfirmed {
				ln.Status = lane.StatusRunning
				ln.HostHoldReason = ""
				if eventID != "" {
					ln.LastEventID = eventID
				}
				ln.LastEventAt = c.clock.Now().UTC()
				if err := c.store.Lanes().Upsert(ctx, *ln); err != nil {
					return err
				}
			}
		}
	}
	msgID := strings.TrimSpace(ask.SourceMessageID)
	srcEventID := strings.TrimSpace(ask.SourceEventID)
	if msgID == "" && srcEventID != "" {
		msgID = c.resolveMailMessageID(ctx, srcEventID)
	}
	if msgID != "" || srcEventID != "" {
		if c.mailbox != nil && msgID != "" {
			if err := mailbox.Respond(ctx, c.mailbox, mailbox.Message{ID: msgID, RunID: c.cfg.RunID, LaneID: ask.LaneID, Question: true}, steerMsg); err != nil {
				return err
			}
		}
		if err := mailbox.MarkAnswered(ctx, c.store, c.cfg.RunID, ask.LaneID, cmp.Or(msgID, srcEventID), cmp.Or(srcEventID, eventID), steerMsg); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) resolveMailMessageID(ctx context.Context, eventID string) string {
	if c.store != nil {
		if evs, err := c.store.Events().List(ctx, store.EventFilter{RunID: c.cfg.RunID, Kinds: []store.EventKind{store.EventMail}}); err == nil {
			for i := range evs {
				if evs[i].ID == eventID && len(evs[i].Payload) > 0 {
					var msg mailbox.Message
					if json.Unmarshal(evs[i].Payload, &msg) == nil && strings.TrimSpace(msg.ID) != "" {
						return strings.TrimSpace(msg.ID)
					}
					return ""
				}
			}
		}
	}
	return ""
}

func (c *Controller) ensureLease(ctx context.Context) error {
	if c.cfg.SkipLease {
		return nil
	}
	if c.leaseHeld {
		_, err := c.store.Runs().HeartbeatLease(ctx, c.cfg.RunID, c.cfg.HolderID, c.cfg.LeaseTTL)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrLeaseNotHeld) {
			c.leaseHeld = false
			return err
		}
	}
	_, err := c.store.Runs().AcquireLease(ctx, c.cfg.RunID, c.cfg.HolderID, c.cfg.LeaseTTL)
	if err != nil {
		c.leaseHeld = false
		return err
	}
	c.leaseHeld = true
	return nil
}

func (c *Controller) syncDecisions(ctx context.Context) error {
	decs, err := c.store.Decisions().List(ctx, store.DecisionFilter{
		RunID:    c.cfg.RunID,
		SinceSeq: c.lastDecisionSeq,
	})
	if err != nil {
		return err
	}
	for i := range decs {
		d := decs[i]
		if d.Seq > c.lastDecisionSeq {
			c.lastDecisionSeq = d.Seq
		}
		c.indexDecision(d)
	}
	return nil
}

func (c *Controller) indexDecision(d store.Decision) {
	if len(d.Args) == 0 {
		return
	}
	var args DecisionArgs
	if err := json.Unmarshal(d.Args, &args); err != nil {
		return
	}
	if args.ActionKey != "" {
		c.decidedActions[args.ActionKey] = d
	}
	for _, termID := range args.StoppedTerminals {
		if t := strings.TrimSpace(termID); t != "" {
			c.stoppedTerminals[t] = true
		}
	}
	if args.EventID != "" && d.Tool != ToolLaneStall && d.Tool != ToolLaneEscalate && d.Tool != "lane.unpushed.report" && d.Tool != "lane.health.disk" {
		c.decidedEvents[args.EventID] = d
	}
	for _, evID := range args.EventIDs {
		if evID != "" {
			c.dispatchedEvents[evID] = d
		}
	}
}

func (c *Controller) hasActionDecision(actionKey string) bool {
	if actionKey == "" {
		return false
	}
	_, ok := c.decidedActions[actionKey]
	return ok
}

func (c *Controller) isEventHandled(eventID string) bool {
	if eventID == "" {
		return false
	}
	if _, ok := c.decidedEvents[eventID]; ok {
		return true
	}
	if _, ok := c.dispatchedEvents[eventID]; ok {
		return true
	}
	return false
}

func (c *Controller) recordDecision(ctx context.Context, tool, laneID, reason, result string, args DecisionArgs) (store.Decision, bool, error) {
	if args.ActionKey != "" && c.hasActionDecision(args.ActionKey) {
		return c.decidedActions[args.ActionKey], true, nil
	}
	if args.EventID != "" {
		if existing, ok := c.decidedEvents[args.EventID]; ok {
			return existing, true, nil
		}
	}

	rawArgs, err := json.Marshal(args)
	if err != nil {
		return store.Decision{}, false, err
	}
	dec, err := c.store.Decisions().Append(ctx, store.Decision{
		RunID:        c.cfg.RunID,
		Category:     store.DecisionAuto,
		Actor:        ActorController,
		LaneID:       laneID,
		Tool:         tool,
		Reason:       reason,
		Result:       result,
		Args:         rawArgs,
		Vetted:       true,
		TraceID:      args.EventID,
		OutcomeLabel: controllerOutcomeLabel(result),
		CreatedAt:    c.clock.Now().UTC(),
	})
	if err != nil {
		return store.Decision{}, false, err
	}
	if dec.Seq > c.lastDecisionSeq {
		c.lastDecisionSeq = dec.Seq
	}
	c.indexDecision(dec)
	return dec, false, nil
}

func controllerOutcomeLabel(result string) string {
	s := strings.ToLower(result)
	switch {
	case strings.Contains(s, "merged"):
		return "merged"
	case strings.Contains(s, "retry"), strings.Contains(s, "rework"):
		return "reworked"
	case strings.Contains(s, "drop"), strings.Contains(s, "abandon"):
		return "abandoned"
	case strings.Contains(s, "correct"):
		return "owner corrected"
	default:
		return ""
	}
}

// recoverRecordedLaunches settles successful effects before liveness checks or replay.
func (c *Controller) recoverRecordedLaunches(ctx context.Context) error {
	keys := make([]string, 0)
	for key := range c.decidedActions {
		if strings.HasPrefix(key, "intent:phase.") {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		dec := c.decidedActions[key]
		var args DecisionArgs
		if err := json.Unmarshal(dec.Args, &args); err != nil {
			return err
		}
		tool := strings.TrimSuffix(dec.Tool, ".intent")
		switch tool {
		case ToolPhaseStart, ToolPhaseAdvance, ToolPhaseRework, ToolPhaseRetry:
		default:
			continue
		}
		if args.Phase == recipe.PhaseIntegrate {
			continue
		}
		actionKey := phaseActionKey(tool, dec.LaneID, args.Phase, args.Round, args.Attempt)
		if key != "intent:"+actionKey || c.hasActionDecision(actionKey) {
			continue
		}
		ln, err := c.store.Lanes().Get(ctx, c.cfg.RunID, dec.LaneID)
		if err != nil {
			return err
		}
		if ln.Status != lane.StatusRunning && ln.Status != lane.StatusPlanned {
			continue
		}
		_, hasReceipt := c.decidedActions["started:"+actionKey]
		savedLane := ln.Status == lane.StatusRunning && ln.Phase == args.Phase && ln.Round == args.Round && ln.Attempt == args.Attempt
		if !hasReceipt && !savedLane {
			continue
		}
		r, err := c.resolveRecipe(ctx, ln)
		if err != nil {
			return err
		}
		phase, ok := r.Phase(args.Phase)
		if !ok {
			continue
		}
		failureKey := "launch-recovery:" + actionKey
		if nextTry := c.laneFailures[failureKey]; !nextTry.IsZero() && c.clock.Now().Before(nextTry) {
			continue
		}
		triggerEventID := args.TriggerEventID
		if triggerEventID == "" && tool != ToolPhaseStart {
			triggerEventID = ln.LastEventID
		}
		if err := c.startLanePhase(ctx, ln, r, phase, args.Round, args.Attempt, tool, triggerEventID, dec.Reason, ""); err != nil {
			if !c.cfg.IsolateLaneErrors || !isIsolatedFailure(err) {
				return err
			}
			if recordErr := c.recordLaneFailure(ctx, ln, failureKey, tool, err); recordErr != nil {
				return recordErr
			}
			continue
		}
		delete(c.laneFailures, failureKey)
	}
	return nil
}

// recoverPendingState reconstructs pending judgments and CI-gated verdicts on startup.
func (c *Controller) recoverPendingState(ctx context.Context) error {
	cur, err := c.store.Events().GetCursor(ctx, c.cfg.RunID, c.cfg.Consumer)
	if err != nil {
		return err
	}
	if cur.Seq == 0 {
		return nil
	}

	events, err := c.store.Events().List(ctx, store.EventFilter{
		RunID: c.cfg.RunID,
	})
	if err != nil {
		return err
	}

	lanes, err := c.store.Lanes().List(ctx, store.LaneFilter{
		RunID:    c.cfg.RunID,
		Statuses: []store.LaneStatus{lane.StatusRunning},
	})
	if err != nil {
		return err
	}
	for i := range events {
		ev := events[i]
		if ev.Source != eventFailureSource || len(ev.Payload) == 0 {
			continue
		}
		var original store.Event
		if err := json.Unmarshal(ev.Payload, &original); err != nil {
			return err
		}
		if !c.hasActionDecision("event.retry.complete:" + original.ID) {
			c.queueEventRetry(original)
		}
	}
	runningByID := make(map[string]lane.Lane, len(lanes))
	for i := range lanes {
		runningByID[lanes[i].ID] = lanes[i]
	}

	for i := range events {
		ev := events[i]
		if ev.Seq > cur.Seq {
			break
		}
		if c.isEventHandled(ev.ID) {
			continue
		}
		if ev.Kind == store.EventLaneReport && ev.LaneID != "" {
			if ln, ok := runningByID[ev.LaneID]; ok {
				if rep, err := c.extractReport(ctx, &ln, ev); err == nil && rep.Outcome == lane.OutcomeDone {
					c.pendingVerdicts[ln.ID] = pendingVerdict{
						key:     ln.AttemptKey(),
						verdict: string(rep.Outcome),
						headSHA: rep.HeadSHA,
					}
					continue
				}
			}
			var rep lane.PhaseReport
			if json.Unmarshal(ev.Payload, &rep) == nil && (rep.Outcome == lane.OutcomeNeedsInput || rep.Outcome == lane.OutcomeBlocked) {
				ev.Urgent = true
			}
			ev.Class = store.EventClassJudgment
			c.enqueueJudgmentEvent(ev)
			continue
		}
		if ev.Kind == store.EventPhaseExit && ev.LaneID != "" {
			var payload PhaseExitPayload
			if json.Unmarshal(ev.Payload, &payload) == nil && payload.Kind == lane.EventWaitingForInput {
				ln, running := runningByID[ev.LaneID]
				if ev.Class == store.EventClassAuto || !running || ln.AwaitingInputSince.IsZero() || lane.WorkingInputWaiting(payload.Error) {
					continue
				}
				termID := cmp.Or(strings.TrimSpace(payload.TerminalID), strings.TrimSpace(ln.TerminalID))
				if termID != strings.TrimSpace(ln.TerminalID) || c.stoppedTerminals[termID] {
					continue
				}
				tail := waitingInputTail(payload.Error)
				ev.ID = waitingInputEventID(ev.LaneID, termID, tail)
				ev.Summary = waitingInputSummary(ev.LaneID, tail)
				ev.Urgent = true
				ev.Class = store.EventClassJudgment
				c.enqueueJudgmentEvent(ev)
				continue
			}
		}
		if isRecoverableJudgmentEvent(ev) {
			ev.Class = store.EventClassJudgment
			c.enqueueJudgmentEvent(ev)
		}
	}
	return nil
}

func isRecoverableJudgmentEvent(ev store.Event) bool {
	if ev.Class == store.EventClassJudgment {
		return true
	}
	if ev.Class == store.EventClassAuto {
		return false
	}
	switch ev.Kind {
	case store.EventOwnerMessage, store.EventMail, store.EventStall:
		return true
	case store.EventPhaseExit:
		var payload PhaseExitPayload
		return json.Unmarshal(ev.Payload, &payload) == nil && payload.Kind == lane.EventWaitingForInput
	default:
		return false
	}
}

func (c *Controller) reconcilePlannedLanes(ctx context.Context) error {
	planned, err := c.store.Lanes().List(ctx, store.LaneFilter{
		RunID:    c.cfg.RunID,
		Statuses: []store.LaneStatus{lane.StatusPlanned},
	})
	if err != nil || len(planned) == 0 {
		return err
	}

	running, err := c.store.Lanes().List(ctx, store.LaneFilter{RunID: c.cfg.RunID})
	if err != nil {
		return err
	}
	activeCount := 0
	for i := range running {
		if running[i].Status == lane.StatusRunning {
			activeCount++
		}
	}

	for i := range planned {
		ln := planned[i]
		ready, err := c.dependenciesReady(ctx, ln.ItemID)
		if err != nil {
			return err
		}
		if !ready {
			continue
		}
		r, err := c.resolveRecipe(ctx, &ln)
		if err != nil {
			return err
		}
		cap := r.Limits.MaxConcurrentLanes
		if c.runtimeSettingsSet {
			cap = c.cfg.ConcurrencyCap
		}
		if cap > 0 && activeCount >= cap {
			break
		}
		firstName := cmp.Or(ln.Phase, r.FirstPhase())
		firstPhase, ok := r.Phase(firstName)
		if !ok {
			continue
		}
		round := max(ln.Round, ln.MaxRound(firstName)+1)
		failureKey := fmt.Sprintf("planned:%s:%s:%d", ln.ID, firstPhase.Name, round)
		if nextTry := c.laneFailures[failureKey]; !nextTry.IsZero() && c.clock.Now().Before(nextTry) {
			continue
		}
		if err := c.startLanePhase(ctx, &ln, r, firstPhase, round, 1, ToolPhaseStart, "", "spawn planned lane", ""); err != nil {
			if !c.cfg.IsolateLaneErrors || !isIsolatedFailure(err) {
				return err
			}
			if recordErr := c.recordLaneFailure(ctx, &ln, failureKey, ToolPhaseStart, err); recordErr != nil {
				return recordErr
			}
			continue
		}
		delete(c.laneFailures, failureKey)
		activeCount++
	}
	return nil
}

func (c *Controller) recordLaneFailure(ctx context.Context, ln *lane.Lane, key, tool string, cause error) error {
	count := c.laneFailureCounts[key] + 1
	c.laneFailureCounts[key] = count
	c.laneFailures[key] = c.clock.Now().Add(time.Second * time.Duration(1<<min(count-1, 5)))
	decisionKey := "lane-failure:" + key
	if !c.hasActionDecision(decisionKey) {
		c.stepError = fmt.Errorf("lane %s: %w", ln.ID, cause)
		if c.cfg.StepError != nil {
			c.cfg.StepError(c.stepError)
		}
		if _, _, err := c.recordDecision(ctx, tool+".failure", ln.ID, "lane lifecycle action failed", cause.Error(), DecisionArgs{ActionKey: decisionKey, Phase: ln.Phase, Round: max(ln.Round, 1), Attempt: max(ln.Attempt, 1)}); err != nil {
			return err
		}
		eventID := "lane-failure-event:" + key
		if _, err := c.store.Events().Append(ctx, store.Event{ID: eventID, RunID: c.cfg.RunID, LaneID: ln.ID, Kind: store.EventTimer, Class: store.EventClassJudgment, Urgent: true, Source: ActorController, Summary: c.stepError.Error()}); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			return err
		}
	}
	return nil
}

func (c *Controller) dependenciesReady(ctx context.Context, itemID string) (bool, error) {
	if itemID == "" {
		return true, nil
	}
	item, err := c.store.Backlog().Get(ctx, c.cfg.RunID, itemID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return true, nil
		}
		return false, err
	}
	for _, dependencyID := range item.DependsOn {
		dependency, err := c.store.Backlog().Get(ctx, c.cfg.RunID, dependencyID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return false, nil
			}
			return false, err
		}
		if dependency.Status != store.BacklogDone {
			return false, nil
		}
	}
	return true, nil
}

func (c *Controller) detectStalls(ctx context.Context) error {
	running, err := c.store.Lanes().List(ctx, store.LaneFilter{
		RunID:    c.cfg.RunID,
		Statuses: []store.LaneStatus{lane.StatusRunning},
	})
	if err != nil {
		return err
	}

	now := c.clock.Now().UTC()
	for i := range running {
		ln := running[i]
		if !ln.AwaitingInputSince.IsZero() {
			continue
		}
		if c.laneIsWorkingOrBusy(ctx, ln) || c.hasActiveExpectedWait(ctx, ln.ID, now) {
			continue
		}
		r, err := c.resolveRecipe(ctx, &ln)
		if err != nil {
			return err
		}
		timeout := c.effectiveStallTimeout(r)
		if timeout <= 0 {
			continue
		}

		lastActive := ln.LastEventAt
		if lastActive.IsZero() {
			lastActive = ln.UpdatedAt
		}
		if lastActive.IsZero() {
			lastActive = ln.CreatedAt
		}
		if lastActive.IsZero() || now.Before(lastActive.Add(timeout)) {
			continue
		}

		if rung, waiting := c.quietLadderRungFromStore(ctx, &ln, r, now); waiting || rung == 1 {
			continue
		}

		attempt := max(ln.Attempt, 1)
		actionKey := stallActionKey(ln.ID, ln.Phase, ln.Round, attempt, ln.TerminalID)
		if c.hasActionDecision(actionKey) || c.emittedStalls[actionKey] {
			continue
		}
		c.emittedStalls[actionKey] = true

		_, err = c.store.Events().Append(ctx, store.Event{
			RunID:   c.cfg.RunID,
			Kind:    store.EventStall,
			Class:   store.EventClassJudgment,
			LaneID:  ln.ID,
			Source:  ActorController,
			Summary: fmt.Sprintf("lane %s stalled in phase %s (round %d, attempt %d) after %s", ln.ID, ln.Phase, max(ln.Round, 1), attempt, now.Sub(lastActive)),
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) steerCurrentTerminal(ctx context.Context, ln *lane.Lane, message string) error {
	if ln == nil || ln.Status.Terminal() {
		return nil
	}
	return c.driver.Steer(ctx, lane.SteerRequest{RunID: c.cfg.RunID, LaneID: ln.ID, SessionID: ln.SessionID, TerminalID: ln.TerminalID, Message: message})
}

func (c *Controller) checkLaneSessions(ctx context.Context) error {
	if c.driver == nil {
		return nil
	}
	running, err := c.store.Lanes().List(ctx, store.LaneFilter{
		RunID:    c.cfg.RunID,
		Statuses: []store.LaneStatus{lane.StatusRunning},
	})
	if err != nil {
		return err
	}
	now := c.clock.Now().UTC()
	for i := range running {
		ln := running[i]
		c.stopStaleRecordedTerminals(ctx, &ln)
		working := c.laneIsWorkingOrBusy(ctx, ln)
		changed := false
		if working && !ln.AwaitingInputSince.IsZero() {
			ln.AwaitingInputSince = time.Time{}
			changed = true
		}
		if working && ln.LastEventAt.Before(now) {
			ln.LastEventAt = now
			changed = true
		}
		if changed {
			if err := c.store.Lanes().Upsert(ctx, ln); err != nil {
				return err
			}
		}
		if err := c.checkLaneUnpushedWork(ctx, &ln, now); err != nil {
			return err
		}
		if err := c.checkLaneDiskHealth(ctx, &ln); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) stopStaleRecordedTerminals(ctx context.Context, ln *lane.Lane) {
	if c.driver == nil || ln == nil || ln.Status.Terminal() || len(ln.Terminals) == 0 {
		return
	}
	curKey := ln.AttemptKey()
	keys := make([]string, 0, len(ln.Terminals))
	for k := range ln.Terminals {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		termID := strings.TrimSpace(ln.Terminals[k])
		if termID == "" || termID == ln.TerminalID || k == curKey || c.stoppedTerminals[termID] {
			continue
		}
		stopKey := ln.ID + ":" + termID
		if c.staleStopFailures[stopKey] >= 3 {
			continue
		}
		sessID := ln.SessionID
		if s := strings.TrimSpace(ln.Sessions[k]); s != "" {
			sessID = s
		}
		_, _, _ = c.recordDecision(ctx, ToolPhaseRetry+".intent", ln.ID,
			"stop stale previous-phase terminal", "stop pending",
			DecisionArgs{ActionKey: "intent:stale-stop:" + stopKey, Phase: ln.Phase, Round: max(ln.Round, 1), Attempt: max(ln.Attempt, 1), SessionID: sessID, TerminalID: termID})
		if err := c.driver.Stop(ctx, lane.StopRequest{
			RunID:      c.cfg.RunID,
			LaneID:     ln.ID,
			SessionID:  sessID,
			TerminalID: termID,
			Reason:     "stop stale previous-phase terminal",
		}); err == nil {
			c.stoppedTerminals[termID] = true
			delete(c.staleStopFailures, stopKey)
			_, _, _ = c.recordDecision(ctx, ToolPhaseRetry+".stop", ln.ID,
				"stopped stale previous-phase terminal", "stopped terminal",
				DecisionArgs{ActionKey: "stale-stopped:" + stopKey, Phase: ln.Phase, Round: max(ln.Round, 1), Attempt: max(ln.Attempt, 1), SessionID: sessID, TerminalID: termID, StoppedTerminals: []string{termID}})
		} else {
			c.staleStopFailures[stopKey]++
		}
	}
}

func (c *Controller) checkLaneUnpushedWork(ctx context.Context, ln *lane.Lane, now time.Time) error {
	gr, ok := c.driver.(lane.GitStateReader)
	if !ok || ln == nil || ln.Status.Terminal() || (strings.TrimSpace(ln.SessionID) == "" && strings.TrimSpace(ln.TerminalID) == "") {
		return nil
	}
	st, err := gr.LaneGitState(ctx, *ln)
	if err != nil {
		return nil
	}
	if !st.HasUnpushedWork() {
		delete(c.unpushedSince, ln.ID)
		return nil
	}
	oldestAt := st.OldestUnpushedAt
	if oldestAt.IsZero() {
		if seenAt, seen := c.unpushedSince[ln.ID]; seen {
			oldestAt = seenAt
		} else {
			oldestAt = cmp.Or(ln.LastEventAt, ln.UpdatedAt, ln.CreatedAt, now)
			c.unpushedSince[ln.ID] = oldestAt
		}
	}
	threshold := c.cfg.UnpushedWorkThreshold
	if threshold <= 0 {
		threshold = DefaultUnpushedWorkThreshold
	}
	if oldestAt.IsZero() || now.Sub(oldestAt) < threshold {
		return nil
	}
	age := now.Sub(oldestAt).Round(time.Minute)
	steerKey := "unpushed:steer:" + ln.ID
	reportKey := "unpushed:report:" + ln.ID
	if !c.hasActionDecision(steerKey) {
		msg := fmt.Sprintf("You have unpushed work in this lane session (%d unpushed commits, %d dirty files, oldest change %s ago). Commit and push a WIP commit to your lane branch now so a session death loses nothing.", st.UnpushedCommits, len(st.DirtyFiles), age)
		if err := c.steerCurrentTerminal(ctx, ln, msg); err != nil {
			var undelivered *lane.UndeliveredError
			if !errors.As(err, &undelivered) {
				return laneDriverError(err)
			}
			return c.recordUndeliveredSteer(ctx, ln, steerKey, "lane.unpushed.steer", "unpushed work steer submitted but not confirmed", DecisionArgs{Phase: ln.Phase, Round: max(ln.Round, 1), Attempt: max(ln.Attempt, 1), SessionID: ln.SessionID, TerminalID: ln.TerminalID}, err)
		}
		_, _, err := c.recordDecision(ctx, "lane.unpushed.steer", ln.ID,
			"unpushed work older than threshold; steered lane to push WIP",
			"steered lane to push WIP",
			DecisionArgs{
				ActionKey:  steerKey,
				Phase:      ln.Phase,
				Round:      max(ln.Round, 1),
				Attempt:    max(ln.Attempt, 1),
				SessionID:  ln.SessionID,
				TerminalID: ln.TerminalID,
			},
		)
		return err
	}
	if !c.hasActionDecision(reportKey) {
		summary := fmt.Sprintf("lane %s still has unpushed work after WIP steer (%d unpushed commits, %d dirty files, oldest %s ago)", ln.ID, st.UnpushedCommits, len(st.DirtyFiles), age)
		ev, err := c.store.Events().Append(ctx, store.Event{
			ID:      "unpushed-" + ln.ID,
			RunID:   c.cfg.RunID,
			LaneID:  ln.ID,
			Kind:    store.EventStall,
			Class:   store.EventClassJudgment,
			Urgent:  true,
			Source:  "lane.unpushed",
			Summary: summary,
		})
		if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			return err
		}
		_, _, err = c.recordDecision(ctx, "lane.unpushed.report", ln.ID,
			summary,
			"reported persistent unpushed work to dispatcher",
			DecisionArgs{
				ActionKey:  reportKey,
				EventID:    ev.ID,
				Phase:      ln.Phase,
				Round:      max(ln.Round, 1),
				Attempt:    max(ln.Attempt, 1),
				SessionID:  ln.SessionID,
				TerminalID: ln.TerminalID,
			},
		)
		return err
	}
	return nil
}

func (c *Controller) checkLaneDiskHealth(ctx context.Context, ln *lane.Lane) error {
	hr, ok := c.driver.(lane.SessionHealthReader)
	if !ok || ln == nil || ln.Status.Terminal() || (strings.TrimSpace(ln.SessionID) == "" && strings.TrimSpace(ln.TerminalID) == "") {
		return nil
	}
	health, err := c.laneSessionHealth(ctx, *ln, hr)
	if err != nil {
		return nil
	}
	threshold := c.cfg.DiskPressurePercent
	if threshold <= 0 {
		threshold = DefaultDiskPressurePercent
	}
	if health.DiskUsedPercent < threshold {
		return nil
	}
	diskKey := "disk:pressure:" + ln.ID
	if c.hasActionDecision(diskKey) {
		return nil
	}
	noteMsg := fmt.Sprintf("Session disk usage is at %d%%. Free space without dropping the build cache (remove temporary artifacts, logs, or scratch files, not the Bazel build cache) and rerun only the failed step.", health.DiskUsedPercent)
	if err := c.steerCurrentTerminal(ctx, ln, noteMsg); err != nil {
		var undelivered *lane.UndeliveredError
		if !errors.As(err, &undelivered) {
			return laneDriverError(err)
		}
		if saveErr := c.recordUndeliveredSteer(ctx, ln, diskKey, "lane.health.disk", "disk pressure steer submitted but not confirmed", DecisionArgs{Phase: ln.Phase, Round: max(ln.Round, 1), Attempt: max(ln.Attempt, 1), SessionID: ln.SessionID, TerminalID: ln.TerminalID}, err); saveErr != nil {
			return saveErr
		}
		_, _ = c.store.Notes().Append(ctx, store.RunNote{ID: "disk-pressure-" + ln.ID, RunID: c.cfg.RunID, TaskID: cmp.Or(ln.ItemID, ln.ID), Phase: ln.Phase, Text: noteMsg})
		return nil
	}
	_, _ = c.store.Notes().Append(ctx, store.RunNote{
		ID:     "disk-pressure-" + ln.ID,
		RunID:  c.cfg.RunID,
		TaskID: cmp.Or(ln.ItemID, ln.ID),
		Phase:  ln.Phase,
		Text:   noteMsg,
	})
	summary := fmt.Sprintf("lane %s session disk pressure at %d%%; steered lane to free space without dropping build cache and rerun only the failed step", ln.ID, health.DiskUsedPercent)
	ev, err := c.store.Events().Append(ctx, store.Event{
		ID:      "disk-pressure-" + ln.ID,
		RunID:   c.cfg.RunID,
		LaneID:  ln.ID,
		Kind:    store.EventStall,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Source:  "lane.health",
		Summary: summary,
	})
	if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return err
	}
	_, _, err = c.recordDecision(ctx, "lane.health.disk", ln.ID,
		summary,
		"steered lane and alerted dispatcher",
		DecisionArgs{
			ActionKey:  diskKey,
			EventID:    ev.ID,
			Phase:      ln.Phase,
			Round:      max(ln.Round, 1),
			Attempt:    max(ln.Attempt, 1),
			SessionID:  ln.SessionID,
			TerminalID: ln.TerminalID,
		},
	)
	return err
}

func (c *Controller) checkOwnerAsks(ctx context.Context) error {
	dirs, err := c.store.Directives().List(ctx, store.DirectiveFilter{
		RunID:      c.cfg.RunID,
		Kinds:      []store.DirectiveKind{store.DirectiveInstruction},
		ActiveOnly: false,
	})
	if err != nil {
		return err
	}

	now := c.clock.Now().UTC()
	for i := range dirs {
		d := dirs[i]
		if d.Status != store.DirectiveActive || !strings.HasPrefix(d.Ref, ownerAskRefPrefix) {
			continue
		}
		if d.ExpiresAt == nil || now.Before(*d.ExpiresAt) {
			continue
		}
		var ask OwnerAsk
		if err := json.Unmarshal([]byte(d.Text), &ask); err != nil {
			continue
		}
		actionKey := askDefaultActionKey(ask.ID)
		if c.hasActionDecision(actionKey) || c.emittedAskExpiries[ask.ID] {
			continue
		}
		_, err := c.store.Events().Append(ctx, store.Event{
			RunID:   c.cfg.RunID,
			Kind:    store.EventOwnerAskExpired,
			Class:   store.EventClassAuto,
			LaneID:  ask.LaneID,
			Source:  ActorController,
			Summary: fmt.Sprintf("owner ask %s expired; applying default %q", ask.ID, ask.Default),
			Payload: json.RawMessage(d.Text),
		})
		if err != nil {
			return err
		}
		c.emittedAskExpiries[ask.ID] = true
	}
	return nil
}

func (c *Controller) consumeEvents(ctx context.Context) error {
	cur, err := c.store.Events().GetCursor(ctx, c.cfg.RunID, c.cfg.Consumer)
	if err != nil {
		return err
	}

	events, err := c.store.Events().List(ctx, store.EventFilter{
		RunID:    c.cfg.RunID,
		AfterSeq: cur.Seq,
	})
	if err != nil {
		return err
	}

	urgentFlush := false
	for i := range events {
		ev := events[i]
		if c.isEventHandled(ev.ID) {
			if err := c.store.Events().SetCursor(ctx, c.cfg.RunID, c.cfg.Consumer, ev.Seq); err != nil {
				return err
			}
			continue
		}

		class, err := c.handleEvent(ctx, &ev)
		if err != nil {
			if !c.cfg.IsolateLaneErrors || !isIsolatedFailure(err) {
				return err
			}
			c.stepError = fmt.Errorf("event %s (lane %s): %w", ev.ID, ev.LaneID, err)
			if c.cfg.StepError != nil {
				c.cfg.StepError(c.stepError)
			}
			failureKey := "event-failure:" + ev.ID
			if !c.hasActionDecision(failureKey) {
				failure := store.Event{ID: failureKey, RunID: c.cfg.RunID, LaneID: ev.LaneID, Kind: store.EventTimer, Class: store.EventClassJudgment, Urgent: true, Source: eventFailureSource, Summary: c.stepError.Error()}
				var local *LaneLocalError
				if errors.As(err, &local) {
					payload, marshalErr := json.Marshal(ev)
					if marshalErr != nil {
						return marshalErr
					}
					failure.Payload = payload
				}
				if _, appendErr := c.store.Events().Append(ctx, failure); appendErr != nil && !errors.Is(appendErr, store.ErrAlreadyExists) {
					return appendErr
				}
				if _, _, dErr := c.recordDecision(ctx, "event.failure", ev.LaneID, "event handler failed", err.Error(), DecisionArgs{ActionKey: failureKey, EventID: ev.ID}); dErr != nil {
					return dErr
				}
				if len(failure.Payload) != 0 {
					c.queueEventRetry(ev)
				}
				c.enqueueJudgmentEvent(failure)
				urgentFlush = true
			}
			if cursorErr := c.store.Events().SetCursor(ctx, c.cfg.RunID, c.cfg.Consumer, ev.Seq); cursorErr != nil {
				return cursorErr
			}
			continue
		}
		ev.Class = class

		if class == store.EventClassJudgment {
			c.enqueueJudgmentEvent(ev)
			if ev.Urgent || c.cfg.JudgmentDebounce <= 0 {
				urgentFlush = true
			}
		}

		if err := c.store.Events().SetCursor(ctx, c.cfg.RunID, c.cfg.Consumer, ev.Seq); err != nil {
			return err
		}
	}
	if err := c.advanceExpiredNudges(ctx, &urgentFlush); err != nil {
		return err
	}
	if urgentFlush {
		if err := c.flushJudgment(ctx, "urgent bypass"); err != nil {
			return err
		}
	}
	return nil
}

// The original failed event is retained in the notification payload so an
// advanced inbox cursor does not discard its lane's retry after a restart.
func (c *Controller) queueEventRetry(ev store.Event) {
	if _, queued := c.eventRetries[ev.ID]; queued {
		return
	}
	c.eventRetries[ev.ID] = ev.Clone()
	c.backoffEventRetry(ev.ID)
}

func (c *Controller) backoffEventRetry(id string) {
	key := "event:" + id
	count := c.laneFailureCounts[key] + 1
	c.laneFailureCounts[key] = count
	c.laneFailures[key] = c.clock.Now().Add(time.Second * time.Duration(1<<min(count-1, 5)))
}

func (c *Controller) retryFailedEvents(ctx context.Context) error {
	retries := make([]string, 0, len(c.eventRetries))
	for id := range c.eventRetries {
		retries = append(retries, id)
	}
	slices.SortFunc(retries, func(a, b string) int { return cmp.Compare(c.eventRetries[a].Seq, c.eventRetries[b].Seq) })
	for _, id := range retries {
		ev := c.eventRetries[id]
		key := "event:" + ev.ID
		if c.clock.Now().Before(c.laneFailures[key]) {
			continue
		}
		class, err := c.handleEvent(ctx, &ev)
		if err != nil {
			if !isIsolatedFailure(err) {
				return err
			}
			c.backoffEventRetry(ev.ID)
			continue
		}
		if _, _, err := c.recordDecision(ctx, "event.retry.complete", ev.LaneID, "failed event retry completed", "handled", DecisionArgs{ActionKey: "event.retry.complete:" + ev.ID}); err != nil {
			return err
		}
		delete(c.eventRetries, ev.ID)
		delete(c.laneFailures, key)
		delete(c.laneFailureCounts, key)
		if class == store.EventClassJudgment {
			ev.Class = class
			c.enqueueJudgmentEvent(ev)
		}
	}
	return nil
}

func (c *Controller) advanceExpiredNudges(ctx context.Context, urgentFlush *bool) error {
	if c.driver == nil || c.store == nil {
		return nil
	}
	running, err := c.store.Lanes().List(ctx, store.LaneFilter{
		RunID:    c.cfg.RunID,
		Statuses: []store.LaneStatus{lane.StatusRunning},
	})
	if err != nil {
		return err
	}
	now := c.clock.Now().UTC()
	for i := range running {
		ln := running[i]
		if !ln.AwaitingInputSince.IsZero() {
			continue
		}
		if c.terminalIsWorkingOrUnknown(ctx, ln.TerminalID) || c.sessionIsBusy(ctx, ln) || c.hasActiveExpectedWait(ctx, ln.ID, now) {
			continue
		}
		r, err := c.resolveRecipe(ctx, &ln)
		if err != nil {
			return err
		}
		rung, waiting := c.quietLadderRungFromStore(ctx, &ln, r, now)
		if rung != 1 || waiting {
			continue
		}
		key := c.quietLaneKey(&ln)
		if c.hasActionDecision("restart:fresh:" + key) {
			continue
		}
		raw, err := json.Marshal(PhaseExitPayload{
			Kind:       lane.EventNoActivity,
			TerminalID: ln.TerminalID,
			Phase:      ln.Phase,
			Error:      "nudge wait expired without activity",
		})
		if err != nil {
			return err
		}
		evID := fmt.Sprintf("nudge-expired:%s:%d:%s", key, max(ln.Attempt, 1), ln.TerminalID)
		if c.isEventHandled(evID) {
			continue
		}
		ev := store.Event{
			ID:        evID,
			RunID:     c.cfg.RunID,
			LaneID:    ln.ID,
			Kind:      store.EventPhaseExit,
			Class:     store.EventClassAuto,
			Source:    "lane.watch",
			Summary:   fmt.Sprintf("lane %s quiet after nudge wait", ln.ID),
			Payload:   raw,
			CreatedAt: now,
		}
		class, err := c.handlePhaseExit(ctx, &ev)
		if err != nil {
			return err
		}
		ev.Class = class
		if class == store.EventClassJudgment {
			c.enqueueJudgmentEvent(ev)
			if urgentFlush != nil && (ev.Urgent || c.cfg.JudgmentDebounce <= 0) {
				*urgentFlush = true
			}
		}
	}
	return nil
}

func (c *Controller) handleEvent(ctx context.Context, ev *store.Event) (store.EventClass, error) {
	// Transport envelopes are retained for the driver. Watch supplies the
	// canonical lifecycle event; consuming both would advance the ladder twice.
	if ev.Kind == store.EventLaneReport || ev.Kind == store.EventPhaseExit || ev.Kind == store.EventStall {
		var envelope struct {
			Report        json.RawMessage `json:"report"`
			TerminalEvent json.RawMessage `json:"terminal_event"`
		}
		if json.Unmarshal(ev.Payload, &envelope) == nil && (len(envelope.Report) > 0 || len(envelope.TerminalEvent) > 0) {
			return store.EventClassAuto, nil
		}
	}
	switch ev.Kind {
	case store.EventLaneReport:
		return c.handleLaneReport(ctx, ev)
	case store.EventPhaseExit:
		return c.handlePhaseExit(ctx, ev)
	case store.EventCI:
		return c.handleCI(ctx, ev)
	case store.EventStall:
		return c.handleStall(ctx, ev)
	case store.EventOwnerAskExpired:
		return c.handleOwnerAskExpired(ctx, ev)
	case store.EventOwnerMessage, store.EventMail:
		if ev.Kind == store.EventMail && (ev.Class == store.EventClassAuto || ev.Source == mailbox.AdoptMailSummarySource || ev.Source == "cost.guard") {
			return store.EventClassAuto, nil
		}
		if ev.Kind == store.EventOwnerMessage && len(ev.Payload) > 0 {
			var msg mailbox.Message
			if json.Unmarshal(ev.Payload, &msg) == nil && msg.InReplyTo != "" {
				err := c.answerOwnerAsk(ctx, msg.InReplyTo, msg.Body, true)
				if errors.Is(err, store.ErrNotFound) {
					return store.EventClassJudgment, nil
				}
				return store.EventClassAuto, err
			}
		}
		if ev.Kind == store.EventMail {
			handled, err := c.routeWorkerQuestion(ctx, ev)
			if err != nil {
				return store.EventClassUnclassified, err
			}
			if handled {
				return store.EventClassAuto, nil
			}
		}
		return store.EventClassJudgment, nil
	case store.EventTimer:
		if strings.HasPrefix(ev.Summary, integrateRecoveryReadyPrefix) {
			ln, err := c.store.Lanes().Get(ctx, c.cfg.RunID, ev.LaneID)
			if err != nil {
				return store.EventClassUnclassified, err
			}
			if ln.Status != lane.StatusRunning || ln.Phase != recipe.PhaseIntegrate {
				return store.EventClassAuto, nil
			}
			r, err := c.resolveRecipe(ctx, ln)
			if err != nil {
				return store.EventClassUnclassified, err
			}
			phase, ok := r.Phase(recipe.PhaseIntegrate)
			if !ok {
				return store.EventClassUnclassified, fmt.Errorf("orca/controller: missing integrate phase")
			}
			if err := c.startLanePhase(ctx, ln, r, phase, max(ln.Round, 1), max(ln.Attempt, 1), ToolPhaseRetry, ev.ID, "retry interrupted integrate launch after backoff", ""); err != nil {
				return store.EventClassUnclassified, err
			}
			return store.EventClassAuto, nil
		}
		if ev.Class == store.EventClassJudgment {
			return store.EventClassJudgment, nil
		}
		return store.EventClassAuto, nil
	default:
		return store.EventClassJudgment, nil
	}
}

func (c *Controller) routeWorkerQuestion(ctx context.Context, ev *store.Event) (bool, error) {
	var msg mailbox.Message
	if len(ev.Payload) == 0 || json.Unmarshal(ev.Payload, &msg) != nil || !msg.Question || ev.LaneID == "" {
		return false, nil
	}
	if c.cfg.AnswerQuestion == nil && strings.TrimSpace(msg.ID) != "" {
		return false, nil
	}
	ln, err := c.store.Lanes().Get(ctx, c.cfg.RunID, ev.LaneID)
	if err != nil {
		return false, nil
	}
	if ln.Status.Terminal() {
		return true, nil
	}
	notes, err := c.store.Notes().List(ctx, store.NoteFilter{RunID: c.cfg.RunID})
	if err != nil {
		return false, err
	}
	var contextParts []string
	for _, note := range notes {
		if note.TaskID == "" || note.TaskID == ln.ID || (ln.ItemID != "" && note.TaskID == ln.ItemID) {
			contextParts = append(contextParts, note.Text)
		}
	}
	if ln.ItemID != "" {
		item, e := c.store.Backlog().Get(ctx, c.cfg.RunID, ln.ItemID)
		if e != nil && !errors.Is(e, store.ErrNotFound) {
			return false, e
		}
		if item != nil {
			contextParts = append(contextParts, item.Description, item.Acceptance)
		}
	}
	questionKey := "worker-answer:" + ev.ID
	if c.hasActionDecision(questionKey) {
		return true, nil
	}
	var answer string
	if c.cfg.AnswerQuestion != nil && !c.hasActionDecision(questionKey) {
		answer, err = c.cfg.AnswerQuestion(ctx, msg.Body, strings.Join(contextParts, "\n"))
		if err != nil {
			return false, err
		}
		answer = strings.TrimSpace(answer)
	}
	if answer != "" {
		if c.mailbox != nil && strings.TrimSpace(msg.ID) != "" {
			if err := mailbox.Respond(ctx, c.mailbox, msg, answer); err != nil {
				return false, err
			}
		}
		_ = mailbox.MarkAnswered(ctx, c.store, c.cfg.RunID, ln.ID, msg.ID, ev.ID, answer)
		if _, err := c.store.Notes().Append(ctx, store.RunNote{ID: "worker-answer-" + ev.ID, RunID: c.cfg.RunID, TaskID: cmp.Or(ln.ItemID, ln.ID), Text: "Question: " + msg.Body + "\nAnswer: " + answer}); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			return false, err
		}
		if err := c.steerCurrentTerminal(ctx, ln, "Use the task text and notes:\n"+answer); err != nil {
			var undelivered *lane.UndeliveredError
			if !errors.As(err, &undelivered) {
				return false, laneDriverError(err)
			}
			if err := c.recordUndeliveredSteer(ctx, ln, questionKey, "worker.question.answer", "worker answer submitted but not confirmed", DecisionArgs{EventID: ev.ID}, err); err != nil {
				return false, err
			}
			return true, nil
		}
		_, _, err := c.recordDecision(ctx, "worker.question.answer", ln.ID, msg.Body, answer, DecisionArgs{ActionKey: questionKey, EventID: ev.ID})
		return true, err
	}
	if c.mailbox == nil {
		return false, nil
	}
	// Reuse the durable ask on event retry, including a failed default Steer.
	dirs, err := c.store.Directives().List(ctx, store.DirectiveFilter{RunID: c.cfg.RunID})
	if err != nil {
		return false, err
	}
	var ask OwnerAsk
	for i := range dirs {
		dir := &dirs[i]
		var candidate OwnerAsk
		if strings.HasPrefix(dir.Ref, ownerAskRefPrefix) && json.Unmarshal([]byte(dir.Text), &candidate) == nil && candidate.SourceEventID == ev.ID {
			ask = candidate
			break
		}
	}
	if ask.ID == "" {
		q := mailbox.Question{Ask: msg.Body, Options: []string{"continue with task notes", "pause"}, Default: "continue with task notes", Deadline: c.clock.Now().UTC().Add(c.cfg.StallTimeout)}
		id, err := c.mailbox.AskOwner(ctx, q)
		if err != nil {
			return false, err
		}
		if id == "" {
			return false, errors.New("mailbox returned empty owner ask ID")
		}
		ask = OwnerAsk{ID: id, LaneID: ln.ID, Question: q.Ask, Options: q.Options, Default: q.Default, Deadline: q.Deadline, SourceEventID: ev.ID, SourceMessageID: strings.TrimSpace(msg.ID)}
		if err := c.registerOwnerAsk(ctx, ask, false); err != nil {
			return false, err
		}
	}
	defaultKey := questionKey
	if c.hasActionDecision(defaultKey) {
		return true, nil
	}
	err = c.steerCurrentTerminal(ctx, ln, "Continue with the default: "+ask.Default+". The owner question is pending.")
	if err != nil {
		var undelivered *lane.UndeliveredError
		if !errors.As(err, &undelivered) {
			return true, laneDriverError(err)
		}
		if err := c.recordUndeliveredSteer(ctx, ln, defaultKey, "worker.question.default", "worker default steer submitted but not confirmed", DecisionArgs{EventID: ev.ID}, err); err != nil {
			return true, err
		}
	}
	return true, nil
}

func (c *Controller) handleLaneReport(ctx context.Context, ev *store.Event) (store.EventClass, error) {
	if ev.LaneID == "" {
		return store.EventClassJudgment, nil
	}
	ln, err := c.store.Lanes().Get(ctx, c.cfg.RunID, ev.LaneID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.EventClassJudgment, nil
		}
		return store.EventClassUnclassified, err
	}
	if ln.Status.Terminal() {
		return store.EventClassAuto, nil
	}
	if ln.HostHoldReason != "" || ln.HostHold {
		return store.EventClassJudgment, nil
	}

	rep, err := c.extractReport(ctx, ln, *ev)
	if err != nil {
		return store.EventClassJudgment, nil
	}
	if rep.TerminalID != "" && ln.TerminalID != "" && rep.TerminalID != ln.TerminalID {
		return store.EventClassAuto, nil
	}
	if rep.Outcome == lane.OutcomeNeedsInput || rep.Outcome == lane.OutcomeBlocked {
		ev.Urgent = true
		question := rep.QuestionText()
		if ln.DuplicateQuestion(question, c.clock.Now()) {
			_, _, err := c.recordDecision(ctx, "lane.question.duplicate", ln.ID, "same question pending or settling", "suppressed duplicate question", DecisionArgs{EventID: ev.ID})
			return store.EventClassAuto, err
		}
		ln.Question = question
		ln.AnswerAttemptedAt, ln.AnswerConfirmedAt = time.Time{}, time.Time{}
	} else {
		ln.Question = ""
	}
	if !ln.AwaitingInputSince.IsZero() {
		ln.AwaitingInputSince = time.Time{}
		if err := c.store.Lanes().Upsert(ctx, *ln); err != nil {
			return store.EventClassUnclassified, err
		}
	}
	// A report is the terminal's explicit handback. The imported host
	// terminal_state may still say working until the host ledger catches up.
	ln.TerminalState = ""
	if strings.TrimSpace(rep.Summary) != "" {
		_, err := c.store.Notes().Append(ctx, store.RunNote{ID: "report-" + ev.ID, RunID: c.cfg.RunID, TaskID: cmp.Or(ln.ItemID, ln.ID), Phase: rep.Phase, Text: rep.Summary})
		if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			return store.EventClassUnclassified, err
		}
	}

	if rep.Phase != "" && ln.Phase != "" {
		repRound := rep.Round
		if repRound <= 0 {
			repRound = ln.Round
		}
		if lane.PhaseRoundKey(rep.Phase, repRound) != ln.AttemptKey() {
			return store.EventClassJudgment, nil
		}
	}

	now := c.clock.Now().UTC()
	ln.LastEventID = ev.ID
	ln.LastEventAt = now
	if rep.PR > 0 {
		ln.PR = rep.PR
	}
	if rep.HeadSHA != "" {
		ln.PRHead = rep.HeadSHA
	}
	if rep.Checks != "" {
		ln.Checks = rep.Checks
	} else if rep.CIGreen != nil {
		ln.Checks = boolToChecks(*rep.CIGreen)
	}

	r, err := c.resolveRecipe(ctx, ln)
	if err != nil {
		return store.EventClassUnclassified, err
	}

	curPhase, hasPhase := r.Phase(ln.Phase)
	if hasPhase && (rep.Outcome == lane.OutcomeNeedsInput || rep.Outcome == lane.OutcomeBlocked) && isWrongRepoReport(rep, ev.Summary, c.runRepository(ctx)) {
		if replayed, allowed := c.canRespawnWrongRepo(ln, ev.ID); replayed {
			return store.EventClassAuto, nil
		} else if allowed {
			return c.respawnWrongRepoLane(ctx, ln, r, curPhase, ev.ID, cmp.Or(rep.Reason, rep.Summary, ev.Summary))
		}
	}
	if hasPhase && len(curPhase.ExitContract.AllowedOutcomes) > 0 {
		noChangeAllowed := rep.Outcome == lane.OutcomeNoChange && (curPhase.Name == recipe.PhaseSWE || curPhase.Name == recipe.PhaseClean || curPhase.Name == recipe.PhaseReview)
		if !noChangeAllowed && !slices.Contains(curPhase.ExitContract.AllowedOutcomes, string(rep.Outcome)) {
			return c.blockLaneForJudgment(ctx, ln)
		}
	}

	if curPhase.Name == recipe.PhaseSWE && rep.PR == 0 && ln.PR == 0 && (rep.Outcome == lane.OutcomeNoChange || strings.EqualFold(strings.TrimSpace(rep.Change), "none") || rep.Outcome == lane.OutcomeDone) {
		// A host run can prove a report-only lane without a local checkout or
		// GitHub compare: the terminal's HEAD is the immutable base returned by
		// the spawn response.
		if rep.Outcome == lane.OutcomeDone && strings.TrimSpace(rep.HeadSHA) != "" && strings.EqualFold(strings.TrimSpace(rep.HeadSHA), strings.TrimSpace(ln.BaseCommit)) {
			return c.completeReportOnlyLane(ctx, ln, ev)
		}
		if rep.Outcome == lane.OutcomeNoChange || strings.EqualFold(strings.TrimSpace(rep.Change), "none") || rep.Outcome == lane.OutcomeDone {
			if checker, ok := c.driver.(lane.ReportOnlyChecker); ok {
				noWork, err := checker.ReportOnlyNoWork(ctx, *ln)
				if err == nil && noWork {
					return c.completeReportOnlyLane(ctx, ln, ev)
				}
			}
		}
	}

	if (rep.Outcome == lane.OutcomeDone || rep.Outcome == lane.OutcomeNoChange) && isReviewPhase(curPhase) {
		ln.ApprovalSHA = cmp.Or(rep.HeadSHA, ln.PRHead, ln.ApprovalSHA)
	}

	verdict := string(rep.Outcome)
	// no-change is a successful handback for mechanical phases; follow the
	// recipe's normal done transition after preserving the reported reason.
	if rep.Outcome == lane.OutcomeNoChange && (curPhase.Name == recipe.PhaseSWE || curPhase.Name == recipe.PhaseClean || curPhase.Name == recipe.PhaseReview) {
		verdict = string(lane.OutcomeDone)
	}
	return c.advanceByRecipe(ctx, ln, r, verdict, rep.Reason, rep.Breaks, ev)
}

func (c *Controller) completeReportOnlyLane(ctx context.Context, ln *lane.Lane, ev *store.Event) (store.EventClass, error) {
	if ln == nil || c.laneIsWorkingOrBusy(ctx, *ln) {
		return store.EventClassJudgment, nil
	}
	_, already, err := c.recordDecision(ctx, ToolLaneComplete, ln.ID,
		"SWE reported no change and no remote work", "lane done (report-only)",
		DecisionArgs{ActionKey: "report-only-complete:" + ln.ID + ":" + ln.AttemptKey(), EventID: ev.ID, FromPhase: ln.Phase, ToPhase: recipe.TargetComplete, Verdict: string(lane.OutcomeDone), Round: max(ln.Round, 1)})
	if err != nil {
		return store.EventClassUnclassified, err
	}
	if !already {
		ln.Status = lane.StatusDone
		ln.LastEventID = ev.ID
		ln.LastEventAt = c.clock.Now().UTC()
		if err := c.store.Lanes().Upsert(ctx, *ln); err != nil {
			return store.EventClassUnclassified, err
		}
		c.tryRelease(ctx, ln)
		if ln.ItemID != "" {
			if item, err := c.store.Backlog().Get(ctx, c.cfg.RunID, ln.ItemID); err == nil && !item.Status.Terminal() {
				if err := c.store.Backlog().Close(ctx, c.cfg.RunID, ln.ItemID, store.BacklogDone, ActorController, "lane completed report-only"); err != nil {
					return store.EventClassUnclassified, err
				}
			}
		}
	}
	return store.EventClassAuto, nil
}

// tryRelease never changes a completed lane back to a working state. Failed
// releases are recorded and retried from the next controller tick.
func (c *Controller) tryRelease(ctx context.Context, ln *lane.Lane) {
	if err := c.releaseLaneSession(ctx, ln); err != nil {
		key := ln.ID + ":" + ln.AttemptKey()
		c.releaseRetries[key]++
		_, _, _ = c.recordDecision(ctx, ToolLaneComplete+".release", ln.ID,
			"lane completed but session release failed", fmt.Sprintf("release retry %d: %v", c.releaseRetries[key], err),
			DecisionArgs{ActionKey: "release-failure:" + key, Phase: ln.Phase, Round: max(ln.Round, 1)})
	}
}

func (c *Controller) retryReleases(ctx context.Context) {
	if len(c.releaseRetries) == 0 {
		return
	}
	for key := range c.releaseRetries {
		parts := strings.SplitN(key, ":", 2)
		if len(parts) != 2 {
			delete(c.releaseRetries, key)
			continue
		}
		ln, err := c.store.Lanes().Get(ctx, c.cfg.RunID, parts[0])
		if err != nil || ln.Status != lane.StatusDone {
			continue
		}
		if err := c.releaseLaneSession(ctx, ln); err == nil {
			delete(c.releaseRetries, key)
		} else if c.releaseRetries[key] >= 3 {
			_, _, _ = c.recordDecision(ctx, ToolLaneComplete+".release", ln.ID,
				"lane session release exhausted retries", "release escalation recorded",
				DecisionArgs{ActionKey: "release-exhausted:" + key, Phase: ln.Phase, Round: max(ln.Round, 1)})
			delete(c.releaseRetries, key)
		} else {
			c.releaseRetries[key]++
		}
	}
}

func (c *Controller) blockLaneForJudgment(ctx context.Context, ln *lane.Lane) (store.EventClass, error) {
	ln.Status = lane.StatusBlocked
	if err := c.store.Lanes().Upsert(ctx, *ln); err != nil {
		return store.EventClassUnclassified, err
	}
	return store.EventClassJudgment, nil
}

func (c *Controller) escalateLane(ctx context.Context, ln *lane.Lane, reason, result string, args DecisionArgs) (store.EventClass, error) {
	_, already, err := c.recordDecision(ctx, ToolLaneEscalate, ln.ID, reason, result, args)
	if err != nil {
		return store.EventClassUnclassified, err
	}
	if !already {
		ln.Status = lane.StatusBlocked
		if err := c.store.Lanes().Upsert(ctx, *ln); err != nil {
			return store.EventClassUnclassified, err
		}
	}
	return store.EventClassJudgment, nil
}

// ReconcileLanePRState checks whether ln's pull request is already MERGED or
// CLOSED on GitHub. When it is, the lane's terminals/session are stopped, the
// lane is persisted as done or failed, and its backlog item is closed.
func ReconcileLanePRState(
	ctx context.Context,
	probe PRProbe,
	st store.Store,
	drv lane.LaneDriver,
	now time.Time,
	runID, actor, eventID string,
	ln *lane.Lane,
) (settled bool, state string, err error) {
	if probe == nil || ln == nil || (ln.PR <= 0 && strings.TrimSpace(ln.Branch) == "") {
		return false, "", nil
	}
	prState, err := probe(ctx, runID, ln.Branch, ln.PR)
	if err != nil {
		slog.WarnContext(ctx, "probe lane PR state failed", "run_id", runID, "lane_id", ln.ID, "pr", ln.PR, "branch", ln.Branch, "error", err)
		return false, "", nil
	}
	if prState.Number > 0 && ln.PR <= 0 {
		ln.PR = prState.Number
	}
	state = strings.ToUpper(strings.TrimSpace(prState.State))
	switch state {
	case "MERGED", "CLOSED":
	default:
		return false, "", nil
	}

	if drv != nil {
		if strings.TrimSpace(ln.TerminalID) != "" || strings.TrimSpace(ln.SessionID) != "" {
			_ = drv.Stop(ctx, lane.StopRequest{
				RunID:      runID,
				LaneID:     ln.ID,
				SessionID:  ln.SessionID,
				TerminalID: ln.TerminalID,
				Reason:     "pr " + strings.ToLower(state),
			})
		}
	}
	if drv != nil {
		if r, ok := drv.(lane.SessionReleaser); ok && strings.TrimSpace(ln.SessionID) != "" {
			_ = r.ReleaseSession(ctx, ln.SessionID)
		}
	}

	if state == "MERGED" {
		ln.Status = lane.StatusDone
		ln.MergeSHA = cmp.Or(strings.TrimSpace(prState.MergeSHA), ln.MergeSHA, ln.PRHead)
	} else {
		ln.Status = lane.StatusFailed
	}
	if eventID != "" {
		ln.LastEventID = eventID
	}
	ln.LastEventAt = now.UTC()
	if err := st.Lanes().Upsert(ctx, *ln); err != nil {
		return true, state, err
	}

	itemID := cmp.Or(ln.ItemID, ln.ID)
	if itemID != "" {
		if item, err := st.Backlog().Get(ctx, runID, itemID); err == nil && !item.Status.Terminal() {
			backlogStatus := store.BacklogDone
			if state == "CLOSED" {
				backlogStatus = store.BacklogDropped
			}
			_ = st.Backlog().Close(ctx, runID, itemID, backlogStatus, actor, fmt.Sprintf("PR #%d %s", ln.PR, strings.ToLower(state)))
		}
	}
	return true, state, nil
}

func (c *Controller) reconcileLanePRState(ctx context.Context, ln *lane.Lane, eventID string) (bool, error) {
	if ln == nil || ln.Status.Terminal() || c.laneIsWorkingOrBusy(ctx, *ln) {
		return false, nil
	}
	settled, state, err := ReconcileLanePRState(ctx, c.cfg.PRProbe, c.store, nil, c.clock.Now(), c.cfg.RunID, ActorController, eventID, ln)
	if !settled {
		return false, err
	}
	if stopErr := c.stopCurrentTerminal(ctx, ln, "pr_"+strings.ToLower(state)); stopErr != nil {
		c.noteStopFailure(ctx, ln, stopErr)
	}
	c.tryRelease(ctx, ln)
	delete(c.pendingVerdicts, ln.ID)
	delete(c.pendingStops, ln.ID+":"+ln.AttemptKey())
	if err != nil {
		return true, err
	}

	round := max(ln.Round, 1)
	actionKey := fmt.Sprintf("pr-state:%s:%d:%s", ln.ID, ln.PR, strings.ToLower(state))
	result := "lane done (pr merged)"
	if state == "CLOSED" {
		result = "lane failed (pr closed)"
	}
	_, _, _ = c.recordDecision(ctx, ToolLaneComplete, ln.ID,
		fmt.Sprintf("PR #%d is %s on GitHub", ln.PR, state),
		result,
		DecisionArgs{
			ActionKey: actionKey,
			EventID:   eventID,
			FromPhase: ln.Phase,
			ToPhase:   recipe.TargetComplete,
			Verdict:   strings.ToLower(state),
			Round:     round,
		},
	)
	return true, nil
}

func (c *Controller) advanceByRecipe(
	ctx context.Context,
	ln *lane.Lane,
	r *recipe.Recipe,
	verdict, reason, breaks string,
	ev *store.Event,
) (store.EventClass, error) {
	eventID := ""
	if ev != nil {
		eventID = ev.ID
	}
	if settled, err := c.reconcileLanePRState(ctx, ln, eventID); settled || err != nil {
		if err != nil {
			return store.EventClassUnclassified, err
		}
		return store.EventClassAuto, nil
	}
	round := max(ln.Round, 1)
	noMergeLane := c.isNoMergeLane(ctx, ln)
	ciGreen := isCIGreen(ln.Checks)

	mech, judg := r.MatchTransitions(ln.Phase, verdict, ciGreen, round)
	if len(judg) > 0 {
		if err := c.store.Lanes().Upsert(ctx, *ln); err != nil {
			return store.EventClassUnclassified, err
		}
		return store.EventClassJudgment, nil
	}

	if mech == nil {
		// Hold the verdict if a transition only awaits green CI.
		if !ciGreen && !isCIRed(ln.Checks) {
			mechWithCI, judgWithCI := r.MatchTransitions(ln.Phase, verdict, true, round)
			if mechWithCI != nil || len(judgWithCI) > 0 {
				c.pendingVerdicts[ln.ID] = pendingVerdict{
					key:     ln.AttemptKey(),
					verdict: verdict,
					headSHA: ln.PRHead,
				}
				if err := c.store.Lanes().Upsert(ctx, *ln); err != nil {
					return store.EventClassUnclassified, err
				}
				if isCIRed(ln.Checks) {
					return store.EventClassJudgment, nil
				}
				return store.EventClassAuto, nil
			}
		}
		return c.blockLaneForJudgment(ctx, ln)
	}

	delete(c.pendingVerdicts, ln.ID)

	noMerge := r.MergePolicy.Mode == recipe.MergeModeNone || noMergeLane
	target := mech.To
	if target == recipe.PhaseIntegrate && noMerge {
		target = recipe.TargetComplete
	}

	switch target {
	case recipe.TargetComplete:
		if c.laneIsWorkingOrBusy(ctx, *ln) {
			return store.EventClassJudgment, nil
		}
		if r.MergePolicy.RequireReviewPass && recipeHasReviewPhase(r) {
			if (ln.PRHead != "" || ln.ApprovalSHA != "") && ln.ApprovalStale() {
				return c.blockLaneForJudgment(ctx, ln)
			}
		}
		if r.MergePolicy.RequireCIGreen && !noMerge && !ciGreen && strings.TrimSpace(ln.MergeSHA) == "" {
			return c.blockLaneForJudgment(ctx, ln)
		}

		actionKey := integrateOnceActionKey(ln.ID, ln.Phase, round, ln.PRHead)
		_, already, err := c.recordDecision(ctx, ToolLaneComplete, ln.ID,
			fmt.Sprintf("phase %s completed with verdict %s", ln.Phase, verdict),
			"lane done",
			DecisionArgs{
				ActionKey: actionKey,
				EventID:   ev.ID,
				FromPhase: ln.Phase,
				ToPhase:   recipe.TargetComplete,
				Verdict:   verdict,
				Round:     round,
			},
		)
		if err != nil {
			return store.EventClassUnclassified, err
		}
		if !already {
			ln.Status = lane.StatusDone
			if !noMerge {
				ln.MergeSHA = cmp.Or(ln.MergeSHA, ln.PRHead)
			}
			if err := c.store.Lanes().Upsert(ctx, *ln); err != nil {
				return store.EventClassUnclassified, err
			}
			c.tryRelease(ctx, ln)
			if ln.ItemID != "" {
				if item, err := c.store.Backlog().Get(ctx, c.cfg.RunID, ln.ItemID); err == nil && !item.Status.Terminal() {
					if err := c.store.Backlog().Close(ctx, c.cfg.RunID, ln.ItemID, store.BacklogDone, ActorController, "lane completed recipe"); err != nil {
						return store.EventClassUnclassified, err
					}
				}
			}
		}
		return store.EventClassAuto, nil

	case recipe.TargetEscalate:
		return c.escalateLane(ctx, ln,
			fmt.Sprintf("escalated from %s on verdict %s at round %d", ln.Phase, verdict, round),
			"escalated to judgment",
			DecisionArgs{
				ActionKey: escalateActionKey(ln.ID, ln.Phase, round, ev.ID),
				EventID:   ev.ID,
				FromPhase: ln.Phase,
				ToPhase:   recipe.TargetEscalate,
				Verdict:   verdict,
				Round:     round,
			},
		)

	default:
		targetPhase, ok := r.Phase(mech.To)
		if !ok {
			return store.EventClassUnclassified, fmt.Errorf("orca/controller: unknown target phase %q in recipe %q", mech.To, r.Name)
		}
		if hasGate(targetPhase, "review_approved") && r.MergePolicy.RequireReviewPass && recipeHasReviewPhase(r) {
			if (ln.PRHead != "" || ln.ApprovalSHA != "") && ln.ApprovalStale() {
				return c.blockLaneForJudgment(ctx, ln)
			}
		}

		nextRound := round
		tool := ToolPhaseAdvance
		if mech.AdvanceRound {
			nextRound = max(round+1, ln.MaxRound(targetPhase.Name)+1)
			tool = ToolPhaseRework
		} else if recorded := ln.MaxRound(targetPhase.Name); recorded >= nextRound {
			nextRound = recorded + 1
		}

		if r.Limits.MaxRounds > 0 && nextRound > r.Limits.MaxRounds {
			return c.escalateLane(ctx, ln,
				fmt.Sprintf("rework to %s round %d exceeds max_rounds %d", targetPhase.Name, nextRound, r.Limits.MaxRounds),
				"round limit exceeded",
				DecisionArgs{
					ActionKey: escalateActionKey(ln.ID, ln.Phase, round, ev.ID),
					EventID:   ev.ID,
					FromPhase: ln.Phase,
					ToPhase:   recipe.TargetEscalate,
					Verdict:   verdict,
					Round:     round,
				},
			)
		}

		note := reason
		if breaks != "" {
			if note != "" {
				note += "; breaks: " + breaks
			} else {
				note = "breaks: " + breaks
			}
		}
		trReason := fmt.Sprintf("%s -> %s on verdict %s (round %d)", ln.Phase, targetPhase.Name, verdict, nextRound)
		if err := c.startLanePhase(ctx, ln, r, targetPhase, nextRound, 1, tool, ev.ID, trReason, note); err != nil {
			return store.EventClassUnclassified, err
		}
		return store.EventClassAuto, nil
	}
}

func (c *Controller) handlePhaseExit(ctx context.Context, ev *store.Event) (store.EventClass, error) {
	if ev.LaneID == "" {
		return store.EventClassAuto, nil
	}
	ln, err := c.store.Lanes().Get(ctx, c.cfg.RunID, ev.LaneID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.EventClassJudgment, nil
		}
		return store.EventClassUnclassified, err
	}
	if ln.Status.Terminal() {
		return store.EventClassAuto, nil
	}
	if ln.HostHoldReason != "" || ln.HostHold {
		return store.EventClassJudgment, nil
	}
	if ln.Status != lane.StatusRunning {
		return store.EventClassAuto, nil
	}

	var payload PhaseExitPayload
	if len(ev.Payload) > 0 {
		_ = json.Unmarshal(ev.Payload, &payload)
	}
	if payload.TerminalID != "" && ln.TerminalID != "" && payload.TerminalID != ln.TerminalID {
		// Stale exit for an earlier terminal that already advanced.
		return store.EventClassAuto, nil
	}
	if terminalID := strings.TrimSpace(payload.TerminalID); terminalID != "" && c.stoppedTerminals[terminalID] {
		// Orca stopped this live terminal as part of a deliberate handover or
		// retry. Its exit is not evidence that the worker failed an attempt.
		return store.EventClassAuto, nil
	}
	if payload.Kind == lane.EventWaitingForInput {
		if lane.WorkingInputWaiting(payload.Error) {
			return store.EventClassAuto, nil
		}
		tail := waitingInputTail(payload.Error)
		termID := cmp.Or(strings.TrimSpace(payload.TerminalID), strings.TrimSpace(ln.TerminalID))
		ev.ID = waitingInputEventID(ln.ID, termID, tail)
		// A replay of an answered question must not restore the waiting marker.
		if c.isEventHandled(ev.ID) {
			return store.EventClassAuto, nil
		}
		ev.Summary = waitingInputSummary(ln.ID, tail)
		ev.Urgent = true
		if ln.AwaitingInputSince.IsZero() {
			ln.AwaitingInputSince = c.clock.Now().UTC()
		}
		if err := c.store.Lanes().Upsert(ctx, *ln); err != nil {
			return store.EventClassUnclassified, err
		}
		return store.EventClassJudgment, nil
	}
	if !ln.AwaitingInputSince.IsZero() && (payload.Kind == lane.EventNoActivity || payload.Kind == lane.EventTurnEndedNoReport) {
		return store.EventClassAuto, nil
	}
	if payload.Kind == lane.EventTurnEndedNoReport || payload.Kind == lane.EventExited {
		// The host ledger can lag the terminal handback. An explicit ended event
		// supersedes its imported working marker; a live driver state below still
		// vetoes completion or release when the agent is actually busy.
		ln.TerminalState = ""
	}
	if settled, err := c.reconcileLanePRState(ctx, ln, ev.ID); settled || err != nil {
		if err != nil {
			return store.EventClassUnclassified, err
		}
		return store.EventClassAuto, nil
	}

	termID := cmp.Or(payload.TerminalID, ln.TerminalID)
	if payload.ExitCode == 0 && (payload.Error == "" || payload.Kind == lane.EventTurnEndedNoReport || payload.Kind == lane.EventExited) {
		if termID != "" && c.driver != nil {
			if rep, err := c.driver.ReadReport(ctx, c.cfg.RunID, ln.ID, termID); err == nil && rep != nil && rep.Outcome.Valid() {
				answeredQuestion := (rep.Outcome == lane.OutcomeNeedsInput || rep.Outcome == lane.OutcomeBlocked) && ln.Question == rep.QuestionText() && !ln.AnswerConfirmedAt.IsZero()
				if !answeredQuestion {
					return c.handleLaneReport(ctx, ev)
				}
			}
		}
	}

	r, err := c.resolveRecipe(ctx, ln)
	if err != nil {
		return store.EventClassUnclassified, err
	}
	curPhase, ok := r.Phase(ln.Phase)
	if !ok {
		return store.EventClassJudgment, nil
	}

	maxAttempts := r.Limits.MaxPhaseAttempts
	if maxAttempts <= 0 {
		maxAttempts = max(len(curPhase.Agents), 1)
	}
	curAttempt := max(ln.Attempt, 1)
	ln.LastEventID = ev.ID
	// A quiet pulse is controller bookkeeping, not worker activity.
	quiet := ev.Source == "lane.watch" && (payload.Kind == lane.EventNoActivity || payload.Kind == lane.EventTurnEndedNoReport)
	now := c.clock.Now().UTC()
	if quiet {
		if c.hasActiveExpectedWait(ctx, ln.ID, now) {
			return store.EventClassAuto, nil
		}
		if payload.Kind == lane.EventNoActivity && (c.terminalIsWorkingOrUnknown(ctx, ln.TerminalID) || c.sessionIsBusy(ctx, *ln)) {
			return store.EventClassAuto, nil
		}
		if payload.Kind == lane.EventTurnEndedNoReport && c.sessionIsBusy(ctx, *ln) {
			return store.EventClassAuto, nil
		}
		if payload.Kind == lane.EventNoActivity {
			if _, waiting := c.quietLadderRungFromStore(ctx, ln, r, now); waiting {
				return store.EventClassAuto, nil
			}
		}
	} else {
		ln.LastEventAt = now
	}
	if quiet {
		key := c.quietLaneKey(ln)
		switch {
		case !c.hasActionDecision("restart:nudge:" + key):
			if _, _, err := c.recordDecision(ctx, ToolPhaseRetry+".intent", ln.ID, "quiet lane: nudge before restart", "effect pending", DecisionArgs{ActionKey: "intent:restart:nudge:" + key, Phase: ln.Phase, Round: max(ln.Round, 1), Attempt: curAttempt}); err != nil {
				return store.EventClassUnclassified, err
			}

			if err := c.steerCurrentTerminal(ctx, ln, "Continue and file the required phase report."); err != nil {
				var undelivered *lane.UndeliveredError
				if !errors.As(err, &undelivered) {
					return store.EventClassUnclassified, laneDriverError(err)
				}
				ln.AwaitingInputSince = now
				ln.AnswerAttemptedAt, ln.AnswerConfirmedAt = now, time.Time{}
				if saveErr := c.store.Lanes().Upsert(ctx, *ln); saveErr != nil {
					return store.EventClassUnclassified, saveErr
				}
				failure, saveErr := c.store.Events().Append(ctx, store.Event{ID: "undelivered-" + ev.ID, RunID: c.cfg.RunID, LaneID: ln.ID, Kind: store.EventStall, Class: store.EventClassJudgment, Source: lane.AnswerDeliverySource, Urgent: true, Summary: err.Error()})
				if saveErr != nil {
					return store.EventClassUnclassified, saveErr
				}
				c.enqueueJudgmentEvent(failure)
				_, _, saveErr = c.recordDecision(ctx, ToolPhaseRetry, ln.ID, "quiet lane: nudge before restart", err.Error(), DecisionArgs{ActionKey: "restart:nudge:" + key, EventID: ev.ID, Phase: ln.Phase, Round: max(ln.Round, 1), Attempt: curAttempt})
				return store.EventClassAuto, saveErr
			}
			if err := c.store.Lanes().Upsert(ctx, *ln); err != nil {
				return store.EventClassUnclassified, err
			}
			_, _, err := c.recordDecision(ctx, ToolPhaseRetry, ln.ID, "quiet lane: nudge before restart", "nudged terminal", DecisionArgs{ActionKey: "restart:nudge:" + key, EventID: ev.ID, Phase: ln.Phase, Round: max(ln.Round, 1), Attempt: curAttempt})
			if err != nil {
				return store.EventClassUnclassified, err
			}
			return store.EventClassAuto, nil
		case !c.hasActionDecision("restart:fresh:" + key):
			if _, _, err := c.recordDecision(ctx, ToolPhaseRetry+".intent", ln.ID, "quiet lane: fresh terminal same round", "effect pending", DecisionArgs{ActionKey: "intent:restart:fresh:" + key, Phase: ln.Phase, Round: max(ln.Round, 1), Attempt: curAttempt}); err != nil {
				return store.EventClassUnclassified, err
			}

			if len(curPhase.Agents) == 0 {
				return store.EventClassUnclassified, fmt.Errorf("orca/controller: phase %q in recipe %q has no agents", curPhase.Name, r.Name)
			}

			agent := curPhase.Agents[min(curAttempt-1, len(curPhase.Agents)-1)]
			prevTermID := strings.TrimSpace(ln.TerminalID)
			if err := c.stopCurrentTerminal(ctx, ln, "replace quiet terminal"); err != nil {
				c.noteStopFailure(ctx, ln, err)
				return store.EventClassUnclassified, err
			}
			delete(c.pendingStops, ln.ID+":"+ln.AttemptKey())
			var stoppedTerminals []string
			if prevTermID != "" {
				stoppedTerminals = []string{prevTermID}
			}
			res, err := c.driver.StartPhaseTerminal(ctx, lane.StartPhaseRequest{RunID: c.cfg.RunID, Repository: c.runRepository(ctx), LaneID: ln.ID, SessionID: ln.SessionID, Phase: curPhase.Name, Round: max(ln.Round, 1), Harness: agent.Harness, Model: agent.Model, Effort: agent.Effort, Brief: c.buildBrief(ctx, ln, r, curPhase, max(ln.Round, 1), "fresh terminal after nudge")})
			if err != nil {
				return store.EventClassUnclassified, laneDriverError(err)
			}
			ln.SessionID, ln.TerminalID = res.SessionID, res.TerminalID
			if err := c.store.Lanes().Upsert(ctx, *ln); err != nil {
				return store.EventClassUnclassified, err
			}
			_, _, err = c.recordDecision(ctx, ToolPhaseRetry, ln.ID, "quiet lane: fresh terminal same round", "fresh terminal", DecisionArgs{ActionKey: "restart:fresh:" + key, EventID: ev.ID, Phase: ln.Phase, Round: max(ln.Round, 1), Attempt: curAttempt, StoppedTerminals: stoppedTerminals})
			if err != nil {
				return store.EventClassUnclassified, err
			}
			return store.EventClassAuto, nil
		}
	}

	if curAttempt < maxAttempts {
		nextAttempt := curAttempt + 1
		reason := fmt.Sprintf("phase %s attempt %d exited without report; retrying attempt %d via fallback chain", curPhase.Name, curAttempt, nextAttempt)
		if err := c.startLanePhase(ctx, ln, r, curPhase, max(ln.Round, 1), nextAttempt, ToolPhaseRetry, ev.ID, reason, payload.Error); err != nil {
			return store.EventClassUnclassified, err
		}
		return store.EventClassAuto, nil
	}

	return c.escalateLane(ctx, ln,
		fmt.Sprintf("phase %s exhausted %d attempts", curPhase.Name, maxAttempts),
		"attempts exhausted",
		DecisionArgs{
			ActionKey: escalateActionKey(ln.ID, ln.Phase, max(ln.Round, 1), ev.ID),
			EventID:   ev.ID,
			Phase:     curPhase.Name,
			Round:     max(ln.Round, 1),
			Attempt:   curAttempt,
		},
	)
}

func (c *Controller) handleCI(ctx context.Context, ev *store.Event) (store.EventClass, error) {
	if ev.LaneID == "" {
		return store.EventClassAuto, nil
	}
	ln, err := c.store.Lanes().Get(ctx, c.cfg.RunID, ev.LaneID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.EventClassAuto, nil
		}
		return store.EventClassUnclassified, err
	}
	if ln.Status.Terminal() {
		return store.EventClassAuto, nil
	}
	if ln.HostHoldReason != "" || ln.HostHold {
		return store.EventClassJudgment, nil
	}
	var payload CIPayload
	if len(ev.Payload) > 0 {
		_ = json.Unmarshal(ev.Payload, &payload)
	}
	now := c.clock.Now().UTC()
	ln.LastEventID = ev.ID
	ln.LastEventAt = now
	if payload.PR > 0 {
		ln.PR = payload.PR
	}
	if payload.HeadSHA != "" {
		ln.PRHead = payload.HeadSHA
	}
	switch {
	case payload.CheckName != "":
		ln.Checks = mergeNamedCheckStatus(ln.Checks, payload.CheckName, payload.Status, payload.Green)
	case payload.Status != "":
		ln.Checks = payload.Status
	case payload.Green != nil:
		ln.Checks = boolToChecks(*payload.Green)
	case strings.Contains(strings.ToLower(ev.Summary), "green") || strings.Contains(strings.ToLower(ev.Summary), "pass"):
		ln.Checks = "green"
	case strings.Contains(strings.ToLower(ev.Summary), "red") || strings.Contains(strings.ToLower(ev.Summary), "fail"):
		ln.Checks = "red"
	}

	if err := c.store.Lanes().Upsert(ctx, *ln); err != nil {
		return store.EventClassUnclassified, err
	}

	ciGreen := isCIGreen(ln.Checks)
	if ciGreen {
		if pv, ok := c.pendingVerdictForLane(ctx, ln); ok {
			r, err := c.resolveRecipe(ctx, ln)
			if err != nil {
				return store.EventClassUnclassified, err
			}
			return c.advanceByRecipe(ctx, ln, r, pv.verdict, "", "", ev)
		}
		return store.EventClassAuto, nil
	}

	if isCIRed(ln.Checks) {
		return store.EventClassJudgment, nil
	}
	return store.EventClassAuto, nil
}

func (c *Controller) pendingVerdictForLane(ctx context.Context, ln *lane.Lane) (pendingVerdict, bool) {
	if pv, ok := c.pendingVerdicts[ln.ID]; ok && pv.key == ln.AttemptKey() {
		return pv, true
	}
	if ln.TerminalID != "" {
		if rep, err := c.driver.ReadReport(ctx, c.cfg.RunID, ln.ID, ln.TerminalID); err == nil && rep != nil && rep.Outcome.Valid() {
			return pendingVerdict{
				key:     ln.AttemptKey(),
				verdict: string(rep.Outcome),
				headSHA: rep.HeadSHA,
			}, true
		}
	}
	return pendingVerdict{}, false
}

func (c *Controller) handleStall(ctx context.Context, ev *store.Event) (store.EventClass, error) {
	if ev.LaneID == "" {
		return store.EventClassJudgment, nil
	}
	ln, err := c.store.Lanes().Get(ctx, c.cfg.RunID, ev.LaneID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.EventClassJudgment, nil
		}
		return store.EventClassUnclassified, err
	}
	if ln.Status.Terminal() {
		return store.EventClassAuto, nil
	}
	if ln.HostHoldReason != "" || ln.HostHold || ev.Source == "lane.unpushed" || ev.Source == "lane.health" || ev.Source == lane.AnswerDeliverySource {
		return store.EventClassJudgment, nil
	}
	if !ln.AwaitingInputSince.IsZero() {
		return store.EventClassAuto, nil
	}
	if settled, err := c.reconcileLanePRState(ctx, ln, ev.ID); settled || err != nil {
		if err != nil {
			return store.EventClassUnclassified, err
		}
		return store.EventClassAuto, nil
	}
	now := c.clock.Now().UTC()
	if c.laneIsWorkingOrBusy(ctx, *ln) || c.hasActiveExpectedWait(ctx, ln.ID, now) {
		return store.EventClassAuto, nil
	}

	round := max(ln.Round, 1)
	attempt := max(ln.Attempt, 1)
	actionKey := stallActionKey(ln.ID, ln.Phase, round, attempt, ln.TerminalID)
	c.emittedStalls[actionKey] = true
	_, _, err = c.recordDecision(ctx, ToolLaneStall, ln.ID,
		fmt.Sprintf("lane %s stalled in phase %s (round %d, attempt %d)", ln.ID, ln.Phase, round, attempt),
		"stopped stalled terminal",
		DecisionArgs{
			ActionKey:  actionKey,
			EventID:    ev.ID,
			Phase:      ln.Phase,
			Round:      round,
			Attempt:    attempt,
			SessionID:  ln.SessionID,
			TerminalID: ln.TerminalID,
		},
	)
	if err != nil {
		return store.EventClassUnclassified, err
	}
	if ln.SessionID != "" || ln.TerminalID != "" {
		if err := c.stopCurrentTerminal(ctx, ln, "phase stall timeout exceeded"); err != nil {
			return store.EventClassUnclassified, laneDriverError(err)
		}
		if strings.TrimSpace(ln.TerminalID) != "" {
			c.stoppedTerminals[strings.TrimSpace(ln.TerminalID)] = true
		}
	}
	if ln.Status != lane.StatusBlocked {
		ln.Status = lane.StatusBlocked
		ln.LastEventID = ev.ID
		ln.LastEventAt = c.clock.Now().UTC()
		if err := c.store.Lanes().Upsert(ctx, *ln); err != nil {
			return store.EventClassUnclassified, err
		}
	}
	return store.EventClassJudgment, nil
}

func (c *Controller) handleOwnerAskExpired(ctx context.Context, ev *store.Event) (store.EventClass, error) {
	var ask OwnerAsk
	if len(ev.Payload) > 0 {
		if err := json.Unmarshal(ev.Payload, &ask); err != nil {
			return store.EventClassJudgment, nil
		}
	}
	if ask.ID == "" {
		return store.EventClassJudgment, nil
	}
	ask.LaneID = cmp.Or(ask.LaneID, ev.LaneID)

	dirID := ownerAskDirectiveID(ask.ID)
	if dir, err := c.store.Directives().Get(ctx, c.cfg.RunID, dirID); err == nil {
		if dir.Status != store.DirectiveActive {
			return store.EventClassAuto, nil
		}
	}

	actionKey := askDefaultActionKey(ask.ID)
	c.emittedAskExpiries[ask.ID] = true

	if _, _, err := c.recordDecision(ctx, ToolOwnerAskDefault+".intent", ask.LaneID, "deadline default pending", ask.Default, DecisionArgs{ActionKey: "intent:" + actionKey, AskID: ask.ID}); err != nil {
		return store.EventClassUnclassified, err
	}

	if err := c.applyOwnerAskResolution(ctx, ask, "ask-default-"+ask.ID, ask.Default, cmp.Or(ask.SteerNote, ask.Default), ev.ID); err != nil {
		return store.EventClassUnclassified, err
	}
	if err := c.store.Directives().UpdateStatus(ctx, c.cfg.RunID, dirID, store.DirectiveHandled); err != nil {
		return store.EventClassUnclassified, err
	}
	_, _, err := c.recordDecision(ctx, ToolOwnerAskDefault, ask.LaneID,
		fmt.Sprintf("owner ask %q expired at deadline; applying default %q", ask.ID, ask.Default),
		ask.Default,
		DecisionArgs{
			ActionKey: actionKey,
			EventID:   ev.ID,
			AskID:     ask.ID,
			Default:   ask.Default,
		},
	)
	return store.EventClassAuto, err
}

func (c *Controller) startLanePhase(
	ctx context.Context,
	ln *lane.Lane,
	r *recipe.Recipe,
	phase recipe.Phase,
	round, attempt int,
	tool, eventID, reason, extraNote string,
) error {
	if ln == nil || ln.Status.Terminal() {
		return nil
	}
	if settled, err := c.reconcileLanePRState(ctx, ln, eventID); settled || err != nil {
		return err
	}
	if round < 1 {
		round = 1
	}
	if attempt < 1 {
		attempt = 1
	}
	if len(phase.Agents) == 0 {
		return fmt.Errorf("orca/controller: phase %q in recipe %q has no agents", phase.Name, r.Name)
	}
	agentIdx := min(attempt-1, len(phase.Agents)-1)
	agent := phase.Agents[agentIdx]

	fromPhase := ln.Phase
	brief := c.buildBrief(ctx, ln, r, phase, round, extraNote)
	actionKey := phaseActionKey(tool, ln.ID, phase.Name, round, attempt)

	if phase.Name == recipe.PhaseIntegrate {
		actionKey = "integrate-launch:" + ln.ID + ":" + ln.ApprovalSHA
	}
	if c.hasActionDecision(actionKey) {
		return nil
	}
	if phase.Name == recipe.PhaseIntegrate {
		intentKey := "intent:" + actionKey
		recovered := c.hasActionDecision(intentKey)
		if _, _, err := c.recordDecision(ctx, "phase.launch.intent", ln.ID, reason, "integrate launch reserved", DecisionArgs{ActionKey: intentKey, Phase: phase.Name, Round: round, Attempt: attempt}); err != nil {
			return err
		}
		if recovered {
			// A durable intent means the previous controller may have died between
			// reserving and applying the launch. Re-run the launch effect with a
			// small persisted retry budget and backoff; never turn recovery into an
			// owner escalation.
			recoveries, lastRecovery, err := c.integrateRecoveryState(ctx, ln.ID, actionKey)
			if err != nil {
				return err
			}
			if recoveries >= integrateRecoveryMaxAttempts {
				_, err := c.store.Events().Append(ctx, store.Event{ID: "integrate-recovery-exhausted:" + actionKey, RunID: c.cfg.RunID, LaneID: ln.ID, Kind: store.EventStall, Class: store.EventClassJudgment, Source: ActorController, Summary: "integrate launch recovery exhausted"})
				if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
					return err
				}
				return nil
			}
			if !lastRecovery.IsZero() && c.clock.Now().Before(lastRecovery.Add(integrateRecoveryBackoff)) {
				expires := lastRecovery.Add(integrateRecoveryBackoff)
				waitID := fmt.Sprintf("%s%s:%d", integrateRecoveryRefPrefix, actionKey, recoveries)
				body, err := json.Marshal(struct {
					LaneID    string `json:"lane_id"`
					ActionKey string `json:"action_key"`
				}{ln.ID, actionKey})
				if err != nil {
					return err
				}
				if err := c.store.Directives().Add(ctx, store.Directive{ID: waitID, RunID: c.cfg.RunID, Kind: store.DirectiveInstruction, Source: store.DirectiveSourceDispatcher, Scope: ln.ID, Ref: waitID, Text: string(body), ExpiresAt: &expires, Status: store.DirectiveActive}); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
					return err
				}
				return nil
			}
			recoveryKey := fmt.Sprintf("recover:%s:%d", actionKey, recoveries+1)
			if _, _, err := c.recordDecision(ctx, "phase.launch.recovery", ln.ID, "retry interrupted integrate launch", "recovery pending", DecisionArgs{ActionKey: recoveryKey, Phase: phase.Name, Round: round, Attempt: attempt}); err != nil {
				return err
			}
			reason = "recover interrupted integrate launch: " + reason
		}
	}

	var recoveredTerminal *lane.StartPhaseResult
	recoveryResult := ""
	if intent, ok := c.decidedActions["intent:"+actionKey]; ok && phase.Name != recipe.PhaseIntegrate {
		var args DecisionArgs
		if json.Unmarshal(intent.Args, &args) == nil && args.FromPhase != "" {
			fromPhase = args.FromPhase
		}
	}
	if phase.Name != recipe.PhaseIntegrate {
		if c.hasActionDecision("intent:" + actionKey) {
			var err error
			lookup := lane.StartPhaseRequest{RunID: c.cfg.RunID, LaneID: ln.ID, SessionID: ln.SessionID, Phase: phase.Name, Round: round, ActionKey: actionKey}
			if receipt, ok := c.decidedActions["started:"+actionKey]; ok {
				var args DecisionArgs
				if err := json.Unmarshal(receipt.Args, &args); err != nil {
					return err
				}
				lookup.SessionID, lookup.KnownTerminalID = args.SessionID, args.TerminalID
			}
			recoveredTerminal, err = c.driver.FindPhaseTerminal(ctx, lookup)
			if err != nil {
				return laneDriverError(err)
			}
			if recoveredTerminal == nil {
				recoveryResult = "relaunch: driver found no live terminal for launch intent"
			} else {
				recoveryResult = "recovered live terminal for launch intent"
			}
		}
		if _, _, err := c.recordDecision(ctx, tool+".intent", ln.ID, reason, "launch pending", DecisionArgs{ActionKey: "intent:" + actionKey, TriggerEventID: eventID, FromPhase: fromPhase, Phase: phase.Name, Round: round, Attempt: attempt}); err != nil {
			return err
		}
	}

	var sessionID, terminalID string
	var stoppedTerminals []string
	repo := c.runRepository(ctx)
	if recoveredTerminal != nil {
		sessionID, terminalID = recoveredTerminal.SessionID, recoveredTerminal.TerminalID
	} else if ln.SessionID == "" {
		if tool != ToolPhaseRetry {
			stoppedTerminals = c.stopPreviousPhaseTerminals(ctx, ln, "new phase "+phase.Name+" started")
		}
		res, err := c.driver.SpawnSession(ctx, lane.SpawnRequest{
			RunID:      c.cfg.RunID,
			ActionKey:  actionKey,
			Repository: repo,
			LaneID:     ln.ID,
			Phase:      phase.Name,
			Round:      round,
			Harness:    agent.Harness,
			Model:      agent.Model,
			Effort:     agent.Effort,
			Brief:      brief,
			Branch:     ln.Branch,
			BaseBranch: ln.BaseBranch,
			Worktree:   ln.Worktree,
		})
		if err != nil {
			return &LaneLocalError{Err: err}
		}
		sessionID = res.SessionID
		terminalID = res.TerminalID
		if strings.TrimSpace(res.Branch) != "" {
			ln.Branch = strings.TrimSpace(res.Branch)
		}
		if strings.TrimSpace(res.BaseBranch) != "" {
			ln.BaseBranch = strings.TrimSpace(res.BaseBranch)
		}
		if strings.TrimSpace(res.BaseCommit) != "" {
			ln.BaseCommit = strings.TrimSpace(res.BaseCommit)
		}
	} else {
		if tool == ToolPhaseRetry {
			prevTermID := strings.TrimSpace(ln.TerminalID)
			if err := c.stopCurrentTerminal(ctx, ln, "replace terminal for retry"); err != nil {
				c.noteStopFailure(ctx, ln, err)
				return err
			}
			delete(c.pendingStops, ln.ID+":"+ln.AttemptKey())
			if prevTermID != "" {
				stoppedTerminals = []string{prevTermID}
			}
		} else {
			stoppedTerminals = c.stopPreviousPhaseTerminals(ctx, ln, "new phase "+phase.Name+" started")
		}
		res, err := c.driver.StartPhaseTerminal(ctx, lane.StartPhaseRequest{
			RunID:      c.cfg.RunID,
			ActionKey:  actionKey,
			Repository: repo,
			LaneID:     ln.ID,
			SessionID:  ln.SessionID,
			Phase:      phase.Name,
			Round:      round,
			Harness:    agent.Harness,
			Model:      agent.Model,
			Effort:     agent.Effort,
			Brief:      brief,
		})
		if err != nil {
			return &LaneLocalError{Err: err}
		}
		sessionID = res.SessionID
		terminalID = res.TerminalID
	}

	if phase.Name != recipe.PhaseIntegrate {
		if _, _, err := c.recordDecision(ctx, tool+".started", ln.ID, reason, "terminal started", DecisionArgs{ActionKey: "started:" + actionKey, Phase: phase.Name, Round: round, Attempt: attempt, SessionID: sessionID, TerminalID: terminalID}); err != nil {
			return err
		}
	}

	now := c.clock.Now().UTC()
	ln.Status = lane.StatusRunning
	ln.RecipeName = cmp.Or(ln.RecipeName, r.Name)
	if ln.RecipeVersion == 0 {
		ln.RecipeVersion = max(r.Version, 1)
	}
	ln.Attempt = attempt
	ln.RecordAttempt(phase.Name, round, sessionID, terminalID)
	if eventID != "" {
		ln.LastEventID = eventID
	}
	ln.LastEventAt = now

	if err := c.store.Lanes().Upsert(ctx, *ln); err != nil {
		return err
	}

	if recoveryResult != "" {
		reason = recoveryResult + ": " + reason
	}
	decisionEventID := eventID
	if _, ok := c.decidedEvents[eventID]; ok {
		decisionEventID = ""
	}
	_, _, err := c.recordDecision(ctx, tool, ln.ID, reason,
		fmt.Sprintf("%s round %d attempt %d (%s:%s)", phase.Name, round, attempt, agent.Harness, agent.Model),
		DecisionArgs{
			ActionKey:        actionKey,
			SessionID:        sessionID,
			TerminalID:       terminalID,
			EventID:          decisionEventID,
			FromPhase:        fromPhase,
			ToPhase:          phase.Name,
			Phase:            phase.Name,
			Round:            round,
			Attempt:          attempt,
			Harness:          agent.Harness,
			Model:            agent.Model,
			Effort:           agent.Effort,
			StoppedTerminals: stoppedTerminals,
		},
	)
	if err != nil {
		return err
	}
	if ln.ItemID != "" {
		if item, err := c.store.Backlog().Get(ctx, c.cfg.RunID, ln.ItemID); err == nil && item.Status == store.BacklogOpen {
			item.Status = store.BacklogInProgress
			item.LaneID = ln.ID
			_ = c.store.Backlog().Update(ctx, *item, ActorController, "lane started")
		}
	}
	return nil
}

func (c *Controller) integrateRecoveryState(ctx context.Context, laneID, actionKey string) (int, time.Time, error) {
	decs, err := c.store.Decisions().List(ctx, store.DecisionFilter{RunID: c.cfg.RunID, LaneID: laneID})
	if err != nil {
		return 0, time.Time{}, err
	}
	count := 0
	var last time.Time
	for i := range decs {
		if decs[i].Tool != "phase.launch.recovery" {
			continue
		}
		var args DecisionArgs
		if json.Unmarshal(decs[i].Args, &args) == nil && strings.HasPrefix(args.ActionKey, "recover:"+actionKey+":") {
			count++
			if decs[i].CreatedAt.After(last) {
				last = decs[i].CreatedAt
			}
		}
	}
	return count, last, nil
}

func (c *Controller) stopCurrentTerminal(ctx context.Context, ln *lane.Lane, reason string) error {
	if c.driver == nil || ln == nil || ln.Status.Terminal() || (strings.TrimSpace(ln.TerminalID) == "" && strings.TrimSpace(ln.SessionID) == "") {
		return nil
	}
	err := c.driver.Stop(ctx, lane.StopRequest{RunID: c.cfg.RunID, LaneID: ln.ID, SessionID: ln.SessionID, TerminalID: ln.TerminalID, Reason: reason})
	if err != nil {
		err = laneDriverError(err)
	}
	if err == nil && strings.TrimSpace(ln.TerminalID) != "" {
		termID := strings.TrimSpace(ln.TerminalID)
		c.stoppedTerminals[termID] = true
		delete(c.staleStopFailures, ln.ID+":"+termID)
	}
	return err
}

func (c *Controller) stopPreviousPhaseTerminals(ctx context.Context, ln *lane.Lane, reason string) []string {
	if c.driver == nil || ln == nil {
		return nil
	}
	seen := make(map[string]bool)
	var stopped []string
	stopOne := func(sessionID, termID string) {
		termID = strings.TrimSpace(termID)
		if termID == "" || seen[termID] {
			return
		}
		seen[termID] = true
		if c.stoppedTerminals[termID] {
			stopped = append(stopped, termID)
			return
		}
		stopKey := ln.ID + ":" + termID
		if c.staleStopFailures[stopKey] >= 3 {
			return
		}
		if err := c.driver.Stop(ctx, lane.StopRequest{
			RunID:      c.cfg.RunID,
			LaneID:     ln.ID,
			SessionID:  strings.TrimSpace(cmp.Or(sessionID, ln.SessionID)),
			TerminalID: termID,
			Reason:     reason,
		}); err == nil {
			c.stoppedTerminals[termID] = true
			delete(c.staleStopFailures, stopKey)
			stopped = append(stopped, termID)
		} else {
			c.staleStopFailures[stopKey]++
		}
	}
	stopOne(ln.SessionID, ln.TerminalID)
	keys := make([]string, 0, len(ln.Terminals))
	for k := range ln.Terminals {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		termID := ln.Terminals[k]
		sessID := ln.SessionID
		if s := strings.TrimSpace(ln.Sessions[k]); s != "" {
			sessID = s
		}
		stopOne(sessID, termID)
	}
	return stopped
}

func (c *Controller) noteStopFailure(ctx context.Context, ln *lane.Lane, err error) {
	if ln == nil {
		return
	}
	key := ln.ID + ":" + ln.AttemptKey()
	c.pendingStops[key]++
	_, _, _ = c.recordDecision(ctx, ToolPhaseRetry+".stop", ln.ID,
		"terminal stop failed; replacement withheld", fmt.Sprintf("stop retry %d: %v", c.pendingStops[key], err),
		DecisionArgs{ActionKey: "stop-failure:" + key, Phase: ln.Phase, Round: max(ln.Round, 1)})
	if c.pendingStops[key] >= 3 {
		ln.Status = lane.StatusBlocked
		_ = c.store.Lanes().Upsert(ctx, *ln)
		_, _, _ = c.recordDecision(ctx, ToolPhaseRetry+".stop", ln.ID,
			"terminal stop failed repeatedly", "escalated without replacement",
			DecisionArgs{ActionKey: "stop-exhausted:" + key, Phase: ln.Phase, Round: max(ln.Round, 1)})
		delete(c.pendingStops, key)
	}
}

func (c *Controller) retryStops(ctx context.Context) {
	for key := range c.pendingStops {
		parts := strings.SplitN(key, ":", 2)
		if len(parts) != 2 {
			delete(c.pendingStops, key)
			continue
		}
		ln, err := c.store.Lanes().Get(ctx, c.cfg.RunID, parts[0])
		if err != nil || ln.Status.Terminal() {
			delete(c.pendingStops, key)
			continue
		}
		err = c.stopCurrentTerminal(ctx, ln, "retry failed terminal stop")
		if err == nil {
			if termID := strings.TrimSpace(ln.TerminalID); termID != "" {
				c.stoppedTerminals[termID] = true
				delete(c.staleStopFailures, ln.ID+":"+termID)
			}
			delete(c.pendingStops, key)
		} else {
			c.noteStopFailure(ctx, ln, err)
		}
	}
}

func (c *Controller) releaseLaneSession(ctx context.Context, ln *lane.Lane) error {
	if ln == nil || strings.TrimSpace(ln.SessionID) == "" {
		return nil
	}
	if c.laneIsWorkingOrBusy(ctx, *ln) {
		return nil
	}
	if r, ok := c.driver.(lane.SessionReleaser); ok {
		return r.ReleaseSession(ctx, ln.SessionID)
	}
	return nil
}

func (c *Controller) runRepository(ctx context.Context) string {
	if c.store == nil || c.cfg.RunID == "" {
		return ""
	}
	if run, err := c.store.Runs().Get(ctx, c.cfg.RunID); err == nil && run != nil {
		return strings.TrimSpace(run.Repository)
	}
	return ""
}

func isWrongRepoReport(rep LaneReportPayload, eventSummary, wantRepo string) bool {
	combined := strings.Join([]string{rep.Reason, rep.Summary, rep.Ask, rep.Breaks, rep.Raw, eventSummary}, "\n")
	return isWrongRepoText(combined, wantRepo)
}

func isWrongRepoText(text, wantRepo string) bool {
	s := strings.ToLower(strings.TrimSpace(text))
	if s == "" {
		return false
	}
	for _, phrase := range []string{
		"wrong repo",
		"wrong repository",
		"unrelated repo",
		"unrelated repository",
	} {
		if strings.Contains(s, phrase) {
			return true
		}
	}
	for _, prefix := range []string{
		"checked out to ",
		"checked out in ",
		"workspace is in ",
		"session is on ",
	} {
		idx := strings.Index(s, prefix)
		if idx < 0 {
			continue
		}
		clause := strings.TrimSpace(s[idx+len(prefix):])
		for _, sep := range []string{"\n", ";", ". "} {
			clause, _, _ = strings.Cut(clause, sep)
		}
		word, _, _ := strings.Cut(clause, " ")
		word = strings.Trim(word, ".,;:()[]\"'`")
		if !lane.IsRepositorySlug(word) {
			continue
		}
		if _, afterInstead, ok := strings.Cut(clause, " instead of "); ok {
			other, _, _ := strings.Cut(strings.TrimSpace(afterInstead), " ")
			other = strings.Trim(other, ".,;:()[]\"'`")
			if lane.IsRepositorySlug(other) && !strings.EqualFold(lane.NormalizeRepositorySlug(word), lane.NormalizeRepositorySlug(other)) {
				return true
			}
		}
		if !strings.Contains(clause, "repo") || !lane.IsRepositorySlug(wantRepo) {
			continue
		}
		if !strings.EqualFold(lane.NormalizeRepositorySlug(word), lane.NormalizeRepositorySlug(wantRepo)) {
			return true
		}
	}
	return false
}

func wrongRepoActionKey(ln *lane.Lane) string {
	return fmt.Sprintf("wrong-repo-respawn:%s:%s", ln.ID, ln.AttemptKey())
}

func (c *Controller) canRespawnWrongRepo(ln *lane.Lane, eventID string) (replayed bool, allowed bool) {
	dec, ok := c.decidedActions[wrongRepoActionKey(ln)]
	if !ok {
		return false, true
	}
	var args DecisionArgs
	if len(dec.Args) > 0 && json.Unmarshal(dec.Args, &args) == nil && eventID != "" && args.EventID == eventID {
		return true, true
	}
	return false, false
}

func (c *Controller) respawnWrongRepoLane(
	ctx context.Context,
	ln *lane.Lane,
	r *recipe.Recipe,
	phase recipe.Phase,
	eventID, reason string,
) (store.EventClass, error) {
	round := max(ln.Round, 1)
	attempt := max(ln.Attempt, 1)
	oldSession := strings.TrimSpace(ln.SessionID)
	actionKey := wrongRepoActionKey(ln)
	if replayed, allowed := c.canRespawnWrongRepo(ln, eventID); replayed || !allowed {
		return store.EventClassAuto, nil
	}
	if len(phase.Agents) == 0 {
		return store.EventClassUnclassified, fmt.Errorf("orca/controller: phase %q in recipe %q has no agents", phase.Name, r.Name)
	}
	agent := phase.Agents[min(attempt-1, len(phase.Agents)-1)]

	if err := c.stopCurrentTerminal(ctx, ln, "wrong repository"); err != nil {
		slog.WarnContext(ctx, "orca/controller: failed to stop terminal before wrong-repo respawn",
			"run_id", c.cfg.RunID, "lane_id", ln.ID, "terminal_id", ln.TerminalID, "err", err)
	}
	delete(c.pendingStops, ln.ID+":"+ln.AttemptKey())
	if oldSession != "" {
		if rel, ok := c.driver.(lane.SessionReleaser); ok {
			if err := rel.ReleaseSession(ctx, oldSession, true); err != nil {
				slog.WarnContext(ctx, "orca/controller: failed to release wrong-repo session",
					"run_id", c.cfg.RunID, "lane_id", ln.ID, "session_id", oldSession, "err", err)
			}
		}
	}
	ln.SessionID = ""
	ln.TerminalID = ""

	brief := c.buildBrief(ctx, ln, r, phase, round, "")
	res, err := c.driver.SpawnSession(ctx, lane.SpawnRequest{
		RunID:      c.cfg.RunID,
		Repository: c.runRepository(ctx),
		LaneID:     ln.ID,
		Phase:      phase.Name,
		Round:      round,
		Harness:    agent.Harness,
		Model:      agent.Model,
		Effort:     agent.Effort,
		Brief:      brief,
		Branch:     ln.Branch,
		BaseBranch: ln.BaseBranch,
		Worktree:   ln.Worktree,
	})
	if err != nil {
		ln.Status = lane.StatusBlocked
		ln.LastEventID = eventID
		ln.LastEventAt = c.clock.Now().UTC()
		_ = c.store.Lanes().Upsert(ctx, *ln)
		return store.EventClassUnclassified, laneDriverError(err)
	}
	if strings.TrimSpace(res.Branch) != "" {
		ln.Branch = strings.TrimSpace(res.Branch)
	}
	if strings.TrimSpace(res.BaseBranch) != "" {
		ln.BaseBranch = strings.TrimSpace(res.BaseBranch)
	}
	if strings.TrimSpace(res.BaseCommit) != "" {
		ln.BaseCommit = strings.TrimSpace(res.BaseCommit)
	}
	ln.Status = lane.StatusRunning
	ln.Attempt = attempt
	ln.RecordAttempt(phase.Name, round, res.SessionID, res.TerminalID)
	ln.LastEventID = eventID
	ln.LastEventAt = c.clock.Now().UTC()
	if err := c.store.Lanes().Upsert(ctx, *ln); err != nil {
		return store.EventClassUnclassified, err
	}
	_, _, err = c.recordDecision(ctx, ToolPhaseRetry, ln.ID,
		fmt.Sprintf("wrong repository on session %s (%s); released and respawned", oldSession, reason),
		fmt.Sprintf("respawned %s round %d on %s", phase.Name, round, res.SessionID),
		DecisionArgs{
			ActionKey:  actionKey,
			EventID:    eventID,
			Phase:      phase.Name,
			Round:      round,
			Attempt:    attempt,
			SessionID:  res.SessionID,
			TerminalID: res.TerminalID,
		},
	)
	return store.EventClassAuto, err
}

func (c *Controller) terminalAgentState(ctx context.Context, terminalID string, useCache bool) (string, bool, error) {
	terminalID = strings.TrimSpace(terminalID)
	if terminalID == "" {
		return "", false, nil
	}
	reader, ok := c.driver.(lane.TerminalStateReader)
	if !ok {
		return "", false, nil
	}
	if useCache {
		c.cacheMu.Lock()
		if c.terminalStateCache != nil {
			if cached, exists := c.terminalStateCache[terminalID]; exists {
				c.cacheMu.Unlock()
				return cached.state, true, cached.err
			}
		}
		c.cacheMu.Unlock()
	}
	state, err := reader.TerminalAgentState(ctx, terminalID)
	if useCache {
		c.cacheMu.Lock()
		if c.terminalStateCache != nil {
			c.terminalStateCache[terminalID] = terminalStateCacheEntry{state: state, err: err}
		}
		c.cacheMu.Unlock()
	}
	return state, true, err
}

func (c *Controller) terminalIsConfirmedWorking(ctx context.Context, terminalID string, useCache bool) bool {
	state, ok, err := c.terminalAgentState(ctx, terminalID, useCache)
	if !ok || err != nil {
		return false
	}
	trimmed := strings.TrimSpace(state)
	return strings.EqualFold(trimmed, "working") || strings.EqualFold(trimmed, "busy")
}

func (c *Controller) terminalIsWorkingOrUnknown(ctx context.Context, terminalID string) bool {
	state, ok, err := c.terminalAgentState(ctx, terminalID, true)
	if !ok {
		return false
	}
	// Transient/unknown API errors are treated as working for destructive
	// replacement safety, unless the terminal is confirmed not found (404).
	if err != nil {
		return !errors.Is(err, store.ErrNotFound)
	}
	trimmed := strings.TrimSpace(state)
	return strings.EqualFold(trimmed, "working") || strings.EqualFold(trimmed, "busy")
}

func (c *Controller) sessionIsBusy(ctx context.Context, ln lane.Lane) bool {
	if c.driver == nil || (strings.TrimSpace(ln.SessionID) == "" && strings.TrimSpace(ln.TerminalID) == "") {
		return false
	}
	hr, ok := c.driver.(lane.SessionHealthReader)
	if !ok {
		return false
	}
	health, err := c.laneSessionHealth(ctx, ln, hr)
	return err == nil && health.HasBusyProcess()
}

func (c *Controller) sessionIsBusyUncached(ctx context.Context, ln lane.Lane) bool {
	if c.driver == nil || (strings.TrimSpace(ln.SessionID) == "" && strings.TrimSpace(ln.TerminalID) == "") {
		return false
	}
	hr, ok := c.driver.(lane.SessionHealthReader)
	if !ok {
		return false
	}
	health, err := hr.LaneSessionHealth(ctx, ln)
	return err == nil && health.HasBusyProcess()
}

func (c *Controller) laneSessionHealth(ctx context.Context, ln lane.Lane, hr lane.SessionHealthReader) (lane.SessionHealth, error) {
	key := ln.ID
	if key == "" {
		key = ln.SessionID + "\x00" + ln.TerminalID
	}
	c.cacheMu.Lock()
	if c.sessionHealthCache != nil {
		if health, ok := c.sessionHealthCache[key]; ok {
			c.cacheMu.Unlock()
			return health, nil
		}
	}
	c.cacheMu.Unlock()
	health, err := hr.LaneSessionHealth(ctx, ln)
	if err == nil {
		c.cacheMu.Lock()
		if c.sessionHealthCache != nil {
			c.sessionHealthCache[key] = health
		}
		c.cacheMu.Unlock()
	}
	return health, err
}

func (c *Controller) laneIsWorkingOrBusy(ctx context.Context, ln lane.Lane) bool {
	return c.terminalIsConfirmedWorking(ctx, ln.TerminalID, true) || c.sessionIsBusy(ctx, ln)
}

func (c *Controller) laneIsWorkingOrBusyUncached(ctx context.Context, ln lane.Lane) bool {
	return c.terminalIsConfirmedWorking(ctx, ln.TerminalID, false) || c.sessionIsBusyUncached(ctx, ln)
}

func (c *Controller) hasActiveExpectedWait(ctx context.Context, laneID string, now time.Time) bool {
	laneID = strings.TrimSpace(laneID)
	if c.store == nil || laneID == "" {
		return false
	}
	dirs, err := c.store.Directives().List(ctx, store.DirectiveFilter{
		RunID:      c.cfg.RunID,
		Scope:      laneID,
		ActiveOnly: false,
	})
	if err != nil {
		return false
	}
	for i := range dirs {
		d := &dirs[i]
		if d.Status != store.DirectiveActive {
			continue
		}
		if strings.HasPrefix(d.Ref, ownerAskRefPrefix) {
			return true
		}
		if strings.HasPrefix(d.Ref, NamedWaitRefPrefix) && (d.ExpiresAt == nil || now.Before(*d.ExpiresAt)) {
			return true
		}
	}
	return false
}

func (c *Controller) effectiveStallTimeout(r *recipe.Recipe) time.Duration {
	c.cacheMu.Lock()
	cfgTimeout := c.cfg.StallTimeout
	explicit := c.stallTimeoutSet
	c.cacheMu.Unlock()
	timeout := cfgTimeout
	if r != nil && r.Limits.PhaseTimeout > 0 && !explicit {
		timeout = r.Limits.PhaseTimeout
	}
	if timeout <= 0 {
		timeout = cfgTimeout
	}
	return timeout
}

func (c *Controller) nudgeWaitDuration(r *recipe.Recipe) time.Duration {
	timeout := c.effectiveStallTimeout(r)
	return max(timeout/2, 5*time.Minute)
}

func (c *Controller) quietLaneKey(ln *lane.Lane) string {
	key := fmt.Sprintf("%s:%s:%d", ln.ID, ln.Phase, max(ln.Round, 1))
	if ln.AnswerEpoch > 0 {
		key += fmt.Sprintf(":answer-%d", ln.AnswerEpoch)
	}
	return key
}

func (c *Controller) quietLadderRungFromStore(ctx context.Context, ln *lane.Lane, r *recipe.Recipe, now time.Time) (int, bool) {
	if ln == nil || c.store == nil {
		return 0, false
	}
	key := c.quietLaneKey(ln)
	nudgeKey := "restart:nudge:" + key
	freshKey := "restart:fresh:" + key

	decs, err := c.store.Decisions().List(ctx, store.DecisionFilter{
		RunID:  c.cfg.RunID,
		LaneID: ln.ID,
	})
	if err != nil {
		return 0, false
	}
	var nudgeDec, freshDec *store.Decision
	for i := range decs {
		var args DecisionArgs
		if len(decs[i].Args) == 0 || json.Unmarshal(decs[i].Args, &args) != nil {
			continue
		}
		switch args.ActionKey {
		case nudgeKey:
			nudgeDec = &decs[i]
		case freshKey:
			freshDec = &decs[i]
		}
	}
	if nudgeDec == nil {
		return 0, false
	}
	wait := c.nudgeWaitDuration(r)
	if freshDec == nil {
		waitStart := nudgeDec.CreatedAt
		if ln.LastEventAt.After(waitStart) {
			waitStart = ln.LastEventAt
		}
		if !waitStart.IsZero() && now.Before(waitStart.Add(wait)) {
			return 1, true
		}
		return 1, false
	}
	return 2, false
}

func (c *Controller) buildBrief(ctx context.Context, ln *lane.Lane, r *recipe.Recipe, phase recipe.Phase, round int, extraNote string) string {
	var parts []string
	if ln.Title != "" {
		parts = append(parts, ln.Title)
	}
	mechanical := phase.Name == recipe.PhaseClean || phase.Name == recipe.PhaseReview
	if ln.ItemID != "" && !mechanical {
		if item, err := c.store.Backlog().Get(ctx, c.cfg.RunID, ln.ItemID); err == nil {
			if item.Description != "" {
				parts = append(parts, item.Description)
			}
			if item.Acceptance != "" {
				parts = append(parts, "Acceptance: "+item.Acceptance)
			}
		}
	}
	if skill := c.driver.PhaseSkill(phase.Name); skill != "" {
		parts = []string{skill}
		if ln.Title != "" {
			parts = append(parts, "Goal: "+ln.Title)
		}
	} else if phase.BriefTemplate != "" {
		parts = append(parts, phase.BriefTemplate)
	}
	if extraNote != "" {
		parts = append(parts, extraNote)
	}
	reportInstructions := c.driver.ReportInstructions(c.cfg.RunID, ln.ID, phase.Name, round)
	return lane.AppendReportInstructions(strings.Join(parts, "\n\n"), reportInstructions, r.Limits.MaxBriefBytes)
}

func (c *Controller) isNoMergeLane(ctx context.Context, ln *lane.Lane) bool {
	if ln == nil {
		return false
	}
	var item *store.BacklogItem
	itemID := cmp.Or(ln.ItemID, ln.ID)
	if itemID != "" && c.store != nil && c.store.Backlog() != nil {
		if loaded, err := c.store.Backlog().Get(ctx, c.cfg.RunID, itemID); err == nil {
			item = loaded
		}
	}
	return store.IsNoMergeLane(*ln, item)
}

func (c *Controller) resolveRecipe(ctx context.Context, ln *lane.Lane) (*recipe.Recipe, error) {
	name := ln.RecipeName
	if name == "" {
		if run, err := c.store.Runs().Get(ctx, c.cfg.RunID); err == nil && run.DefaultRecipe != "" {
			name = run.DefaultRecipe
		} else {
			name = recipe.DefaultName
		}
	}
	r, err := c.store.Recipes().Get(ctx, c.cfg.RunID, name, ln.RecipeVersion)
	if errors.Is(err, store.ErrNotFound) && name == recipe.DefaultName {
		def := recipe.Default()
		r = &def
		err = nil
	}
	if err != nil {
		return nil, err
	}
	if r != nil && c.isNoMergeLane(ctx, ln) {
		stripped := recipe.WithoutIntegrate(*r)
		return &stripped, nil
	}
	return r, nil
}

func (c *Controller) extractReport(ctx context.Context, ln *lane.Lane, ev store.Event) (LaneReportPayload, error) {
	var payload LaneReportPayload
	if len(ev.Payload) > 0 {
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			var rawStr string
			if strErr := json.Unmarshal(ev.Payload, &rawStr); strErr == nil {
				if parsed, pErr := lane.ParsePhaseReport(rawStr); pErr == nil {
					payload.PhaseReport = *parsed
				}
			} else if parsed, pErr := lane.ParsePhaseReport(string(ev.Payload)); pErr == nil {
				payload.PhaseReport = *parsed
			}
		}
	}
	payload.HeadSHA = cmp.Or(payload.HeadSHA, payload.Head)

	termID := cmp.Or(payload.TerminalID, ln.TerminalID)
	if termID != "" && c.driver != nil {
		if drvRep, err := c.driver.ReadReport(ctx, c.cfg.RunID, ln.ID, termID); err == nil && drvRep != nil {
			payload.Outcome = cmp.Or(payload.Outcome, drvRep.Outcome)
			payload.Change = cmp.Or(payload.Change, drvRep.Change)
			payload.HeadSHA = cmp.Or(payload.HeadSHA, drvRep.HeadSHA)
			payload.PR = cmp.Or(payload.PR, drvRep.PR)
			payload.Phase = cmp.Or(payload.Phase, drvRep.Phase)
			payload.Round = cmp.Or(payload.Round, drvRep.Round)
			payload.Reason = cmp.Or(payload.Reason, drvRep.Reason)
			payload.For = cmp.Or(payload.For, drvRep.For)
			payload.Ask = cmp.Or(payload.Ask, drvRep.Ask)
			payload.Breaks = cmp.Or(payload.Breaks, drvRep.Breaks)
			payload.Summary = cmp.Or(payload.Summary, drvRep.Summary)
		}
	}

	if payload.Outcome == "" && ev.Summary != "" {
		if parsed, err := lane.ParsePhaseReport(ev.Summary); err == nil {
			payload.PhaseReport = *parsed
		}
	}

	if !payload.Outcome.Valid() {
		return LaneReportPayload{}, fmt.Errorf("invalid or missing phase report outcome %q", payload.Outcome)
	}
	return payload, nil
}

func (c *Controller) enqueueJudgmentEvent(ev store.Event) {
	if _, ok := c.dispatchedEvents[ev.ID]; ok {
		return
	}
	if slices.ContainsFunc(c.pendingJudgment, func(e store.Event) bool { return e.ID == ev.ID }) {
		return
	}
	now := c.clock.Now().UTC()
	if len(c.pendingJudgment) == 0 {
		if !ev.CreatedAt.IsZero() && ev.CreatedAt.Before(now) {
			c.batchFirstAt = ev.CreatedAt.UTC()
		} else {
			c.batchFirstAt = now
		}
	}
	c.pendingJudgment = append(c.pendingJudgment, ev.Clone())
}

func (c *Controller) flushJudgmentIfDue(ctx context.Context) error {
	if len(c.pendingJudgment) == 0 {
		return nil
	}
	now := c.clock.Now().UTC()
	if slices.ContainsFunc(c.pendingJudgment, func(e store.Event) bool { return e.Urgent }) {
		return c.flushJudgment(ctx, "urgent bypass")
	}
	if !now.Before(c.batchFirstAt.Add(c.cfg.JudgmentDebounce)) {
		return c.flushJudgment(ctx, "debounce elapsed")
	}
	return nil
}

func (c *Controller) flushJudgment(ctx context.Context, reason string) error {
	now := c.clock.Now().UTC()
	if paused, _, err := c.enforceCostGuard(ctx, now); err != nil {
		return err
	} else if paused {
		return nil
	}

	var batch []store.Event
	var eventIDs []string
	for i := range c.pendingJudgment {
		ev := c.pendingJudgment[i]
		if _, already := c.dispatchedEvents[ev.ID]; already {
			continue
		}
		batch = append(batch, ev.Clone())
		eventIDs = append(eventIDs, ev.ID)
	}
	c.pendingJudgment = nil
	c.batchFirstAt = time.Time{}

	if len(batch) == 0 {
		return nil
	}

	actionKey := judgmentBatchActionKey(eventIDs)
	_, already, err := c.recordDecision(ctx, ToolJudgmentDispatch, "",
		reason,
		fmt.Sprintf("dispatched %d judgment event(s)", len(batch)),
		DecisionArgs{
			ActionKey: actionKey,
			EventIDs:  eventIDs,
		},
	)
	if err != nil {
		return err
	}
	if already {
		return nil
	}

	if c.judge != nil {
		if err := c.judge.Judge(ctx, c.cfg.RunID, batch); err != nil {
			return err
		}
	}
	_, _, _ = c.enforceCostGuard(ctx, c.clock.Now().UTC())
	return nil
}

func (c *Controller) enforceCostGuard(ctx context.Context, now time.Time) (bool, string, error) {
	settings, err := c.store.Runs().GetSettings(ctx, c.cfg.RunID)
	if err != nil {
		return false, "", err
	}
	paused, _, _, msg, err := store.EnforceCostGuard(ctx, c.store, c.cfg.RunID, now, c.cfg.MaxCostPerHourUSD, c.cfg.SubscriptionActors...)
	if err != nil {
		return false, "", err
	}
	if paused && !settings.Paused {
		c.notifyOwnerCostGuard(ctx, msg)
	}
	return paused, msg, nil
}

func (c *Controller) notifyOwnerCostGuard(ctx context.Context, msg string) {
	if strings.TrimSpace(msg) == "" {
		return
	}
	if c.mailbox == nil {
		return
	}
	// The persisted guard decision identifies the breach across restarts, even
	// when two unpause/re-pause transitions share a clock timestamp.
	decisions, err := c.store.Decisions().List(ctx, store.DecisionFilter{RunID: c.cfg.RunID})
	if err != nil {
		slog.WarnContext(ctx, "cost guard notification lookup failed", "run", c.cfg.RunID, "error", err)
		return
	}
	for i := len(decisions) - 1; i >= 0; i-- {
		if decisions[i].Tool == "cost.guard" {
			id := fmt.Sprintf("cost-guard-%s-%d", c.cfg.RunID, decisions[i].Seq)
			_, _ = c.mailbox.AskOwner(ctx, mailbox.Question{ID: id, RunID: c.cfg.RunID, Ask: msg})
			return
		}
	}
}

func boolToChecks(green bool) string {
	if green {
		return "green"
	}
	return "red"
}

func isCIGreen(checks string) bool {
	switch strings.ToLower(strings.TrimSpace(checks)) {
	case "green", "pass", "passed", "success", "ok", "true":
		return true
	default:
		return false
	}
}

func mergeNamedCheckStatus(existing, name, status string, green *bool) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return existing
	}
	value := strings.TrimSpace(status)
	if value == "" && green != nil {
		value = boolToChecks(*green)
	}
	if value == "" {
		return existing
	}
	newEntry := name + ": " + value
	if existing == "" {
		return newEntry
	}
	target := normalizeCheckName(name)
	parts := strings.Split(existing, ";")
	for i, part := range parts {
		key, _, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok {
			key, _, ok = strings.Cut(strings.TrimSpace(part), "=")
		}
		if ok && normalizeCheckName(key) == target {
			parts[i] = newEntry
			return strings.Join(parts, "; ")
		}
	}
	return existing + "; " + newEntry
}

func normalizeCheckName(name string) string {
	name = strings.ToLower(strings.ReplaceAll(name, "&", "and"))
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isCIRed(checks string) bool {
	switch strings.ToLower(strings.TrimSpace(checks)) {
	case "red", "fail", "failed", "failure", "error", "false":
		return true
	default:
		return false
	}
}

func isReviewPhase(p recipe.Phase) bool {
	if p.Name == recipe.PhaseReview || (!p.Mutating && p.Name != "") {
		return true
	}
	return hasGate(p, "review_head_pinned")
}

func recipeHasReviewPhase(r *recipe.Recipe) bool {
	return slices.ContainsFunc(r.Phases, isReviewPhase)
}

func hasGate(p recipe.Phase, gateName string) bool {
	return slices.ContainsFunc(p.Gates, func(g recipe.Gate) bool {
		return g.Name == gateName
	})
}

func ownerAskDirectiveID(askID string) string {
	return "ask-" + askID
}

func phaseActionKey(tool, laneID, phase string, round, attempt int) string {
	return fmt.Sprintf("%s:%s:%s:attempt-%d", tool, laneID, lane.PhaseRoundKey(phase, round), max(attempt, 1))
}

func completeActionKey(laneID, fromPhase string, round int) string {
	return fmt.Sprintf("%s:%s:%s", ToolLaneComplete, laneID, lane.PhaseRoundKey(fromPhase, round))
}

func integrateOnceActionKey(laneID, fromPhase string, round int, head string) string {
	return completeActionKey(laneID, fromPhase, round) + ":reviewed-head:" + head
}

func escalateActionKey(laneID, fromPhase string, round int, eventID string) string {
	return fmt.Sprintf("%s:%s:%s:%s", ToolLaneEscalate, laneID, lane.PhaseRoundKey(fromPhase, round), eventID)
}

func stallActionKey(laneID, phase string, round, attempt int, terminalID string) string {
	return fmt.Sprintf("%s:%s:%s:attempt-%d:%s", ToolLaneStall, laneID, lane.PhaseRoundKey(phase, round), max(attempt, 1), terminalID)
}

func askDefaultActionKey(askID string) string {
	return fmt.Sprintf("%s:%s", ToolOwnerAskDefault, askID)
}

func judgmentBatchActionKey(eventIDs []string) string {
	return fmt.Sprintf("%s:%s", ToolJudgmentDispatch, strings.Join(eventIDs, ","))
}
