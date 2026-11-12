// Package controller implements the deterministic swarm orchestrator state machine.
// Every action is recorded in store.DecisionStore before execution so a crash and restart never repeats it.
package controller

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/orch/clock"
	"github.com/bazelment/yoloswe/orch/lane"
	"github.com/bazelment/yoloswe/orch/recipe"
	"github.com/bazelment/yoloswe/orch/store"
)

// Default configuration values for Controller.
const (
	DefaultConsumer          = "controller"
	DefaultHolderID          = "controller-1"
	DefaultLeaseTTL          = 30 * time.Second
	DefaultHeartbeatInterval = 10 * time.Second
	DefaultPollInterval      = 5 * time.Second
	DefaultJudgmentDebounce  = 2 * time.Minute
	DefaultStallTimeout      = 45 * time.Minute
	ActorController          = "controller"
)

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

const ownerAskRefPrefix = "owner_ask:"

// ErrControllerStopped is returned when Step or Run is called on a killed controller.
var ErrControllerStopped = errors.New("orch/controller: controller stopped")

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

// Config configures a Controller instance for a single Run.
type Config struct {
	RunID             string
	HolderID          string
	Consumer          string
	LeaseTTL          time.Duration
	HeartbeatInterval time.Duration
	PollInterval      time.Duration
	JudgmentDebounce  time.Duration
	StallTimeout      time.Duration
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
	return c
}

// OwnerAsk represents a decision question posed to the owner with a mandatory
// default option and deadline ("decide, don't ask").
type OwnerAsk struct {
	Deadline  time.Time `json:"deadline"`
	ID        string    `json:"id"`
	RunID     string    `json:"run_id,omitempty"`
	LaneID    string    `json:"lane_id,omitempty"`
	Question  string    `json:"question"`
	Default   string    `json:"default"`
	SteerNote string    `json:"steer_note,omitempty"`
	Options   []string  `json:"options"`
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
	Green       *bool  `json:"green,omitempty"`
	HeadSHA     string `json:"head_sha,omitempty"`
	Status      string `json:"status,omitempty"`
	FailedCheck string `json:"failed_check,omitempty"`
	PR          int    `json:"pr,omitempty"`
}

// PhaseExitPayload is the JSON payload for store.EventPhaseExit events.
type PhaseExitPayload struct {
	TerminalID string `json:"terminal_id,omitempty"`
	Phase      string `json:"phase,omitempty"`
	Error      string `json:"error,omitempty"`
	Round      int    `json:"round,omitempty"`
	ExitCode   int    `json:"exit_code,omitempty"`
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
	ActionKey  string   `json:"action_key"`
	EventID    string   `json:"event_id,omitempty"`
	FromPhase  string   `json:"from_phase,omitempty"`
	ToPhase    string   `json:"to_phase,omitempty"`
	Phase      string   `json:"phase,omitempty"`
	Verdict    string   `json:"verdict,omitempty"`
	Harness    string   `json:"harness,omitempty"`
	Model      string   `json:"model,omitempty"`
	Effort     string   `json:"effort,omitempty"`
	TerminalID string   `json:"terminal_id,omitempty"`
	SessionID  string   `json:"session_id,omitempty"`
	AskID      string   `json:"ask_id,omitempty"`
	Default    string   `json:"default,omitempty"`
	EventIDs   []string `json:"event_ids,omitempty"`
	Round      int      `json:"round,omitempty"`
	Attempt    int      `json:"attempt,omitempty"`
}

type pendingVerdict struct {
	verdict string
	headSHA string
	key     string
}

// Controller is the deterministic orchestration loop for a single Run.
type Controller struct {
	batchFirstAt       time.Time
	store              store.Store
	driver             lane.LaneDriver
	clock              clock.Clock
	judge              Judge
	decidedActions     map[string]store.Decision
	decidedEvents      map[string]store.Decision
	dispatchedEvents   map[string]store.Decision
	pendingVerdicts    map[string]pendingVerdict
	emittedStalls      map[string]bool
	emittedAskExpiries map[string]bool
	wakeCh             chan struct{}
	cancelRun          context.CancelFunc
	pendingJudgment    []store.Event
	cfg                Config
	lastDecisionSeq    int64
	mu                 sync.Mutex
	leaseHeld          bool
	initialized        bool
	killed             bool
}

// New constructs a new deterministic Controller for cfg.RunID.
func New(cfg Config, st store.Store, drv lane.LaneDriver, clk clock.Clock, judge Judge) *Controller {
	if clk == nil {
		clk = clock.NewRealClock()
	}
	return &Controller{
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
		emittedAskExpiries: make(map[string]bool),
		wakeCh:             make(chan struct{}, 1),
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
	if c.cancelRun != nil {
		c.cancelRun()
	}
}

// ReleaseLease explicitly releases the run lease held by this Controller.
func (c *Controller) ReleaseLease(ctx context.Context) error {
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

	if err := c.Step(runCtx); err != nil && !errors.Is(err, store.ErrLeaseHeld) {
		return err
	}

	pollTicker := c.clock.NewTicker(c.cfg.PollInterval)
	defer pollTicker.Stop()

	hbTicker := c.clock.NewTicker(c.cfg.HeartbeatInterval)
	defer hbTicker.Stop()

	for {
		select {
		case <-runCtx.Done():
			return nil
		case <-hbTicker.C():
		case <-pollTicker.C():
		case <-c.wakeCh:
		}
		if err := c.Step(runCtx); err != nil && !errors.Is(err, store.ErrLeaseHeld) && !errors.Is(err, ErrControllerStopped) {
			return err
		}
	}
}

// Step executes one deterministic pass of lease maintenance, stall/expiry detection, event consumption, and judgment flushing.
func (c *Controller) Step(ctx context.Context) error {
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

	if !c.initialized {
		if err := c.recoverPendingState(ctx); err != nil {
			return err
		}
		c.initialized = true
	}

	if err := c.reconcilePlannedLanes(ctx); err != nil {
		return err
	}

	if err := c.detectStalls(ctx); err != nil {
		return err
	}

	if err := c.checkOwnerAsks(ctx); err != nil {
		return err
	}

	if err := c.consumeEvents(ctx); err != nil {
		return err
	}

	return c.flushJudgmentIfDue(ctx)
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
		return fmt.Errorf("orch/controller: recipe %q has no first phase", r.Name)
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

	if ask.LaneID != "" {
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
	dir, err := c.store.Directives().Get(ctx, c.cfg.RunID, ownerAskDirectiveID(askID))
	if err != nil {
		return err
	}
	var ask OwnerAsk
	if err := json.Unmarshal([]byte(dir.Text), &ask); err != nil {
		return fmt.Errorf("orch/controller: invalid owner ask directive %q: %w", askID, err)
	}
	if !slices.Contains(ask.Options, chosenOption) {
		return fmt.Errorf("%w: option %q not in ask %q options %v", store.ErrInvalidArgument, chosenOption, askID, ask.Options)
	}

	if err := c.store.Directives().UpdateStatus(ctx, c.cfg.RunID, dir.ID, store.DirectiveHandled); err != nil {
		return err
	}
	return c.applyOwnerAskResolution(ctx, ask, "ask-answer-"+ask.ID, chosenOption, chosenOption, "")
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
	if ask.LaneID == "" {
		return nil
	}
	ln, err := c.store.Lanes().Get(ctx, c.cfg.RunID, ask.LaneID)
	if err != nil {
		return nil
	}
	if ln.SessionID != "" || ln.TerminalID != "" {
		if err := c.driver.Steer(ctx, lane.SteerRequest{
			RunID:      c.cfg.RunID,
			LaneID:     ln.ID,
			SessionID:  ln.SessionID,
			TerminalID: ln.TerminalID,
			Message:    steerMsg,
		}); err != nil {
			return err
		}
	}
	if ln.Status == lane.StatusBlocked {
		ln.Status = lane.StatusRunning
		if eventID != "" {
			ln.LastEventID = eventID
		}
		ln.LastEventAt = c.clock.Now().UTC()
		if err := c.store.Lanes().Upsert(ctx, *ln); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) ensureLease(ctx context.Context) error {
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
	if args.EventID != "" && d.Tool != ToolLaneStall && d.Tool != ToolLaneEscalate {
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
		RunID:    c.cfg.RunID,
		Category: store.DecisionAuto,
		Actor:    ActorController,
		LaneID:   laneID,
		Tool:     tool,
		Reason:   reason,
		Result:   result,
		Args:     rawArgs,
		Vetted:   true,
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

// recoverPendingState reconstructs pending judgment events and CI-gated verdicts on startup.
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
			ev.Class = store.EventClassJudgment
			c.enqueueJudgmentEvent(ev)
			continue
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

	running, err := c.store.Lanes().List(ctx, store.LaneFilter{
		RunID:    c.cfg.RunID,
		Statuses: []store.LaneStatus{lane.StatusRunning},
	})
	if err != nil {
		return err
	}
	activeCount := len(running)

	for i := range planned {
		ln := planned[i]
		r, err := c.resolveRecipe(ctx, &ln)
		if err != nil {
			return err
		}
		if r.Limits.MaxConcurrentLanes > 0 && activeCount >= r.Limits.MaxConcurrentLanes {
			break
		}
		firstName := cmp.Or(ln.Phase, r.FirstPhase())
		firstPhase, ok := r.Phase(firstName)
		if !ok {
			continue
		}
		round := max(ln.Round, ln.MaxRound(firstName)+1)
		if err := c.startLanePhase(ctx, &ln, r, firstPhase, round, 1, ToolPhaseStart, "", "spawn planned lane", ""); err != nil {
			return err
		}
		activeCount++
	}
	return nil
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
		r, err := c.resolveRecipe(ctx, &ln)
		if err != nil {
			return err
		}
		timeout := r.Limits.PhaseTimeout
		if timeout <= 0 {
			timeout = c.cfg.StallTimeout
		}
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
		c.emittedAskExpiries[ask.ID] = true

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
			return err
		}
		ev.Class = class

		if class == store.EventClassJudgment {
			c.enqueueJudgmentEvent(ev)
			if ev.Urgent || c.cfg.JudgmentDebounce <= 0 {
				if err := c.flushJudgment(ctx, "urgent bypass"); err != nil {
					return err
				}
			}
		}

		if err := c.store.Events().SetCursor(ctx, c.cfg.RunID, c.cfg.Consumer, ev.Seq); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) handleEvent(ctx context.Context, ev *store.Event) (store.EventClass, error) {
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
		return store.EventClassJudgment, nil
	case store.EventTimer:
		if ev.Class == store.EventClassJudgment {
			return store.EventClassJudgment, nil
		}
		return store.EventClassAuto, nil
	default:
		return store.EventClassJudgment, nil
	}
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

	rep, err := c.extractReport(ctx, ln, *ev)
	if err != nil {
		return store.EventClassJudgment, nil
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
	if hasPhase && len(curPhase.ExitContract.AllowedOutcomes) > 0 {
		if !slices.Contains(curPhase.ExitContract.AllowedOutcomes, string(rep.Outcome)) {
			return c.blockLaneForJudgment(ctx, ln)
		}
	}

	if rep.Outcome == lane.OutcomeDone && isReviewPhase(curPhase) {
		ln.ApprovalSHA = cmp.Or(rep.HeadSHA, ln.PRHead, ln.ApprovalSHA)
	}

	return c.advanceByRecipe(ctx, ln, r, string(rep.Outcome), rep.Reason, rep.Breaks, ev)
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

func (c *Controller) advanceByRecipe(
	ctx context.Context,
	ln *lane.Lane,
	r *recipe.Recipe,
	verdict, reason, breaks string,
	ev *store.Event,
) (store.EventClass, error) {
	round := max(ln.Round, 1)
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
				return store.EventClassAuto, nil
			}
		}
		return c.blockLaneForJudgment(ctx, ln)
	}

	delete(c.pendingVerdicts, ln.ID)

	switch mech.To {
	case recipe.TargetComplete:
		if r.MergePolicy.RequireReviewPass && recipeHasReviewPhase(r) {
			if (ln.PRHead != "" || ln.ApprovalSHA != "") && ln.ApprovalStale() {
				return c.blockLaneForJudgment(ctx, ln)
			}
		}
		if r.MergePolicy.RequireCIGreen && !ciGreen {
			return c.blockLaneForJudgment(ctx, ln)
		}

		actionKey := completeActionKey(ln.ID, ln.Phase, round)
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
			ln.MergeSHA = cmp.Or(ln.MergeSHA, ln.PRHead)
			if err := c.store.Lanes().Upsert(ctx, *ln); err != nil {
				return store.EventClassUnclassified, err
			}
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
			return store.EventClassUnclassified, fmt.Errorf("orch/controller: unknown target phase %q in recipe %q", mech.To, r.Name)
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

	termID := cmp.Or(payload.TerminalID, ln.TerminalID)
	if termID != "" && payload.ExitCode == 0 && payload.Error == "" {
		if rep, err := c.driver.ReadReport(ctx, c.cfg.RunID, ln.ID, termID); err == nil && rep != nil && rep.Outcome.Valid() {
			return c.handleLaneReport(ctx, ev)
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
	ln.LastEventAt = c.clock.Now().UTC()

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

	if isCIGreen(ln.Checks) {
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

	round := max(ln.Round, 1)
	attempt := max(ln.Attempt, 1)
	actionKey := stallActionKey(ln.ID, ln.Phase, round, attempt, ln.TerminalID)
	c.emittedStalls[actionKey] = true

	_, already, err := c.recordDecision(ctx, ToolLaneStall, ln.ID,
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
	if !already {
		if ln.SessionID != "" || ln.TerminalID != "" {
			if err := c.driver.Stop(ctx, lane.StopRequest{
				RunID:      c.cfg.RunID,
				LaneID:     ln.ID,
				SessionID:  ln.SessionID,
				TerminalID: ln.TerminalID,
				Reason:     "phase stall timeout exceeded",
			}); err != nil {
				return store.EventClassUnclassified, err
			}
		}
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

	_, already, err := c.recordDecision(ctx, ToolOwnerAskDefault, ask.LaneID,
		fmt.Sprintf("owner ask %q expired at deadline; applying default %q", ask.ID, ask.Default),
		ask.Default,
		DecisionArgs{
			ActionKey: actionKey,
			EventID:   ev.ID,
			AskID:     ask.ID,
			Default:   ask.Default,
		},
	)
	if err != nil {
		return store.EventClassUnclassified, err
	}
	if already {
		return store.EventClassAuto, nil
	}

	_ = c.store.Directives().UpdateStatus(ctx, c.cfg.RunID, dirID, store.DirectiveHandled)

	if err := c.applyOwnerAskResolution(ctx, ask, "ask-default-"+ask.ID, ask.Default, cmp.Or(ask.SteerNote, ask.Default), ev.ID); err != nil {
		return store.EventClassUnclassified, err
	}
	return store.EventClassAuto, nil
}

func (c *Controller) startLanePhase(
	ctx context.Context,
	ln *lane.Lane,
	r *recipe.Recipe,
	phase recipe.Phase,
	round, attempt int,
	tool, eventID, reason, extraNote string,
) error {
	if round < 1 {
		round = 1
	}
	if attempt < 1 {
		attempt = 1
	}
	if len(phase.Agents) == 0 {
		return fmt.Errorf("orch/controller: phase %q in recipe %q has no agents", phase.Name, r.Name)
	}
	agentIdx := min(attempt-1, len(phase.Agents)-1)
	agent := phase.Agents[agentIdx]

	brief := c.buildBrief(ctx, ln, r, phase, extraNote)
	actionKey := phaseActionKey(tool, ln.ID, phase.Name, round, attempt)

	_, already, err := c.recordDecision(ctx, tool, ln.ID, reason,
		fmt.Sprintf("%s round %d attempt %d (%s:%s)", phase.Name, round, attempt, agent.Harness, agent.Model),
		DecisionArgs{
			ActionKey: actionKey,
			EventID:   eventID,
			FromPhase: ln.Phase,
			ToPhase:   phase.Name,
			Phase:     phase.Name,
			Round:     round,
			Attempt:   attempt,
			Harness:   agent.Harness,
			Model:     agent.Model,
			Effort:    agent.Effort,
		},
	)
	if err != nil {
		return err
	}
	if already {
		return nil
	}

	var sessionID, terminalID string
	if ln.SessionID == "" {
		res, err := c.driver.SpawnSession(ctx, lane.SpawnRequest{
			RunID:      c.cfg.RunID,
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
			return err
		}
		sessionID = res.SessionID
		terminalID = res.TerminalID
	} else {
		res, err := c.driver.StartPhaseTerminal(ctx, lane.StartPhaseRequest{
			RunID:     c.cfg.RunID,
			LaneID:    ln.ID,
			SessionID: ln.SessionID,
			Phase:     phase.Name,
			Round:     round,
			Harness:   agent.Harness,
			Model:     agent.Model,
			Effort:    agent.Effort,
			Brief:     brief,
		})
		if err != nil {
			return err
		}
		sessionID = res.SessionID
		terminalID = res.TerminalID
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

	if ln.ItemID != "" {
		if item, err := c.store.Backlog().Get(ctx, c.cfg.RunID, ln.ItemID); err == nil && item.Status == store.BacklogOpen {
			item.Status = store.BacklogInProgress
			item.LaneID = ln.ID
			_ = c.store.Backlog().Update(ctx, *item, ActorController, "lane started")
		}
	}
	return nil
}

func (c *Controller) buildBrief(ctx context.Context, ln *lane.Lane, r *recipe.Recipe, phase recipe.Phase, extraNote string) string {
	var parts []string
	if ln.Title != "" {
		parts = append(parts, ln.Title)
	}
	if ln.ItemID != "" {
		if item, err := c.store.Backlog().Get(ctx, c.cfg.RunID, ln.ItemID); err == nil {
			if item.Description != "" {
				parts = append(parts, item.Description)
			}
			if item.Acceptance != "" {
				parts = append(parts, "Acceptance: "+item.Acceptance)
			}
		}
	}
	if phase.BriefTemplate != "" {
		parts = append(parts, phase.BriefTemplate)
	}
	if extraNote != "" {
		parts = append(parts, extraNote)
	}
	brief := strings.Join(parts, "\n\n")
	if r.Limits.MaxBriefBytes > 0 && len(brief) > r.Limits.MaxBriefBytes {
		brief = brief[:r.Limits.MaxBriefBytes]
	}
	return brief
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
	if err == nil {
		return r, nil
	}
	if errors.Is(err, store.ErrNotFound) && name == recipe.DefaultName {
		def := recipe.Default()
		return &def, nil
	}
	return nil, err
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
	return nil
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
