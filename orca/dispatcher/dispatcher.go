package dispatcher

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/orca/brief"
	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/mailbox"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
)

const (
	ActorDispatcher       = "dispatcher"
	DefaultModel          = "claude-sonnet-5-5"
	DefaultHardBatchModel = "claude-opus-4-6"
	DefaultMaxTurns       = model.DefaultMaxTurns
	DefaultMaxTokens      = model.DefaultMaxTokens
	DefaultMaxReadBytes   = 4096
	ownerAskRefPrefix     = "owner_ask:"
	plannerRefPrefix      = "planner_request:"
)

var (
	_ controller.Judge = (*Dispatcher)(nil)
	_ Advisor          = (*Dispatcher)(nil)
)

// RecipeCompiler compiles a natural-language recipe override against a base recipe.
type RecipeCompiler interface {
	CompileOverride(ctx context.Context, base recipe.Recipe, override string) (recipe.Recipe, error)
}

// RecipeCompilerFunc adapts a function to RecipeCompiler.
type RecipeCompilerFunc func(ctx context.Context, base recipe.Recipe, override string) (recipe.Recipe, error)

func (f RecipeCompilerFunc) CompileOverride(ctx context.Context, base recipe.Recipe, override string) (recipe.Recipe, error) {
	return f(ctx, base, override)
}

// Config configures a Dispatcher instance.
type Config struct {
	RecipeCompiler   RecipeCompiler
	PRProbe          controller.PRProbe
	OnPlannerRequest func(ctx context.Context, runID, reason, planRef string) error
	OnOwnerAnswer    func(ctx context.Context, runID, askID, answer string) error
	LaneInspector    func(ctx context.Context, runID, laneID string, req ReadLaneParams) (string, error)
	OnRunCloseout    func(ctx context.Context, runID string, params RunCloseoutParams) (string, error)
	// Logger, when set, receives one INFO line per tool call.
	Logger *slog.Logger
	// Temperature is the sampling temperature for judgment turns. Nil omits it
	// from the request: newer models reject the parameter outright.
	Temperature      *float64
	Investigate      model.InvestigateConfig
	AgentBackend     model.AgentBackend
	Model            string
	Provider         string
	Effort           string
	LLMGatewayURL    string
	ClaudeBin        string
	CodexBin         string
	PromptVersion    string
	HardBatchModel   string
	SystemPrompt     string
	ThreadID         string
	AllowedModels    []string
	Mailboxes        []mailbox.Mailbox
	Invariants       []Invariant
	Snapshot         SnapshotConfig
	MaxTurns         int
	MaxTokens        int
	MaxBriefBytes    int
	MaxReadBytes     int
	MaxSessionTokens int
	MaxSessionTurns  int
	// AllowMultiAction lifts the one-primary-action-per-batch contract
	// (checkOnePrimaryAction). Production leaves it false; tests that script
	// every tool into a single batch set it.
	AllowMultiAction bool
}

func (c Config) withDefaults() Config {
	if c.AgentBackend == "" {
		c.AgentBackend = model.AgentBackendLoop
	}
	c.Model = cmp.Or(strings.TrimSpace(c.Model), DefaultModel)
	c.HardBatchModel = cmp.Or(strings.TrimSpace(c.HardBatchModel), DefaultHardBatchModel)
	if c.SystemPrompt == "" {
		if c.AgentBackend == model.AgentBackendCLI {
			c.SystemPrompt = CLISystemPrompt
		} else {
			c.SystemPrompt = SystemPrompt
		}
	}
	if strings.TrimSpace(c.PromptVersion) == "" {
		if c.AgentBackend == model.AgentBackendCLI {
			c.PromptVersion = CLISystemPromptVersion
		} else {
			c.PromptVersion = SystemPromptVersion
		}
	}
	if len(c.Invariants) == 0 {
		c.Invariants = DefaultInvariants()
	}
	if c.MaxTurns <= 0 {
		c.MaxTurns = DefaultMaxTurns
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = DefaultMaxTokens
	}
	if c.MaxBriefBytes <= 0 {
		c.MaxBriefBytes = DefaultMaxBriefBytes
	}
	if c.MaxReadBytes <= 0 {
		c.MaxReadBytes = DefaultMaxReadBytes
	}
	c.Snapshot = c.Snapshot.withDefaults()
	return c
}

// BatchResult captures the outcome of a single Dispatcher judgment batch turn.
type BatchResult struct {
	ThreadID  string
	TurnID    string
	Model     string
	Summary   string
	Decisions []store.Decision
	Accepted  []ProposedAction
	Rejected  []string
	Snapshot  Snapshot
	Usage     model.Usage
	TurnCount int
}

// Dispatcher handles debounced judgment batches from the Controller by building
// a compact Store snapshot, running a bounded tool loop on ModelClient (or a
// long-lived CLI agent session), vetting every tool call against deterministic
// invariants, and logging decisions.
type Dispatcher struct {
	store              store.Store
	driver             lane.LaneDriver
	clock              clock.Clock
	client             model.ModelClient
	lastMemoryIndex    map[string]string
	contractBroadcasts map[string]map[string]bool
	sessions           model.AgentSessionMap
	lastResult         *BatchResult
	cfg                Config
	seq                int64
	mu                 sync.Mutex
}

// New constructs a new Dispatcher.
func New(
	cfg Config,
	st store.Store,
	drv lane.LaneDriver,
	clk clock.Clock,
	client model.ModelClient,
) *Dispatcher {
	if clk == nil {
		clk = clock.NewRealClock()
	}
	return &Dispatcher{
		cfg:                cfg.withDefaults(),
		store:              st,
		driver:             drv,
		clock:              clk,
		client:             client,
		lastMemoryIndex:    make(map[string]string),
		contractBroadcasts: make(map[string]map[string]bool),
	}
}

// Session returns the active CLI AgentSession for runID, if any.
func (d *Dispatcher) Session(runID string) *model.AgentSession {
	return d.sessions.Session(runID)
}

// CloseRun stops and removes the CLI AgentSession for runID, if any.
func (d *Dispatcher) CloseRun(runID string) error {
	d.mu.Lock()
	delete(d.lastMemoryIndex, runID)
	delete(d.contractBroadcasts, runID)
	d.mu.Unlock()
	return d.sessions.CloseRun(runID)
}

// Close stops all active CLI AgentSessions managed by this Dispatcher.
func (d *Dispatcher) Close() error {
	d.mu.Lock()
	clear(d.lastMemoryIndex)
	clear(d.contractBroadcasts)
	d.mu.Unlock()
	return d.sessions.Close()
}

// SetOwnerAnswer installs the controller-owned resolution path for owner.answer.
func (d *Dispatcher) SetOwnerAnswer(fn func(context.Context, string, string, string) error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cfg.OnOwnerAnswer = fn
}

// SetRunCloseout installs the close-out handler for run.closeout on adopted runs.
func (d *Dispatcher) SetRunCloseout(fn func(context.Context, string, RunCloseoutParams) (string, error)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cfg.OnRunCloseout = fn
}

func (d *Dispatcher) runCloseoutHandler() func(context.Context, string, RunCloseoutParams) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cfg.OnRunCloseout
}

// SetDriver replaces the lane driver used for spawns, retries, steers, and stops.
func (d *Dispatcher) SetDriver(drv lane.LaneDriver) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.driver = drv
}

func (d *Dispatcher) laneDriver() lane.LaneDriver {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.driver
}

// SetMailboxes installs the run mailboxes used to answer or reply to lane mail.
func (d *Dispatcher) SetMailboxes(boxes ...mailbox.Mailbox) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cfg.Mailboxes = slices.Clone(boxes)
}

func (d *Dispatcher) mailboxes() []mailbox.Mailbox {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.cfg.Mailboxes)
}

// Judge implements controller.Judge by executing a fresh Dispatcher turn for events.
func (d *Dispatcher) Judge(ctx context.Context, runID string, events []store.Event) error {
	_, err := d.Dispatch(ctx, runID, events)
	return err
}

// Propose implements Advisor by running one model turn over snapshot and decoding
// the proposed tool calls into a Proposal without mutating Store state.
func (d *Dispatcher) Propose(ctx context.Context, snapshot string) (Proposal, error) {
	if d.client == nil {
		return Proposal{}, errors.New("orca/dispatcher: model client is required")
	}
	resp, err := d.client.CreateMessage(ctx, model.Request{
		Model:     d.cfg.Model,
		System:    d.cfg.SystemPrompt,
		Messages:  []model.Message{model.NewUserMessage(snapshot)},
		Tools:     d.ToolDefinitions(),
		MaxTokens: d.cfg.MaxTokens,
	})
	if err != nil {
		return Proposal{}, err
	}
	calls := resp.ToolCalls()
	out := Proposal{
		Unresolved: strings.TrimSpace(resp.Text()),
		Actions:    make([]ProposedAction, 0, len(calls)),
	}
	for i, tc := range calls {
		callID := cmp.Or(tc.ID, fmt.Sprintf("act-%d", i+1))
		act, pErr := ParseAction(callID, tc.Name, tc.Input)
		if pErr != nil {
			return out, pErr
		}
		out.Actions = append(out.Actions, act)
	}
	return out, nil
}

// LastResult returns the most recent BatchResult produced by Dispatch or Judge.
func (d *Dispatcher) LastResult() *BatchResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastResult == nil {
		return nil
	}
	cp := *d.lastResult
	cp.Decisions = slices.Clone(d.lastResult.Decisions)
	cp.Accepted = slices.Clone(d.lastResult.Accepted)
	cp.Rejected = slices.Clone(d.lastResult.Rejected)
	return &cp
}

// BuildSnapshot constructs a compact Store snapshot using the Dispatcher's configuration.
func (d *Dispatcher) BuildSnapshot(ctx context.Context, runID string, events []store.Event) (Snapshot, error) {
	return BuildSnapshot(ctx, d.store, d.clock, runID, events, d.cfg.Snapshot)
}

// ToolDefinitions returns the canonical tool definitions exposed to ModelClient.
func (d *Dispatcher) ToolDefinitions() []model.ToolDefinition {
	out := make([]model.ToolDefinition, len(dispatcherToolDefs))
	for i := range dispatcherToolDefs {
		out[i] = dispatcherToolDefs[i].Clone()
	}
	return out
}

func (d *Dispatcher) nextID(prefix string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seq++
	return fmt.Sprintf("%s-%d", prefix, d.seq)
}

func (d *Dispatcher) syncSeq(ctx context.Context, runID, threadID string) {
	var maxObserved int64
	if turns, err := d.store.Threads().ListTurns(ctx, threadID); err == nil {
		maxObserved = max(maxObserved, int64(len(turns)))
		for i := range turns {
			maxObserved = max(maxObserved, int64(turns[i].Seq), trailingSeqNumber(turns[i].ID))
		}
	}
	if dirs, err := d.store.Directives().List(ctx, store.DirectiveFilter{RunID: runID}); err == nil {
		maxObserved = max(maxObserved, int64(len(dirs)))
		for i := range dirs {
			maxObserved = max(maxObserved, trailingSeqNumber(dirs[i].ID), trailingSeqNumber(dirs[i].Ref))
		}
	}
	d.mu.Lock()
	if maxObserved > d.seq {
		d.seq = maxObserved
	}
	d.mu.Unlock()
}

func trailingSeqNumber(id string) int64 {
	idx := strings.LastIndexByte(id, '-')
	if idx < 0 || idx+1 >= len(id) {
		return 0
	}
	n, err := strconv.ParseInt(id[idx+1:], 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

//nolint:govet // state groups are ordered by execution concerns.
type turnExecState struct {
	view                  *StateView
	pinnedRecipes         map[string]recipe.Recipe
	autoRevokedDirectives map[string]store.Directive
	turnUsage             map[int]model.Usage
	turnUsed              map[int]bool
	events                []store.Event
	threadID              string
	turnID                string
	decisions             []store.Decision
	accepted              []ProposedAction
	rejected              []string
	currentTurn           int
	nextActionSeq         int
	traceID               string
	input                 string
	model                 string
}

// Dispatch executes one judgment batch turn and returns its structured BatchResult.
func (d *Dispatcher) Dispatch(ctx context.Context, runID string, events []store.Event) (*BatchResult, error) {
	if d.store == nil {
		return nil, errors.New("orca/dispatcher: store is required")
	}
	if d.cfg.AgentBackend != model.AgentBackendCLI && d.client == nil {
		return nil, errors.New("orca/dispatcher: model client is required")
	}

	threadID, err := d.ensureThread(ctx, runID)
	if err != nil {
		return nil, err
	}
	d.syncSeq(ctx, runID, threadID)

	turnID := d.nextID(threadID + "-turn")
	turnRec, err := d.store.Threads().StartTurn(ctx, store.Turn{
		ID:       turnID,
		ThreadID: threadID,
		RunID:    runID,
		Input:    fmt.Sprintf("judgment batch (%d events)", len(events)),
	})
	if err != nil {
		return nil, err
	}

	cleanupCtx := context.WithoutCancel(ctx)
	d.syncRunContract(ctx, runID)

	snapCfg := d.cfg.Snapshot
	if len(turnRec.Steers) > 0 {
		snapCfg.Steers = append(slices.Clone(snapCfg.Steers), turnRec.Steers...)
	}

	snap, err := BuildSnapshot(ctx, d.store, d.clock, runID, events, snapCfg)
	if err != nil {
		_ = d.store.Threads().CompleteTurn(cleanupCtx, threadID, turnID, store.TurnStatusFailed, model.Usage{}, err.Error())
		for _, steerMsg := range turnRec.Steers {
			_ = d.store.Threads().SteerTurn(cleanupCtx, threadID, "", steerMsg)
		}
		return nil, err
	}

	if _, err := d.store.Threads().AppendItem(ctx, store.Item{
		ThreadID: threadID,
		TurnID:   turnID,
		Kind:     store.ItemKindSnapshot,
		Content:  snap.Text,
	}); err != nil {
		_ = d.store.Threads().CompleteTurn(cleanupCtx, threadID, turnID, store.TurnStatusFailed, model.Usage{}, err.Error())
		for _, steerMsg := range turnRec.Steers {
			_ = d.store.Threads().SteerTurn(cleanupCtx, threadID, "", steerMsg)
		}
		return nil, err
	}

	state := &turnExecState{
		threadID:      threadID,
		turnID:        turnID,
		pinnedRecipes: make(map[string]recipe.Recipe),
		turnUsage:     make(map[int]model.Usage),
		turnUsed:      make(map[int]bool),
		events:        events,
		traceID:       turnID,
		input:         snap.Text,
	}

	reg := d.buildToolRegistry(runID, state)
	modelName := d.selectModel(events, snap.Done)
	state.model = modelName

	var (
		usage     model.Usage
		turnCount int
		summary   string
		loopErr   error
	)

	if d.shouldSkipDispatcherTurn(ctx, runID, snap.Done, events, turnRec.Steers) {
		summary = "No action needed."
		_, _ = d.store.Threads().AppendItem(cleanupCtx, store.Item{
			ThreadID: threadID,
			TurnID:   turnID,
			Kind:     store.ItemKindAssistantMessage,
			Content:  summary,
		})
	} else if d.cfg.AgentBackend == model.AgentBackendCLI {
		usage, turnCount, summary, loopErr = d.dispatchCLITurn(ctx, cleanupCtx, runID, threadID, turnID, modelName, snap.Text, events, state, snap.Done)
	} else {
		req := model.Request{
			Model:       modelName,
			System:      d.systemPromptWithMemories(ctx, runID),
			Messages:    []model.Message{model.NewUserMessage(snap.Text)},
			Tools:       toolsForBatch(reg.Definitions(), events),
			MaxTokens:   d.cfg.MaxTokens,
			Temperature: d.cfg.Temperature,
		}

		var followedUp bool
		var loopRes *model.ToolLoopResult
		loopRes, loopErr = model.RunToolLoop(
			ctx,
			retryOnTruncationClient{
				inner: d.client,
				followUp: func(callCtx context.Context, resp *model.Response) string {
					if followedUp {
						return ""
					}
					nudge := d.followUpSpawnNudge(callCtx, runID, events, state, resp)
					if nudge != "" {
						followedUp = true
					}
					return nudge
				},
			},
			req,
			reg,
			model.WithMaxTurns(d.cfg.MaxTurns),
			model.WithToolCallLog(d.cfg.Logger, ActorDispatcher),
			model.WithOnTurn(func(turn int, resp model.Response) {
				state.currentTurn = turn
				state.turnUsage[turn] = resp.Usage
				if txt := strings.TrimSpace(resp.Text()); txt != "" {
					_, _ = d.store.Threads().AppendItem(cleanupCtx, store.Item{
						ThreadID: threadID,
						TurnID:   turnID,
						Kind:     store.ItemKindAssistantMessage,
						Content:  txt,
					})
				}
				for _, tc := range resp.ToolCalls() {
					if !reg.Has(tc.Name) {
						toolName := tc.Name
						reg.Register(model.ToolDefinition{Name: toolName}, func(callCtx context.Context, raw json.RawMessage) (string, error) {
							return d.handleRawToolCall(callCtx, runID, state, toolName, raw)
						})
					}
				}
			}),
			model.WithOnToolCall(func(_ int, call model.ToolCall, resBlock model.ContentBlock) {
				_, _ = d.store.Threads().AppendItem(cleanupCtx, store.Item{
					ThreadID: threadID,
					TurnID:   turnID,
					Kind:     store.ItemKindToolCall,
					Name:     call.Name,
					CallID:   call.ID,
					Payload:  call.Input,
				})
				_, _ = d.store.Threads().AppendItem(cleanupCtx, store.Item{
					ThreadID: threadID,
					TurnID:   turnID,
					Kind:     store.ItemKindToolResult,
					Name:     call.Name,
					CallID:   call.ID,
					Content:  resBlock.Content,
					IsError:  resBlock.IsError,
				})
			}),
		)
		if loopRes != nil {
			usage = loopRes.Usage
			turnCount = loopRes.TurnCount
			summary = strings.TrimSpace(loopRes.Response.Text())
		}
	}

	turnStatus := store.TurnStatusCompleted
	errMsg := ""
	if loopErr != nil {
		turnStatus = store.TurnStatusFailed
		errMsg = loopErr.Error()
	}
	if compErr := d.store.Threads().CompleteTurn(cleanupCtx, threadID, turnID, turnStatus, usage, errMsg); compErr != nil && loopErr == nil {
		loopErr = compErr
	}
	if loopErr == nil {
		d.staffIdleObjectiveReadyWork(cleanupCtx, runID, events, state)
		d.dismissStaleBatchQuestions(cleanupCtx, runID, events, state, summary)
	}

	res := &BatchResult{
		ThreadID:  threadID,
		TurnID:    turnID,
		Model:     modelName,
		Summary:   summary,
		Snapshot:  snap,
		Decisions: state.decisions,
		Accepted:  state.accepted,
		Rejected:  state.rejected,
		Usage:     usage,
		TurnCount: turnCount,
	}

	d.mu.Lock()
	d.lastResult = res
	d.mu.Unlock()

	if loopErr != nil {
		return res, loopErr
	}
	return res, nil
}

func (d *Dispatcher) memoryIndexSection(ctx context.Context, runID string) string {
	ms, err := d.store.Memories().List(ctx, store.MemoryFilter{RunID: runID})
	if err != nil {
		return ""
	}
	ms = store.FilterActiveMemories(ctx, d.store, runID, ms, d.clock.Now().UTC())
	return "\n\n## Memory Index\n" + store.MemoryIndex(ms)
}

func (d *Dispatcher) systemPromptWithMemories(ctx context.Context, runID string) string {
	return d.cfg.SystemPrompt + d.memoryIndexSection(ctx, runID)
}

func (d *Dispatcher) ensureThread(ctx context.Context, runID string) (string, error) {
	threadID := strings.TrimSpace(d.cfg.ThreadID)
	if threadID == "" {
		threadID = "dispatcher-" + runID
	}
	if th, err := d.store.Threads().GetThread(ctx, threadID); err == nil && th != nil {
		return th.ID, nil
	}
	goal := "Orchestrate backlog and lanes for run " + runID
	if run, err := d.store.Runs().Get(ctx, runID); err == nil && run != nil && run.Objective != "" {
		goal = run.Objective
	}
	th, err := d.store.Threads().StartThread(ctx, store.Thread{
		ID:     threadID,
		RunID:  runID,
		Role:   store.ThreadRoleDispatcher,
		Goal:   goal,
		Status: store.ThreadStatusIdle,
	})
	if err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return threadID, nil
		}
		return "", err
	}
	return th.ID, nil
}

func (d *Dispatcher) selectModel(events []store.Event, doneRun bool) string {
	if d.cfg.HardBatchModel != "" && isHardBatch(events) && !doneRun {
		return d.cfg.HardBatchModel
	}
	return d.cfg.Model
}

func (d *Dispatcher) shouldSkipDispatcherTurn(ctx context.Context, runID string, doneRun bool, events []store.Event, steers []string) bool {
	if !doneRun || len(steers) > 0 || len(events) == 0 {
		return false
	}
	if slices.ContainsFunc(events, isOwnerOriginEvent) {
		return false
	}
	if d.store != nil {
		if dirs, err := d.store.Directives().List(ctx, store.DirectiveFilter{RunID: runID, ActiveOnly: true}); err == nil && len(dirs) > 0 {
			return false
		}
	}
	return true
}

// toolsForBatch drops the read.* tools from a batch that names no lane: they
// only expand a lane's truncated excerpt, and a model offered them anyway
// invents a lane ID. mail.answer is offered only when the batch carries lane mail.
func toolsForBatch(defs []model.ToolDefinition, events []store.Event) []model.ToolDefinition {
	hasLane := batchNamesLane(events)
	hasLaneMail := batchHasLaneMail(events)
	if hasLane && hasLaneMail {
		return defs
	}
	return slices.DeleteFunc(slices.Clone(defs), func(def model.ToolDefinition) bool {
		if !hasLane && (isReadOnlyTool(def.Name) || def.Name == ToolLaneForward) {
			return true
		}
		if !hasLaneMail && (def.Name == ToolMailAnswer || def.Name == ToolMailDismiss) {
			return true
		}
		return false
	})
}

// batchNamesLane reports whether any event of the batch is about a lane.
func batchNamesLane(events []store.Event) bool {
	return slices.ContainsFunc(events, func(ev store.Event) bool { return strings.TrimSpace(ev.LaneID) != "" })
}

// batchHasLaneMail reports whether any event in the batch is an inbound lane mail message.
func batchHasLaneMail(events []store.Event) bool {
	return len(extractBatchLaneMailsIncludeClosed(events)) > 0
}

func isHardBatch(events []store.Event) bool {
	if len(events) >= 4 {
		return true
	}
	for i := range events {
		if events[i].Urgent {
			return true
		}
	}
	return false
}

var dispatcherToolDefs = []model.ToolDefinition{
	{Name: ToolBacklogAdd, Description: "Add a brand-new prioritized item to the run backlog only when no matching item is already listed in ## Backlog. Never call backlog.add for an item ID or work already shown in ## Backlog (call lane.spawn alone with that existing item_id)", InputSchema: model.SchemaFor[BacklogAddParams]()},
	{Name: ToolBacklogUpdate, Description: "Edit an existing open or blocked backlog item (title, acceptance, priority, dependencies, open/blocked). Never sets in_progress: lane.spawn does that", InputSchema: model.SchemaFor[BacklogUpdateParams]()},
	{Name: ToolBacklogReorder, Description: "Reorder backlog items within their priority tier", InputSchema: model.SchemaFor[BacklogReorderParams]()},
	{Name: ToolBacklogClose, Description: "Close a backlog item as done or dropped", InputSchema: model.SchemaFor[BacklogCloseParams]()},
	{Name: ToolLaneSpawn, Description: "Spawn a worker lane for an open, ready backlog item. This also marks the item in_progress, so it is the only call needed to start work on it", InputSchema: model.SchemaFor[LaneSpawnParams]()},
	{Name: ToolLaneSteer, Description: "Send a steering note to an active or blocked lane", InputSchema: model.SchemaFor[LaneSteerParams]()},
	{Name: ToolLaneForward, Description: "Forward a finding or report excerpt from a source lane to another active or blocked target lane as a steering note", InputSchema: model.SchemaFor[LaneForwardParams]()},
	{Name: ToolLaneRetry, Description: "Retry a stalled, blocked, or failed lane on a fresh round", InputSchema: model.SchemaFor[LaneRetryParams]()},
	{Name: ToolLaneStop, Description: "Stop a non-terminal lane and mark it failed", InputSchema: model.SchemaFor[LaneStopParams]()},
	{Name: ToolLaneWait, Description: "Record a named expected wait on a lane (e.g. cloud VM run, sweep, or merge gate) with a reason and deadline so the controller suppresses stall judgments until the deadline", InputSchema: model.SchemaFor[LaneWaitParams]()},
	{Name: ToolOwnerAsk, Description: "Pose a decision to the owner that only the owner can make (product scope, spend, credentials, one-way/destructive policy) and that the batch presents as still open, with options, a mandatory default, and a deadline. Never use it for engineering, ordering, retry, or reversible workflow-dispatch/deploy timing choices covered by a runbook and rollback (e.g. HOST-ASK dispatch vs hold), or for work the owner already ordered", InputSchema: model.SchemaFor[OwnerAskParams]()},
	{Name: ToolOwnerAnswer, Description: "If the owner's message answers one of the listed open owner asks, resolve it with that ask's id; otherwise treat the message as work", InputSchema: model.SchemaFor[OwnerAnswerParams]()},
	{Name: ToolDirectiveNote, Description: "Persist a standing rule, preference, inter-agent status/agreed context, or engineering/deploy-timing decision (including resolving a reversible HOST-ASK dispatch/hold choice) that later batches need and that no backlog item, lane or other call in this batch carries. At most one per batch, holding every point in its fact. Not a log of your own actions or questions", InputSchema: model.SchemaFor[DirectiveNoteParams]()},
	{Name: ToolDirectiveRevoke, Description: "Revoke or supersede an active standing directive or instruction by ID when the owner lifts, clears, or replaces it", InputSchema: model.SchemaFor[DirectiveRevokeParams]()},
	{Name: ToolPlannerRequest, Description: "Request a Planner turn to replan or import a plan. The reason is delivered to the Planner and also stored as an active directive, so it carries any standing rule given with the request", InputSchema: model.SchemaFor[PlannerRequestParams]()},
	{Name: ToolReadLaneReport, Description: "Read a bounded phase report for a lane", InputSchema: model.SchemaFor[ReadLaneReportParams]()},
	{Name: ToolReadLane, Description: "Inspect raw files, directory listings, or read-only command output in a lane's session/worktree or pushed branch to verify claims from raw evidence before integration", InputSchema: model.SchemaFor[ReadLaneParams]()},
	{Name: ToolReadPR, Description: "Read bounded PR metadata and approval status for a lane", InputSchema: model.SchemaFor[ReadPRParams]()},
	{Name: ToolReadCI, Description: "Read bounded CI check status and recent CI events for a lane", InputSchema: model.SchemaFor[ReadCIParams]()},
	{Name: "memory.read", Description: "Read the full body of a memory by ID", InputSchema: model.SchemaFor[struct {
		ID string `json:"id"`
	}]()},
}

var mailAnswerToolDef = model.ToolDefinition{
	Name:        ToolMailAnswer,
	Description: "Answer a lane mail question or reply to a lane mail note from what orca knows (run objective, backlog, prior reports). When only the owner can decide, call owner.ask instead",
	InputSchema: model.SchemaFor[MailAnswerParams](),
}

var mailDismissToolDef = model.ToolDefinition{
	Name:        ToolMailDismiss,
	Description: "Dismiss or close an inbound worker-lane question (with message_id or lane_id) that is stale, withdrawn, or obsolete (e.g. lane already done/merged) so it leaves the open questions list without escalating to the owner. Never use this for run-level host/inter-agent mail",
	InputSchema: model.SchemaFor[MailDismissParams](),
}

var runCloseoutToolDef = model.ToolDefinition{
	Name:        ToolRunCloseout,
	Description: "Close out an adopted run once every lane and backlog item is terminal: write FINAL.md, update PROGRESS.md and OBJECTIVE.md Next:, append to a plan log file named in OBJECTIVE.md, republish a Nebula post named in OBJECTIVE.md, and release remaining lane sessions",
	InputSchema: model.SchemaFor[RunCloseoutParams](),
}

func (d *Dispatcher) buildToolRegistry(runID string, state *turnExecState) *model.ToolRegistry {
	reg := model.NewToolRegistry()
	for i := range dispatcherToolDefs {
		def := dispatcherToolDefs[i]
		reg.Register(def, func(ctx context.Context, raw json.RawMessage) (string, error) {
			return d.handleRawToolCall(ctx, runID, state, def.Name, raw)
		})
	}
	reg.Register(mailAnswerToolDef, func(ctx context.Context, raw json.RawMessage) (string, error) {
		return d.handleRawToolCall(ctx, runID, state, mailAnswerToolDef.Name, raw)
	})
	reg.Register(mailDismissToolDef, func(ctx context.Context, raw json.RawMessage) (string, error) {
		return d.handleRawToolCall(ctx, runID, state, mailDismissToolDef.Name, raw)
	})
	if d.runCloseoutHandler() != nil {
		reg.Register(runCloseoutToolDef, func(ctx context.Context, raw json.RawMessage) (string, error) {
			return d.handleRawToolCall(ctx, runID, state, runCloseoutToolDef.Name, raw)
		})
	}
	return reg
}

func batchAllowsTool(toolName string, events []store.Event) error {
	if (isReadOnlyTool(toolName) || toolName == ToolLaneForward) && !batchNamesLane(events) {
		return fmt.Errorf("%s is only available when the judgment batch contains a lane event", toolName)
	}
	if (toolName == ToolMailAnswer || toolName == ToolMailDismiss) && !batchHasLaneMail(events) {
		return fmt.Errorf("%s is only available when the judgment batch contains inbound lane mail", toolName)
	}
	return nil
}

// buildCLIToolRegistry registers the full Dispatcher tool superset so the
// long-lived CLI session's MCP tool list stays constant across run-level and
// lane-level batches (avoiding subprocess restarts), while enforcing batch-level
// tool eligibility inside the handler.
func (d *Dispatcher) buildCLIToolRegistry(runID string, state *turnExecState, events []store.Event, doneRun bool) *model.ToolRegistry {
	reg := model.NewToolRegistry()
	defs := make([]model.ToolDefinition, 0, len(dispatcherToolDefs)+3)
	defs = append(defs, dispatcherToolDefs...)
	defs = append(defs, mailAnswerToolDef, mailDismissToolDef)
	if d.runCloseoutHandler() != nil && !doneRun {
		defs = append(defs, runCloseoutToolDef)
	}
	for i := range defs {
		def := defs[i]
		reg.Register(def, func(ctx context.Context, raw json.RawMessage) (string, error) {
			if err := batchAllowsTool(def.Name, events); err != nil {
				return "", err
			}
			res, err := d.handleRawToolCall(ctx, runID, state, def.Name, raw)
			if err != nil || d.cfg.AllowMultiAction || isReadOnlyTool(def.Name) || def.Name == ToolLaneForward || def.Name == "memory.read" {
				return res, err
			}
			if isFanOutTool(def.Name) {
				return res + " (batch action applied; end your turn now unless combining another fan-out call [backlog_add, lane_spawn, lane_retry, directive_revoke, owner_answer] — do NOT call lane_steer, directive_note, planner_request, or owner_ask)", nil
			}
			return res + " (batch primary action applied; end your turn now — no further mutating calls are permitted in this batch)", nil
		})
	}
	if !doneRun {
		model.RegisterInvestigationTools(reg, d.cfg.Investigate)
	}
	return reg
}

func (d *Dispatcher) dispatchCLITurn(
	ctx context.Context,
	cleanupCtx context.Context,
	runID string,
	threadID string,
	turnID string,
	modelName string,
	promptText string,
	events []store.Event,
	state *turnExecState,
	doneRun bool,
) (model.Usage, int, string, error) {
	cliReg := d.buildCLIToolRegistry(runID, state, events, doneRun)
	memSection := d.memoryIndexSection(ctx, runID)
	turnPrompt := promptText
	if d.shouldIncludeMemoryIndex(runID, memSection) {
		turnPrompt += memSection
	}

	turnRes, err := store.RunRoleAgentTurn(
		ctx,
		d.store.Threads(),
		&d.sessions,
		runID,
		threadID,
		turnID,
		ActorDispatcher,
		model.AgentSessionConfig{
			Provider:         d.cfg.Provider,
			Model:            modelName,
			Effort:           d.cfg.Effort,
			SystemPrompt:     d.cfg.SystemPrompt,
			WorkDir:          model.DeterministicSessionWorkDir("", runID, ActorDispatcher),
			LLMGatewayURL:    d.cfg.LLMGatewayURL,
			ClaudeBin:        d.cfg.ClaudeBin,
			CodexBin:         d.cfg.CodexBin,
			MaxSessionTokens: d.cfg.MaxSessionTokens,
			MaxSessionTurns:  d.cfg.MaxSessionTurns,
		},
		model.AgentTurnRequest{
			Prompt:       turnPrompt,
			Model:        modelName,
			SystemPrompt: d.cfg.SystemPrompt,
			Registry:     cliReg,
			MaxToolCalls: d.cfg.MaxTurns,
		},
		d.cfg.Logger,
	)
	if err != nil {
		return model.Usage{}, 0, "", err
	}
	d.recordSentMemoryIndex(runID, memSection, turnRes.Rotated)
	if turnRes.Text != "" {
		_, _ = d.store.Threads().AppendItem(cleanupCtx, store.Item{
			ThreadID: threadID,
			TurnID:   turnID,
			Kind:     store.ItemKindAssistantMessage,
			Content:  turnRes.Text,
		})
	}
	store.AttributeTurnUsage(cleanupCtx, d.store.Decisions(), runID, state.decisions, turnRes.Usage)
	return turnRes.Usage, turnRes.TurnCount, turnRes.Text, nil
}

func (d *Dispatcher) shouldIncludeMemoryIndex(runID, memSection string) bool {
	if memSection == "" {
		return false
	}
	sess := d.sessions.Session(runID)
	if sess == nil || sess.TurnCount() == 0 {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	last, ok := d.lastMemoryIndex[runID]
	return !ok || last != memSection
}

func (d *Dispatcher) recordSentMemoryIndex(runID, memSection string, rotated bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastMemoryIndex == nil {
		d.lastMemoryIndex = make(map[string]string)
	}
	if rotated && memSection == "" {
		delete(d.lastMemoryIndex, runID)
		return
	}
	d.lastMemoryIndex[runID] = memSection
}

type invariantViolationError struct {
	msg string
}

func (e *invariantViolationError) Error() string {
	return e.msg
}

func (d *Dispatcher) handleRawToolCall(
	ctx context.Context,
	runID string,
	state *turnExecState,
	toolName string,
	raw json.RawMessage,
) (string, error) {
	if toolName == "memory.read" {
		var p struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return "", err
		}
		m, err := d.store.Memories().Get(ctx, "", strings.TrimSpace(p.ID))
		if err != nil {
			return "", err
		}
		if m.RunID != "" && m.RunID != runID {
			return "", store.ErrNotFound
		}
		return fmt.Sprintf("%s\n%s", m.Name, m.Body), nil
	}
	start := d.clock.Now()
	var callID string
	if state != nil {
		state.nextActionSeq++
		callID = fmt.Sprintf("act-%d", state.nextActionSeq)
	}

	act, parseErr := ParseAction(callID, toolName, raw)
	if parseErr != nil {
		rejMsg := fmt.Sprintf("%s [run]: invalid arguments — %v", NormalizeToolName(toolName), parseErr)
		if state != nil {
			state.rejected = append(state.rejected, rejMsg)
		}
		_ = d.recordDecision(ctx, runID, state, act, false, "rejected: "+rejMsg, d.clock.Now().Sub(start))
		return "", errors.New(rejMsg)
	}

	var view *StateView
	if state != nil && state.view != nil {
		view = state.view
	} else {
		var pinned map[string]recipe.Recipe
		if state != nil {
			pinned = state.pinnedRecipes
		}
		var err error
		view, err = loadStateViewWithCache(ctx, d.store, d.clock, runID, d.cfg.MaxBriefBytes, pinned)
		if err != nil {
			return "", err
		}
		if state != nil {
			for k := range state.autoRevokedDirectives {
				if view.RevokedInTurn == nil {
					view.RevokedInTurn = make(map[string]store.Directive)
				}
				view.RevokedInTurn[k] = state.autoRevokedDirectives[k]
				delete(view.Directives, k)
			}
			state.view = view
		}
	}
	view.AllowedModels = slices.Clone(d.cfg.AllowedModels)

	if vetErr := VetAction(view, act, d.cfg.Invariants); vetErr != nil {
		rejMsg := vetErr.Error()
		if state != nil {
			state.rejected = append(state.rejected, rejMsg)
		}
		_ = d.recordDecision(ctx, runID, state, act, false, "rejected: "+rejMsg, d.clock.Now().Sub(start))
		return "", fmt.Errorf("invariant violation: %s", rejMsg)
	}

	if state != nil && !d.cfg.AllowMultiAction {
		if oneErr := checkOnePrimaryAction(state.accepted, act); oneErr != nil {
			rejMsg := fmt.Sprintf("%s [%s]: one primary action per batch — %v. The batch is answered: do not retry or substitute another mutating call, end your turn", act.Tool, act.Target(), oneErr)
			state.rejected = append(state.rejected, rejMsg)
			_ = d.recordDecision(ctx, runID, state, act, false, "rejected: "+rejMsg, d.clock.Now().Sub(start))
			return "", fmt.Errorf("invariant violation: %s", rejMsg)
		}
	}

	result, execErr := d.applyVettedAction(ctx, runID, state, view, act)
	if execErr != nil {
		if state != nil && !isReadOnlyTool(act.Tool) {
			state.view = nil
		}
		var invErr *invariantViolationError
		if errors.As(execErr, &invErr) {
			if state != nil {
				state.rejected = append(state.rejected, invErr.msg)
			}
			_ = d.recordDecision(ctx, runID, state, act, false, "rejected: "+invErr.msg, d.clock.Now().Sub(start))
			return "", fmt.Errorf("invariant violation: %s", invErr.msg)
		}
		_ = d.recordDecision(ctx, runID, state, act, true, "error: "+execErr.Error(), d.clock.Now().Sub(start))
		return "", execErr
	}

	if state != nil {
		if !isReadOnlyTool(act.Tool) {
			state.view = nil
		}
		state.accepted = append(state.accepted, act)
	}
	if err := d.recordDecision(ctx, runID, state, act, true, result, d.clock.Now().Sub(start)); err != nil {
		return "", err
	}
	return result, nil
}

// isFanOutTool reports whether a tool may repeat or combine within a batch:
// the work that is naturally plural and independent per target.
func isFanOutTool(tool string) bool {
	return tool == ToolBacklogAdd || tool == ToolLaneSpawn || tool == ToolLaneRetry || tool == ToolLaneWait ||
		tool == ToolDirectiveRevoke || tool == ToolOwnerAnswer || tool == ToolMailDismiss ||
		tool == ToolLaneForward
}

// checkOnePrimaryAction enforces one primary action per batch: the first
// mutating call wins, and any further one is rejected (visibly, so the model
// and the decision log see it) unless both are fan-out tools. Read tools are
// never limited.
func checkOnePrimaryAction(accepted []ProposedAction, act ProposedAction) error {
	if isReadOnlyTool(act.Tool) || act.Tool == ToolLaneForward {
		return nil
	}
	for i := range accepted {
		prev := accepted[i]
		if isReadOnlyTool(prev.Tool) || prev.Tool == ToolLaneForward || (isFanOutTool(prev.Tool) && isFanOutTool(act.Tool)) {
			continue
		}
		return fmt.Errorf("this batch already applied %s [%s]; answer a batch with a single primary action (only %s, %s, %s, %s and %s may combine)",
			prev.Tool, prev.Target(), ToolBacklogAdd, ToolLaneSpawn, ToolLaneRetry, ToolDirectiveRevoke, ToolOwnerAnswer)
	}
	return nil
}

func isReadOnlyTool(tool string) bool {
	return tool == ToolReadLaneReport || tool == ToolReadLane || tool == ToolReadPR || tool == ToolReadCI
}

func (d *Dispatcher) recordDecision(
	ctx context.Context,
	runID string,
	state *turnExecState,
	act ProposedAction,
	vetted bool,
	result string,
	elapsed time.Duration,
) error {
	var (
		threadID string
		turnID   string
		usage    model.Usage
	)
	if state != nil {
		threadID = state.threadID
		turnID = state.turnID
		if !state.turnUsed[state.currentTurn] {
			usage = state.turnUsage[state.currentTurn]
			state.turnUsed[state.currentTurn] = true
		}
	}

	traceID, modelName, input := "", "", ""
	if state != nil {
		traceID, modelName, input = state.traceID, state.model, state.input
	}
	if act.Tool == ToolLaneSpawn && act.LaneSpawn != nil && strings.TrimSpace(act.LaneSpawn.Model) != "" {
		modelName = resolveDecisionWorkerModel(act.LaneSpawn.Model, result)
	} else if act.Tool == ToolLaneRetry && act.LaneRetry != nil && strings.TrimSpace(act.LaneRetry.Model) != "" {
		modelName = resolveDecisionWorkerModel(act.LaneRetry.Model, result)
	}
	dec, err := d.store.Decisions().Append(context.WithoutCancel(ctx), store.Decision{
		RunID:         runID,
		Category:      store.DecisionJudgment,
		Actor:         ActorDispatcher,
		ThreadID:      threadID,
		TurnID:        turnID,
		LaneID:        act.LaneID,
		Tool:          cmp.Or(act.Tool, "unknown"),
		Reason:        cmp.Or(act.Reason, result),
		Result:        result,
		Args:          act.Args,
		Vetted:        vetted,
		InputTokens:   usage.InputTokens,
		OutputTokens:  usage.OutputTokens,
		CostUSD:       usage.CostUSD,
		LatencyMs:     max(elapsed.Milliseconds(), 0),
		TraceID:       traceID,
		Model:         modelName,
		Input:         input,
		Output:        result,
		Provider:      d.cfg.Provider,
		PromptVersion: d.cfg.PromptVersion,
		OutcomeLabel:  outcomeLabel(result),
	})
	if err != nil {
		return err
	}
	if state != nil {
		state.decisions = append(state.decisions, dec)
	}
	return nil
}

func outcomeLabel(result string) string {
	s := strings.ToLower(result)
	switch {
	case strings.Contains(s, "owner corrected"), strings.Contains(s, "corrected"):
		return "owner corrected"
	case strings.Contains(s, "merged"):
		return "merged"
	case strings.Contains(s, "reworked"), strings.Contains(s, "retry"):
		return "reworked"
	case strings.Contains(s, "abandoned"), strings.Contains(s, "dropped"):
		return "abandoned"
	default:
		return ""
	}
}

func resolveDecisionWorkerModel(rawModel, result string) string {
	if _, after, ok := strings.Cut(result, "model="); ok {
		if resolved := strings.TrimSpace(strings.TrimSuffix(after, ")")); resolved != "" {
			return resolved
		}
	}
	return ResolveWorkerModel(rawModel, "")
}

func (d *Dispatcher) applyVettedAction(
	ctx context.Context,
	runID string,
	state *turnExecState,
	view *StateView,
	act ProposedAction,
) (string, error) {
	switch act.Tool {
	case ToolBacklogAdd:
		return d.applyBacklogAdd(ctx, runID, *act.BacklogAdd)
	case ToolBacklogUpdate:
		return d.applyBacklogUpdate(ctx, runID, view, *act.BacklogUpdate)
	case ToolBacklogReorder:
		return d.applyBacklogReorder(ctx, runID, *act.BacklogReorder)
	case ToolBacklogClose:
		return d.applyBacklogClose(ctx, runID, *act.BacklogClose)
	case ToolLaneSpawn:
		return d.applyLaneSpawn(ctx, runID, view, *act.LaneSpawn)
	case ToolLaneSteer:
		return d.applyLaneSteer(ctx, runID, view, *act.LaneSteer)
	case ToolLaneForward:
		return d.applyLaneForward(ctx, runID, view, *act.LaneForward)
	case ToolMailAnswer:
		return d.applyMailAnswer(ctx, runID, state, view, *act.MailAnswer)
	case ToolMailDismiss:
		return d.applyMailDismiss(ctx, runID, state, view, *act.MailDismiss)
	case ToolLaneRetry:
		return d.applyLaneRetry(ctx, runID, view, *act.LaneRetry)
	case ToolLaneStop:
		return d.applyLaneStop(ctx, runID, view, *act.LaneStop)
	case ToolLaneWait:
		return d.applyLaneWait(ctx, runID, view, *act.LaneWait)
	case ToolOwnerAsk:
		return d.applyOwnerAsk(ctx, runID, state, view, *act.OwnerAsk)
	case ToolOwnerAnswer:
		if d.cfg.OnOwnerAnswer == nil {
			return "", errors.New("owner answer handler is not configured")
		}
		return fmt.Sprintf("resolved owner ask %s", act.OwnerAnswer.AskID), d.cfg.OnOwnerAnswer(ctx, runID, act.OwnerAnswer.AskID, act.OwnerAnswer.Answer)
	case ToolDirectiveNote:
		return d.applyDirectiveNote(ctx, runID, state, *act.DirectiveNote)
	case ToolDirectiveRevoke:
		return d.applyDirectiveRevoke(ctx, runID, state, view, *act.DirectiveRevoke)
	case ToolPlannerRequest:
		return d.applyPlannerRequest(ctx, runID, *act.PlannerRequest)
	case ToolRunCloseout:
		fn := d.runCloseoutHandler()
		if fn == nil {
			return "", errors.New("run closeout handler is not configured")
		}
		return fn(ctx, runID, *act.RunCloseout)
	case ToolReadLaneReport:
		return d.applyReadLaneReport(ctx, runID, view, *act.ReadLaneReport)
	case ToolReadLane:
		return d.applyReadLane(ctx, runID, view, *act.ReadLane)
	case ToolReadPR:
		return d.applyReadPR(ctx, runID, view, *act.ReadPR)
	case ToolReadCI:
		return d.applyReadCI(ctx, runID, view, *act.ReadCI)
	default:
		return "", fmt.Errorf("unsupported tool %q", act.Tool)
	}
}

func (d *Dispatcher) applyBacklogAdd(ctx context.Context, runID string, p BacklogAddParams) (string, error) {
	prio := store.Priority(p.Priority)
	if prio == "" {
		prio = store.PriorityP2
	}
	item := store.BacklogItem{
		ID:          strings.TrimSpace(p.ID),
		RunID:       runID,
		Title:       strings.TrimSpace(p.Title),
		Description: strings.TrimSpace(p.Description),
		Acceptance:  strings.TrimSpace(p.Acceptance),
		Priority:    prio,
		Status:      store.BacklogOpen,
		RecipeName:  strings.TrimSpace(p.RecipeName),
		DependsOn:   slices.Clone(p.DependsOn),
		Tags:        slices.Clone(p.Tags),
		Order:       p.Order,
		NoMerge:     p.NoMerge,
	}
	if !item.NoMerge && store.IsNoMergeBacklogItem(item) {
		item.NoMerge = true
	}
	if err := d.store.Backlog().Add(ctx, item); err != nil {
		return "", err
	}
	if len(item.DependsOn) == 0 {
		return fmt.Sprintf("added backlog item %s (%s, open ready — call %s with item_id=%q to spawn its lane now)", item.ID, item.Priority, ToolLaneSpawn, item.ID), nil
	}
	return fmt.Sprintf("added backlog item %s (%s)", item.ID, item.Priority), nil
}

func (d *Dispatcher) applyBacklogUpdate(ctx context.Context, runID string, view *StateView, p BacklogUpdateParams) (string, error) {
	existing, ok := view.BacklogItem(p.ID)
	if !ok {
		return "", fmt.Errorf("%w: backlog item %q", store.ErrNotFound, p.ID)
	}
	updated := existing.Clone()
	if strings.TrimSpace(p.Title) != "" {
		updated.Title = strings.TrimSpace(p.Title)
	}
	if p.Description != "" {
		updated.Description = strings.TrimSpace(p.Description)
	}
	if p.Acceptance != "" {
		updated.Acceptance = strings.TrimSpace(p.Acceptance)
	}
	if p.Priority != "" {
		updated.Priority = store.Priority(p.Priority)
	}
	if p.Status != "" {
		updated.Status = store.BacklogStatus(p.Status)
	}
	if p.RecipeName != "" {
		updated.RecipeName = strings.TrimSpace(p.RecipeName)
	}
	if p.DependsOn != nil {
		updated.DependsOn = slices.Clone(p.DependsOn)
	}
	if p.Tags != nil {
		updated.Tags = slices.Clone(p.Tags)
	}
	if p.NoMerge != nil {
		updated.NoMerge = *p.NoMerge
	} else if !existing.NoMerge && (strings.TrimSpace(p.Title) != "" || p.Description != "" || p.Acceptance != "") && store.IsNoMergeBacklogItem(updated) {
		updated.NoMerge = true
	}
	if err := d.store.Backlog().Update(ctx, updated, ActorDispatcher, strings.TrimSpace(p.Reason)); err != nil {
		return "", err
	}
	return fmt.Sprintf("updated backlog item %s", updated.ID), nil
}

func (d *Dispatcher) applyBacklogReorder(ctx context.Context, runID string, p BacklogReorderParams) (string, error) {
	if err := d.store.Backlog().Reorder(ctx, runID, p.ItemIDs, ActorDispatcher); err != nil {
		return "", err
	}
	return fmt.Sprintf("reordered %d backlog items", len(p.ItemIDs)), nil
}

func (d *Dispatcher) applyBacklogClose(ctx context.Context, runID string, p BacklogCloseParams) (string, error) {
	status := store.BacklogStatus(p.Status)
	if err := d.store.Backlog().Close(ctx, runID, p.ID, status, ActorDispatcher, strings.TrimSpace(p.Reason)); err != nil {
		return "", err
	}
	return fmt.Sprintf("closed backlog item %s as %s", p.ID, status), nil
}

func (d *Dispatcher) applyLaneSpawn(ctx context.Context, runID string, view *StateView, p LaneSpawnParams) (string, error) {
	item, ok := view.BacklogItem(p.ItemID)
	if !ok {
		if synth, found := view.ObjectiveMentionsReadyItem(p.ItemID); found {
			if addErr := d.store.Backlog().Add(ctx, synth); addErr != nil && !errors.Is(addErr, store.ErrAlreadyExists) {
				return "", addErr
			}
			view.Backlog[synth.ID] = synth
			item = synth
			ok = true
		}
	}
	if !ok {
		return "", fmt.Errorf("%w: backlog item %q", store.ErrNotFound, p.ItemID)
	}
	laneID := cmp.Or(strings.TrimSpace(p.LaneID), item.ID)

	base, resolved, _ := view.ResolveSpawnRecipe(p)
	r := base
	noMerge := store.IsNoMergeSpawn(p.NoMerge, &item, p.Brief, p.RecipeOverride)

	override := strings.TrimSpace(p.RecipeOverride)
	if override != "" {
		if resolved.Version > 0 {
			r = resolved
		} else {
			var compiled recipe.Recipe
			if d.cfg.RecipeCompiler != nil {
				var err error
				compiled, err = d.cfg.RecipeCompiler.CompileOverride(ctx, base, override)
				if err != nil {
					return "", err
				}
			} else {
				return "", fmt.Errorf("natural-language recipe override %q requires a recipe compiler", override)
			}
			if noMerge {
				compiled = recipe.WithoutIntegrate(compiled)
			}
			compiled.RunID = runID
			compiled.Name = fmt.Sprintf("override-%s", laneID)
			compiled.Version = 0
			if maxBytes := view.EffectiveMaxBriefBytes(compiled); maxBytes > 0 && len(p.Brief) > maxBytes {
				return "", &invariantViolationError{
					msg: fmt.Sprintf("%s [%s]: brief size within cap — brief size %d bytes exceeds cap of %d bytes",
						ToolLaneSpawn, laneID, len(p.Brief), maxBytes),
				}
			}
			if compiled.MergePolicy.Mode != recipe.MergeModeSelf && containsDirectMergeInstruction(p.Brief) {
				return "", &invariantViolationError{
					msg: fmt.Sprintf("%s [%s]: no merge outside recipe — brief instructs lane to merge directly, which is forbidden under recipe %q (merge_mode=%s)",
						ToolLaneSpawn, laneID, compiled.Name, compiled.MergePolicy.Mode),
				}
			}
			saved, err := d.store.Recipes().Save(ctx, compiled)
			if err != nil {
				return "", err
			}
			r = saved
		}
	}
	if noMerge {
		r = recipe.WithoutIntegrate(r)
	}

	firstPhaseName := r.FirstPhase()
	firstPhase, hasFirst := r.Phase(firstPhaseName)
	if !hasFirst {
		return "", fmt.Errorf("recipe %q has no phases", r.Name)
	}

	now := d.clock.Now().UTC()
	ln := lane.Lane{
		ID:            laneID,
		RunID:         runID,
		ItemID:        item.ID,
		Title:         item.Title,
		Branch:        strings.TrimSpace(p.Branch),
		BaseBranch:    strings.TrimSpace(p.BaseBranch),
		Status:        lane.StatusPlanned,
		Phase:         firstPhase.Name,
		Round:         1,
		Attempt:       1,
		RecipeName:    r.Name,
		RecipeVersion: max(r.Version, 1),
		CreatedAt:     now,
		UpdatedAt:     now,
		LastEventAt:   now,
	}
	if existing, exists := view.Lane(laneID); exists {
		ln = existing.Clone()
		ln.ItemID = item.ID
		ln.Title = cmp.Or(item.Title, ln.Title)
		ln.Branch = cmp.Or(strings.TrimSpace(p.Branch), ln.Branch)
		ln.BaseBranch = cmp.Or(strings.TrimSpace(p.BaseBranch), ln.BaseBranch)
		ln.RecipeName = r.Name
		ln.RecipeVersion = max(r.Version, 1)
		ln.Phase = firstPhase.Name
		ln.Round = ln.MaxRound(firstPhase.Name) + 1
		ln.Attempt = 1
		ln.LastEventAt = now
	}
	if settled, msg, err := d.reconcileLanePRState(ctx, runID, &ln); settled || err != nil {
		return msg, err
	}

	workerBrief, err := d.buildWorkerBrief(view, ln, r, firstPhase, ln.Round, p.Brief)
	if err != nil {
		return "", err
	}

	selectedModel := strings.TrimSpace(p.Model)
	if selectedModel != "" {
		var harness string
		if len(firstPhase.Agents) > 0 {
			harness = firstPhase.Agents[0].Harness
		}
		selectedModel = ResolveWorkerModel(selectedModel, harness)
	}
	if drv := d.laneDriver(); drv != nil && len(firstPhase.Agents) > 0 {
		agent := firstPhase.Agents[0]
		if selectedModel != "" {
			agent.Model = selectedModel
		}
		spawnRes, err := d.spawnLaneSession(ctx, drv, runID, view.Run.Repository, ln, firstPhase.Name, ln.Round, agent, workerBrief)
		if err != nil {
			return "", err
		}
		ln.Status = lane.StatusRunning
		if strings.TrimSpace(spawnRes.BaseCommit) != "" {
			ln.BaseCommit = strings.TrimSpace(spawnRes.BaseCommit)
		}
		ln.RecordAttempt(firstPhase.Name, ln.Round, spawnRes.SessionID, spawnRes.TerminalID)
	}
	d.markContractKnownForLane(runID, ln.ID, view.Directives)

	if err := d.store.Lanes().Upsert(ctx, ln); err != nil {
		return "", err
	}

	updatedItem := item.Clone()
	updatedItem.Status = store.BacklogInProgress
	updatedItem.LaneID = ln.ID
	if noMerge {
		updatedItem.NoMerge = true
	}
	if override != "" {
		updatedItem.RecipeName = r.Name
	}
	if err := d.store.Backlog().Update(ctx, updatedItem, ActorDispatcher, fmt.Sprintf("spawned lane %s", ln.ID)); err != nil {
		return "", err
	}

	if selectedModel != "" {
		return fmt.Sprintf("spawned lane %s for item %s on %s (round %d, recipe %s@v%d, model=%s)",
			ln.ID, item.ID, ln.Phase, ln.Round, ln.RecipeName, ln.RecipeVersion, selectedModel), nil
	}
	return fmt.Sprintf("spawned lane %s for item %s on %s (round %d, recipe %s@v%d)",
		ln.ID, item.ID, ln.Phase, ln.Round, ln.RecipeName, ln.RecipeVersion), nil
}

func (d *Dispatcher) reconcileLanePRState(ctx context.Context, runID string, ln *lane.Lane) (bool, string, error) {
	settled, state, err := controller.ReconcileLanePRState(ctx, d.cfg.PRProbe, d.store, d.laneDriver(), d.clock.Now(), runID, ActorDispatcher, "", ln)
	if !settled || err != nil {
		return settled, "", err
	}
	return true, fmt.Sprintf("lane %s PR #%d already %s; stopped terminals", ln.ID, ln.PR, strings.ToLower(state)), nil
}

func (d *Dispatcher) applyLaneSteer(ctx context.Context, runID string, view *StateView, p LaneSteerParams) (string, error) {
	ln, ok := view.Lane(p.LaneID)
	if !ok {
		return "", fmt.Errorf("%w: lane %q", store.ErrNotFound, p.LaneID)
	}
	if settled, msg, err := d.reconcileLanePRState(ctx, runID, &ln); settled || err != nil {
		return msg, err
	}
	if drv := d.laneDriver(); drv != nil {
		if err := drv.Steer(ctx, lane.SteerRequest{
			RunID:      runID,
			LaneID:     ln.ID,
			SessionID:  ln.SessionID,
			TerminalID: ln.TerminalID,
			Message:    p.Note,
		}); err != nil {
			return "", err
		}
	}
	if ln.Status == lane.StatusBlocked {
		ln.Status = lane.StatusRunning
	}
	ln.LastEventAt = d.clock.Now().UTC()
	if err := d.store.Lanes().Upsert(ctx, ln); err != nil {
		return "", err
	}
	return fmt.Sprintf("steered lane %s", ln.ID), nil
}

func (d *Dispatcher) applyLaneForward(ctx context.Context, runID string, view *StateView, p LaneForwardParams) (string, error) {
	target, ok := view.Lane(p.ToLaneID)
	if !ok {
		return "", fmt.Errorf("%w: lane %q", store.ErrNotFound, p.ToLaneID)
	}
	if settled, msg, err := d.reconcileLanePRState(ctx, runID, &target); settled || err != nil {
		return msg, err
	}
	from := strings.TrimSpace(p.FromLaneID)
	if from != "" {
		if _, ok := view.Lane(from); !ok {
			return "", fmt.Errorf("%w: source lane %q", store.ErrNotFound, from)
		}
	}
	finding := strings.TrimSpace(p.Excerpt)
	msg := finding
	if from != "" {
		msg = fmt.Sprintf("Forwarded finding from lane %s: %s", from, finding)
	}
	if drv := d.laneDriver(); drv != nil {
		if err := drv.Steer(ctx, lane.SteerRequest{
			RunID:      runID,
			LaneID:     target.ID,
			SessionID:  target.SessionID,
			TerminalID: target.TerminalID,
			Message:    msg,
		}); err != nil {
			return "", err
		}
	}
	if target.Status == lane.StatusBlocked {
		target.Status = lane.StatusRunning
	}
	target.LastEventAt = d.clock.Now().UTC()
	if err := d.store.Lanes().Upsert(ctx, target); err != nil {
		return "", err
	}
	if from != "" {
		return fmt.Sprintf("forwarded finding from lane %s to lane %s", from, target.ID), nil
	}
	return fmt.Sprintf("forwarded finding to lane %s", target.ID), nil
}

func (d *Dispatcher) applyLaneRetry(ctx context.Context, runID string, view *StateView, p LaneRetryParams) (string, error) {
	ln, ok := view.Lane(p.LaneID)
	if !ok {
		return "", fmt.Errorf("%w: lane %q", store.ErrNotFound, p.LaneID)
	}
	if settled, msg, err := d.reconcileLanePRState(ctx, runID, &ln); settled || err != nil {
		return msg, err
	}
	r, ok := view.RecipeForLane(ln)
	if !ok {
		r = recipe.Default()
	}
	targetPhaseName := cmp.Or(strings.TrimSpace(p.Phase), ln.Phase, r.FirstPhase())
	targetPhase, ok := r.Phase(targetPhaseName)
	if !ok {
		return "", fmt.Errorf("unknown phase %q in recipe %q", targetPhaseName, r.Name)
	}

	round := p.Round
	if round <= 0 {
		round = ln.MaxRound(targetPhase.Name) + 1
	}

	workerBrief, err := d.buildWorkerBrief(view, ln, r, targetPhase, round, p.Note)
	if err != nil {
		return "", err
	}
	var sessionID, terminalID string

	selectedModel := strings.TrimSpace(p.Model)
	if selectedModel != "" {
		var harness string
		if len(targetPhase.Agents) > 0 {
			harness = targetPhase.Agents[0].Harness
		}
		selectedModel = ResolveWorkerModel(selectedModel, harness)
	}
	if drv := d.laneDriver(); drv != nil && len(targetPhase.Agents) > 0 {
		agent := targetPhase.Agents[0]
		if selectedModel != "" {
			agent.Model = selectedModel
		}
		if ln.SessionID == "" {
			res, err := d.spawnLaneSession(ctx, drv, runID, view.Run.Repository, ln, targetPhase.Name, round, agent, workerBrief)
			if err != nil {
				return "", err
			}
			sessionID = res.SessionID
			terminalID = res.TerminalID
			if strings.TrimSpace(res.BaseCommit) != "" {
				ln.BaseCommit = strings.TrimSpace(res.BaseCommit)
			}
		} else {
			res, err := drv.StartPhaseTerminal(ctx, lane.StartPhaseRequest{
				RunID:      runID,
				Repository: view.Run.Repository,
				LaneID:     ln.ID,
				SessionID:  ln.SessionID,
				Phase:      targetPhase.Name,
				Round:      round,
				Harness:    agent.Harness,
				Model:      agent.Model,
				Effort:     agent.Effort,
				Brief:      workerBrief,
			})
			if err != nil {
				return "", err
			}
			sessionID = res.SessionID
			terminalID = res.TerminalID
		}
	} else {
		sessionID = cmp.Or(ln.SessionID, fmt.Sprintf("sess-%s-r%d", ln.ID, round))
		terminalID = fmt.Sprintf("term-%s-%s", ln.ID, lane.PhaseRoundKey(targetPhase.Name, round))
	}
	d.markContractKnownForLane(runID, ln.ID, view.Directives)

	ln.Status = lane.StatusRunning
	ln.Attempt = 1
	ln.RecordAttempt(targetPhase.Name, round, sessionID, terminalID)
	ln.LastEventAt = d.clock.Now().UTC()
	if err := d.store.Lanes().Upsert(ctx, ln); err != nil {
		return "", err
	}
	if selectedModel != "" {
		return fmt.Sprintf("retried lane %s in phase %s (round %d, model=%s)", ln.ID, targetPhase.Name, round, selectedModel), nil
	}
	return fmt.Sprintf("retried lane %s in phase %s (round %d)", ln.ID, targetPhase.Name, round), nil
}

func (d *Dispatcher) spawnLaneSession(
	ctx context.Context,
	drv lane.LaneDriver,
	runID string,
	repository string,
	ln lane.Lane,
	phase string,
	round int,
	agent recipe.AgentSpec,
	brief string,
) (lane.SpawnResult, error) {
	return drv.SpawnSession(ctx, lane.SpawnRequest{
		RunID:      runID,
		Repository: repository,
		LaneID:     ln.ID,
		Phase:      phase,
		Round:      round,
		Harness:    agent.Harness,
		Model:      agent.Model,
		Effort:     agent.Effort,
		Brief:      brief,
		Branch:     ln.Branch,
		BaseBranch: ln.BaseBranch,
		Worktree:   ln.Worktree,
	})
}

// buildWorkerBrief is the single dispatcher path for briefs sent to a driver.
// Keeping the report contract here ensures initial spawns and retries have the
// same handback instructions as controller-started phase terminals.
func (d *Dispatcher) buildWorkerBrief(view *StateView, ln lane.Lane, r recipe.Recipe, phase recipe.Phase, round int, extra string) (string, error) {
	drv := d.laneDriver()
	raw := d.buildRetryBriefText(view, ln, phase, extra)
	instructions := ""
	if drv != nil {
		instructions = drv.ReportInstructions(ln.RunID, ln.ID, phase.Name, round)
	}
	var dirs []store.Directive
	var memories []store.MemoryEntry
	if view != nil {
		for id := range view.Directives {
			dirs = append(dirs, view.Directives[id])
		}
		memories = view.Memories
	}
	text, err := brief.RenderWithContract(raw, dirs, memories, 0)
	if err != nil {
		return "", err
	}
	return lane.AppendReportInstructions(text, instructions, view.EffectiveMaxBriefBytes(r)), nil
}

func (d *Dispatcher) buildRetryBriefText(view *StateView, ln lane.Lane, phase recipe.Phase, note string) string {
	var parts []string
	if ln.Title != "" {
		parts = append(parts, ln.Title)
	}
	mechanical := phase.Name == recipe.PhaseClean || phase.Name == recipe.PhaseReview
	if ln.ItemID != "" && !mechanical {
		if item, ok := view.BacklogItem(ln.ItemID); ok {
			if item.Description != "" {
				parts = append(parts, item.Description)
			}
			if item.Acceptance != "" {
				parts = append(parts, "Acceptance: "+item.Acceptance)
			}
		}
	}
	var skill string
	if drv := d.laneDriver(); drv != nil {
		skill = drv.PhaseSkill(phase.Name)
	}
	if skill != "" {
		parts = []string{skill}
		if ln.Title != "" {
			parts = append(parts, "Goal: "+ln.Title)
		}
	} else if phase.BriefTemplate != "" {
		parts = append(parts, phase.BriefTemplate)
	}
	if strings.TrimSpace(note) != "" {
		parts = append(parts, strings.TrimSpace(note))
	}
	return strings.Join(parts, "\n\n")
}

func (d *Dispatcher) applyLaneStop(ctx context.Context, runID string, view *StateView, p LaneStopParams) (string, error) {
	ln, ok := view.Lane(p.LaneID)
	if !ok {
		return "", fmt.Errorf("%w: lane %q", store.ErrNotFound, p.LaneID)
	}
	if drv := d.laneDriver(); drv != nil && (ln.SessionID != "" || ln.TerminalID != "") {
		if err := drv.Stop(ctx, lane.StopRequest{
			RunID:      runID,
			LaneID:     ln.ID,
			SessionID:  ln.SessionID,
			TerminalID: ln.TerminalID,
			Reason:     p.Reason,
		}); err != nil {
			return "", err
		}
	}
	ln.Status = lane.StatusFailed
	ln.LastEventAt = d.clock.Now().UTC()
	if err := d.store.Lanes().Upsert(ctx, ln); err != nil {
		return "", err
	}
	if ln.ItemID != "" {
		if item, hasItem := view.BacklogItem(ln.ItemID); hasItem && item.Status == store.BacklogInProgress {
			item.Status = store.BacklogBlocked
			_ = d.store.Backlog().Update(ctx, item, ActorDispatcher, "lane stopped: "+p.Reason)
		}
	}
	return fmt.Sprintf("stopped lane %s (%s)", ln.ID, p.Reason), nil
}

func (d *Dispatcher) applyLaneWait(ctx context.Context, runID string, view *StateView, p LaneWaitParams) (string, error) {
	ln, ok := view.Lane(p.LaneID)
	if !ok {
		return "", fmt.Errorf("%w: lane %q", store.ErrNotFound, p.LaneID)
	}
	now := d.clock.Now().UTC()
	dl, err := parseFutureTime(now, p.Deadline)
	if err != nil {
		return "", err
	}
	ref := controller.NamedWaitRefPrefix + ln.ID
	if existing, listErr := d.store.Directives().List(ctx, store.DirectiveFilter{
		RunID:      runID,
		Scope:      ln.ID,
		ActiveOnly: true,
	}); listErr == nil {
		for i := range existing {
			if existing[i].Ref == ref {
				_ = d.store.Directives().UpdateStatus(ctx, runID, existing[i].ID, store.DirectiveHandled)
			}
		}
	}
	dir := store.Directive{
		ID:        d.nextID("wait"),
		RunID:     runID,
		Kind:      store.DirectiveInstruction,
		Source:    store.DirectiveSourceDispatcher,
		Scope:     ln.ID,
		Ref:       ref,
		Text:      strings.TrimSpace(p.Reason),
		ExpiresAt: &dl,
		Status:    store.DirectiveActive,
	}
	if err := d.store.Directives().Add(ctx, dir); err != nil {
		return "", err
	}
	return fmt.Sprintf("recorded expected wait on lane %s until %s (%s)", ln.ID, dl.Format(time.RFC3339), dir.Text), nil
}

func (d *Dispatcher) applyOwnerAsk(ctx context.Context, runID string, state *turnExecState, view *StateView, p OwnerAskParams) (string, error) {
	now := d.clock.Now().UTC()
	dl, err := parseFutureTime(now, p.Deadline)
	if err != nil {
		return "", err
	}
	askID := strings.TrimSpace(p.ID)
	if askID == "" {
		askID = d.nextID("ask")
	}

	var sourceEventID, sourceMessageID string
	laneID := strings.TrimSpace(p.LaneID)
	msgID := strings.TrimSpace(p.MessageID)
	var batchEvents []store.Event
	if state != nil {
		batchEvents = state.events
	}
	bm, hasQuestion, matchErr := d.resolveLaneMailTarget(ctx, runID, batchEvents, msgID, laneID, false, func(m batchLaneMail) bool {
		return m.msg.IsQuestion()
	})
	if matchErr != nil {
		return "", matchErr
	}
	if hasQuestion {
		sourceEventID = bm.event.ID
		sourceMessageID = bm.msg.ID
		if laneID == "" {
			laneID = bm.msg.LaneID
		}
		if mailbox.IsQuestionObsoleteOrClosed(ctx, d.store, runID, laneID, bm.msg.ID, bm.event.ID, bm.msg.Body) {
			reason := cmp.Or(strings.TrimSpace(p.Reason), "stale or obsolete lane question")
			if err := d.dismissBatchQuestion(ctx, runID, bm, reason); err != nil {
				return "", err
			}
			return fmt.Sprintf("dismissed stale lane question %s for lane %s (%s)", cmp.Or(bm.msg.ID, bm.event.ID, "-"), cmp.Or(laneID, "-"), reason), nil
		}
	}

	ask := controller.OwnerAsk{
		ID:              askID,
		RunID:           runID,
		LaneID:          laneID,
		Question:        strings.TrimSpace(p.Question),
		Options:         slices.Clone(p.Options),
		Default:         strings.TrimSpace(p.Default),
		SteerNote:       strings.TrimSpace(p.SteerNote),
		Deadline:        dl,
		SourceEventID:   sourceEventID,
		SourceMessageID: sourceMessageID,
	}
	if err := ask.Validate(); err != nil {
		return "", err
	}

	raw, err := json.Marshal(ask)
	if err != nil {
		return "", err
	}
	dir := store.Directive{
		ID:        "ask-" + ask.ID,
		RunID:     runID,
		Kind:      store.DirectiveInstruction,
		Source:    store.DirectiveSourceDispatcher,
		Scope:     ask.LaneID,
		Ref:       ownerAskRefPrefix + ask.ID,
		Text:      string(raw),
		ExpiresAt: &dl,
		Status:    store.DirectiveActive,
	}
	if err := d.store.Directives().Add(ctx, dir); err != nil {
		return "", err
	}

	if ask.LaneID != "" {
		if ln, ok := view.Lane(ask.LaneID); ok && ln.Status == lane.StatusRunning {
			ln.Status = lane.StatusBlocked
			ln.LastEventAt = now
			if err := d.store.Lanes().Upsert(ctx, ln); err != nil {
				return "", err
			}
		}
	}
	return fmt.Sprintf("registered owner ask %s (default=%q deadline=%s)", ask.ID, ask.Default, dl.Format(time.RFC3339)), nil
}

func (d *Dispatcher) applyDirectiveNote(ctx context.Context, runID string, state *turnExecState, p DirectiveNoteParams) (string, error) {
	now := d.clock.Now().UTC()
	var expiresAt *time.Time
	if strings.TrimSpace(p.Expiry) != "" {
		exp, err := parseFutureTime(now, p.Expiry)
		if err != nil {
			return "", err
		}
		expiresAt = &exp
	}
	var batchEvents []store.Event
	if state != nil {
		batchEvents = state.events
	}
	if err := d.validateDispatcherNoteFact(ctx, runID, batchEvents, p.Fact); err != nil {
		return "", err
	}
	dirID := strings.TrimSpace(p.ID)
	if dirID == "" {
		dirID = d.nextID("note")
	}
	isContract := p.Contract ||
		strings.EqualFold(strings.TrimSpace(p.Kind), "contract") ||
		strings.EqualFold(strings.TrimSpace(p.Scope), "contract")
	kind := store.DirectiveNote
	if isContract {
		kind = store.DirectiveContract
	}
	dir := store.Directive{
		ID:        dirID,
		RunID:     runID,
		Kind:      kind,
		Contract:  isContract,
		Source:    store.DirectiveSourceDispatcher,
		Scope:     strings.TrimSpace(p.Scope),
		Text:      strings.TrimSpace(p.Fact),
		ExpiresAt: expiresAt,
		Status:    store.DirectiveActive,
	}
	if err := d.store.Directives().Add(ctx, dir); err != nil {
		return "", err
	}
	if expiresAt != nil {
		if dir.IsContract() {
			d.syncRunContract(ctx, runID)
		}
		return fmt.Sprintf("recorded time-limited directive note %s", dir.ID), nil
	}
	if existing, getErr := d.store.Memories().Get(ctx, "", dir.ID); getErr != nil && !errors.Is(getErr, store.ErrNotFound) {
		_ = d.store.Directives().UpdateStatus(context.WithoutCancel(ctx), runID, dir.ID, store.DirectiveRevoked)
		return "", getErr
	} else if existing != nil {
		_ = d.store.Directives().UpdateStatus(context.WithoutCancel(ctx), runID, dir.ID, store.DirectiveRevoked)
		return "", fmt.Errorf("%w: memory %s already exists", store.ErrInvalidArgument, dir.ID)
	}
	scope := strings.TrimSpace(dir.Scope)
	if scope != "owner" && scope != "repo" && scope != "run" {
		scope = "run"
	}
	memRunID := runID
	if scope != "run" {
		memRunID = ""
	}
	_, err := d.store.Memories().Put(ctx, store.MemoryEntry{
		ID: dir.ID, Name: "Dispatcher directive", Description: dir.Text, Type: "rule", Scope: scope,
		RunID: memRunID, Source: dir.Text, Body: dir.Text, ProposedBy: "model",
	})
	if err != nil {
		_ = d.store.Directives().UpdateStatus(context.WithoutCancel(ctx), runID, dir.ID, store.DirectiveRevoked)
		return "", err
	}
	if dir.IsContract() {
		d.syncRunContract(ctx, runID)
	}
	return fmt.Sprintf("recorded directive note %s", dir.ID), nil
}

func contractDirectiveKey(dir *store.Directive) string {
	return dir.ID + "\x00" + strings.TrimSpace(dir.Text)
}

func (d *Dispatcher) markContractKnownForLane(runID, laneID string, directives map[string]store.Directive) {
	if laneID == "" || len(directives) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.contractBroadcasts == nil {
		d.contractBroadcasts = make(map[string]map[string]bool)
	}
	byRun := d.contractBroadcasts[runID]
	if byRun == nil {
		byRun = make(map[string]bool)
		d.contractBroadcasts[runID] = byRun
	}
	for id := range directives {
		dir := directives[id]
		if !dir.IsContract() {
			continue
		}
		byRun[contractDirectiveKey(&dir)+"\x00"+laneID] = true
	}
}

func (d *Dispatcher) syncRunContract(ctx context.Context, runID string) {
	if d.store == nil {
		return
	}
	dirs, err := d.store.Directives().List(ctx, store.DirectiveFilter{
		RunID:      runID,
		ActiveOnly: true,
	})
	if err != nil {
		if d.cfg.Logger != nil {
			d.cfg.Logger.WarnContext(ctx, "dispatcher: list directives for run contract failed", "run_id", runID, "err", err)
		}
		return
	}
	var contractDirs []store.Directive
	for i := range dirs {
		if dirs[i].IsContract() {
			contractDirs = append(contractDirs, dirs[i])
		}
	}
	if len(contractDirs) == 0 {
		return
	}
	brief.SortContractDirectives(contractDirs)
	drv := d.laneDriver()
	if drv == nil {
		return
	}
	lanes, err := d.store.Lanes().List(ctx, store.LaneFilter{RunID: runID})
	if err != nil {
		if d.cfg.Logger != nil {
			d.cfg.Logger.WarnContext(ctx, "dispatcher: list lanes for run contract failed", "run_id", runID, "err", err)
		}
		return
	}
	for i := range lanes {
		ln := lanes[i]
		if ln.Status != lane.StatusRunning {
			continue
		}
		var unsent []store.Directive
		d.mu.Lock()
		if d.contractBroadcasts == nil {
			d.contractBroadcasts = make(map[string]map[string]bool)
		}
		byRun := d.contractBroadcasts[runID]
		if byRun == nil {
			byRun = make(map[string]bool)
			d.contractBroadcasts[runID] = byRun
		}
		for j := range contractDirs {
			if !byRun[contractDirectiveKey(&contractDirs[j])+"\x00"+ln.ID] {
				unsent = append(unsent, contractDirs[j])
			}
		}
		d.mu.Unlock()
		if len(unsent) == 0 {
			continue
		}
		msg := brief.FormatRunContract(unsent)
		if msg == "" {
			continue
		}
		if sErr := drv.Steer(ctx, lane.SteerRequest{
			RunID:      runID,
			LaneID:     ln.ID,
			SessionID:  ln.SessionID,
			TerminalID: ln.TerminalID,
			Message:    msg,
		}); sErr == nil {
			d.mu.Lock()
			if d.contractBroadcasts == nil {
				d.contractBroadcasts = make(map[string]map[string]bool)
			}
			byRun := d.contractBroadcasts[runID]
			if byRun == nil {
				byRun = make(map[string]bool)
				d.contractBroadcasts[runID] = byRun
			}
			for j := range unsent {
				byRun[contractDirectiveKey(&unsent[j])+"\x00"+ln.ID] = true
			}
			d.mu.Unlock()
		} else if d.cfg.Logger != nil {
			d.cfg.Logger.WarnContext(ctx, "dispatcher: steer run contract failed", "run_id", runID, "lane_id", ln.ID, "err", sErr)
		}
	}
}

func (d *Dispatcher) applyDirectiveRevoke(ctx context.Context, runID string, state *turnExecState, view *StateView, p DirectiveRevokeParams) (string, error) {
	id := store.NormalizeDirectiveID(p.ID)
	if id == "" {
		return "", fmt.Errorf("%w: directive id is required", store.ErrInvalidArgument)
	}
	dir, err := d.store.Directives().Get(ctx, runID, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			if state != nil {
				if autoDir, ok := state.autoRevokedDirectives[id]; ok {
					dir = &autoDir
					err = nil
				}
			}
			if err != nil && view != nil {
				if vDir, ok := view.Directives[id]; ok {
					dir = &vDir
					err = nil
				}
			}
			if err != nil {
				return "", fmt.Errorf("directive %q not found: %w", id, err)
			}
		} else {
			return "", err
		}
	}
	if state != nil && len(state.events) > 0 && !slices.ContainsFunc(state.events, func(ev store.Event) bool {
		return ev.Kind == store.EventOwnerMessage
	}) {
		if !d.canRevokeDirectiveWithoutBatchOwnerMsg(ctx, runID, view, dir) {
			return "", &invariantViolationError{msg: "directive.revoke [run]: requires an owner_msg event in the batch"}
		}
	}
	if dir != nil && strings.HasPrefix(strings.TrimSpace(dir.Ref), ownerAskRefPrefix) {
		return "", fmt.Errorf("%w: directive %q is an open owner question (%s); use owner.answer to resolve it", store.ErrInvalidArgument, id, dir.Ref)
	}
	if uErr := d.store.Directives().UpdateStatus(ctx, runID, id, store.DirectiveRevoked); uErr != nil && !errors.Is(uErr, store.ErrNotFound) {
		return "", uErr
	}
	if memErr := d.store.Memories().Delete(ctx, "", id); memErr != nil && !errors.Is(memErr, store.ErrNotFound) {
		return "", memErr
	}
	if view != nil && view.Directives != nil {
		delete(view.Directives, id)
	}
	if dir != nil {
		if view != nil {
			if view.RevokedInTurn == nil {
				view.RevokedInTurn = make(map[string]store.Directive)
			}
			view.RevokedInTurn[id] = *dir
		}
		if state != nil {
			if state.autoRevokedDirectives == nil {
				state.autoRevokedDirectives = make(map[string]store.Directive)
			}
			state.autoRevokedDirectives[id] = *dir
		}
	}
	if dir != nil && d.shouldSweepAllHoldsOnRevoke(state, *dir) && view != nil {
		for otherID := range view.Directives {
			od := view.Directives[otherID]
			if otherID == id || strings.HasPrefix(strings.TrimSpace(od.Ref), ownerAskRefPrefix) {
				continue
			}
			if od.Scope != "" && od.Scope != "run" {
				continue
			}
			isHold := isHoldDirectiveText(od.Text)
			isOwnerHoldLiftChat := strings.HasPrefix(od.ID, "chat-") && od.Source == store.DirectiveSourceOwner && ownerMessageLiftsHold(od.Text, store.Directive{})
			if !isHold && !isOwnerHoldLiftChat {
				continue
			}
			if uErr := d.store.Directives().UpdateStatus(ctx, runID, otherID, store.DirectiveRevoked); uErr != nil && !errors.Is(uErr, store.ErrNotFound) {
				return "", uErr
			}
			if memErr := d.store.Memories().Delete(ctx, "", otherID); memErr != nil && !errors.Is(memErr, store.ErrNotFound) {
				return "", memErr
			}
			delete(view.Directives, otherID)
			if view.RevokedInTurn == nil {
				view.RevokedInTurn = make(map[string]store.Directive)
			}
			view.RevokedInTurn[otherID] = od
			if state != nil {
				if state.autoRevokedDirectives == nil {
					state.autoRevokedDirectives = make(map[string]store.Directive)
				}
				state.autoRevokedDirectives[otherID] = od
			}
		}
	}
	if dir != nil && dir.IsContract() {
		d.syncRunContract(ctx, runID)
		if drv := d.laneDriver(); drv != nil {
			if lanes, listErr := d.store.Lanes().List(ctx, store.LaneFilter{RunID: runID}); listErr == nil {
				for i := range lanes {
					ln := lanes[i]
					if ln.Status.Terminal() || (ln.SessionID == "" && ln.TerminalID == "") {
						continue
					}
					_ = drv.Steer(ctx, lane.SteerRequest{RunID: runID, LaneID: ln.ID, SessionID: ln.SessionID, TerminalID: ln.TerminalID,
						Message: fmt.Sprintf("Run contract updated: directive %s was revoked.", id)})
				}
			}
		}
	}
	return fmt.Sprintf("revoked directive %s", id), nil
}

func (d *Dispatcher) shouldSweepAllHoldsOnRevoke(state *turnExecState, revoked store.Directive) bool {
	if revoked.Scope != "" && revoked.Scope != "run" {
		return false
	}
	revokedIsHoldOrLift := isHoldDirectiveText(revoked.Text) ||
		(revoked.Source == store.DirectiveSourceOwner && ownerMessageLiftsHold(revoked.Text, store.Directive{}))
	if !revokedIsHoldOrLift || state == nil {
		return false
	}
	for i := range state.events {
		ev := &state.events[i]
		if ev.Kind != store.EventOwnerMessage {
			continue
		}
		text := strings.TrimSpace(ev.Summary + " " + string(ev.Payload))
		lower := strings.ToLower(text)
		if dispatcherContainsWholeWord(lower, "keep") || dispatcherContainsWholeWord(lower, "except") || dispatcherContainsWholeWord(lower, "only") {
			return false
		}
		if ownerMessageLiftsHold(text, store.Directive{}) {
			return true
		}
	}
	return false
}

func (d *Dispatcher) canRevokeDirectiveWithoutBatchOwnerMsg(ctx context.Context, runID string, view *StateView, dir *store.Directive) bool {
	if view != nil && view.Run != nil && isObjectiveDoneText(view.Run.Objective) {
		return true
	}
	if dir == nil || !isHoldDirectiveText(dir.Text) {
		return false
	}
	return d.priorOwnerMessageLiftedHold(ctx, runID, *dir)
}

func (d *Dispatcher) priorOwnerMessageLiftedHold(ctx context.Context, runID string, dir store.Directive) bool {
	if d.store == nil {
		return false
	}
	lifted := false
	if evs, err := d.store.Events().List(ctx, store.EventFilter{
		RunID: runID,
		Kinds: []store.EventKind{store.EventOwnerMessage},
	}); err == nil {
		for i := range evs {
			ev := &evs[i]
			if !dir.CreatedAt.IsZero() && !ev.CreatedAt.IsZero() && ev.CreatedAt.Before(dir.CreatedAt) {
				continue
			}
			text := strings.TrimSpace(ev.Summary + " " + string(ev.Payload))
			if ownerMessageLiftsHold(text, dir) {
				lifted = true
			} else if isHoldDirectiveText(text) {
				lifted = false
			}
		}
	}
	if dirs, err := d.store.Directives().List(ctx, store.DirectiveFilter{RunID: runID}); err == nil {
		for i := range dirs {
			od := &dirs[i]
			if od.ID == dir.ID || od.Source != store.DirectiveSourceOwner {
				continue
			}
			if !dir.CreatedAt.IsZero() && !od.CreatedAt.IsZero() && od.CreatedAt.Before(dir.CreatedAt) {
				continue
			}
			if ownerMessageLiftsHold(od.Text, dir) {
				lifted = true
			}
		}
	}
	return lifted
}

func ownerMessageLiftsHold(text string, dir store.Directive) bool {
	if ownerTextAskedToStart(text) {
		return true
	}
	lower := strings.ToLower(stripDispatcherQuotedSpans(text))
	if dir.ID != "" && strings.Contains(lower, strings.ToLower(dir.ID)) {
		for _, w := range dispatcherHoldLiftWords {
			if dispatcherContainsWholeWord(lower, w) {
				return true
			}
		}
	}
	for _, sentence := range strings.FieldsFunc(lower, func(r rune) bool {
		return r == '.' || r == ';' || r == '\n' || r == '!'
	}) {
		sentence = strings.TrimSpace(sentence)
		if sentence == "" || strings.Contains(sentence, "?") {
			continue
		}
		for _, clause := range splitHoldClauses(sentence) {
			clause = strings.TrimSpace(clause)
			if clause == "" {
				continue
			}
			norm := normalizeContractionWords(clause)
			if dispatcherContainsWholeWord(norm, "not") ||
				dispatcherContainsWholeWord(norm, "never") ||
				dispatcherContainsWholeWord(norm, "dont") ||
				strings.Contains(norm, "keep the hold") ||
				strings.Contains(norm, "keep hold") {
				continue
			}
			if !dispatcherContainsWholeWord(norm, "hold") &&
				!dispatcherContainsWholeWord(norm, "holds") &&
				!dispatcherContainsWholeWord(norm, "pause") &&
				!dispatcherContainsWholeWord(norm, "freeze") {
				continue
			}
			for _, w := range dispatcherHoldLiftWords {
				if dispatcherContainsWholeWord(norm, w) {
					return true
				}
			}
		}
	}
	return false
}

func (d *Dispatcher) followUpSpawnNudge(
	ctx context.Context,
	runID string,
	events []store.Event,
	state *turnExecState,
	resp *model.Response,
) string {
	if state == nil || len(state.rejected) > 0 {
		return ""
	}
	for i := range state.accepted {
		switch state.accepted[i].Tool {
		case ToolBacklogAdd, ToolDirectiveRevoke, ToolOwnerAnswer:
		default:
			return ""
		}
	}
	txtLower := ""
	if resp != nil {
		txtLower = strings.ToLower(resp.Text())
	}
	claimedMissing := strings.Contains(txtLower, "lane.spawn")
	if len(state.accepted) == 0 && !claimedMissing {
		return ""
	}
	if !ownerAskedToStart(events) && !claimedMissing {
		return ""
	}
	view, err := loadStateViewWithCache(ctx, d.store, d.clock, runID, d.cfg.MaxBriefBytes, state.pinnedRecipes)
	if err != nil {
		return ""
	}
	for id := range view.Directives {
		dir := view.Directives[id]
		if strings.HasPrefix(dir.Ref, ownerAskRefPrefix) {
			continue
		}
		if isHoldDirectiveText(dir.Text) {
			return ""
		}
	}
	var readyP0 *store.BacklogItem
	for id := range view.Backlog {
		item := view.Backlog[id]
		if item.Status != store.BacklogOpen || item.Priority != store.PriorityP0 {
			continue
		}
		ready := true
		for _, depID := range item.DependsOn {
			dep, ok := view.BacklogItem(depID)
			if !ok || dep.Status != store.BacklogDone {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		if existing, hasLane := view.Lane(item.ID); hasLane && existing.Status != lane.StatusPlanned {
			continue
		}
		_, r, _ := view.ResolveSpawnRecipe(LaneSpawnParams{ItemID: item.ID, RecipeName: item.RecipeName})
		if maxConcurrent := view.EffectiveMaxConcurrentLanes(r); maxConcurrent > 0 && view.RunningLaneCount() >= maxConcurrent {
			continue
		}
		if readyP0 == nil || item.Order < readyP0.Order || (item.Order == readyP0.Order && item.ID < readyP0.ID) {
			cp := item
			readyP0 = &cp
		}
	}
	if readyP0 == nil {
		return ""
	}
	return fmt.Sprintf("The %s tool is registered and callable in this session, and backlog item %q (%s, open ready) has no lane yet. Call %s with item_id=%q and a task brief now.",
		ToolLaneSpawn, readyP0.ID, readyP0.Priority, ToolLaneSpawn, readyP0.ID)
}

func (d *Dispatcher) staffIdleObjectiveReadyWork(ctx context.Context, runID string, events []store.Event, state *turnExecState) {
	if state == nil || len(state.accepted) > 0 || len(state.rejected) > 0 {
		return
	}
	if !isIdleTick(events) {
		return
	}
	view, err := loadStateViewWithCache(ctx, d.store, d.clock, runID, d.cfg.MaxBriefBytes, state.pinnedRecipes)
	if err != nil || view == nil || view.Run == nil {
		return
	}
	if !containsReadyWorkMarker(view.Run.Objective) {
		return
	}
	item, ok := view.FirstObjectiveReadyItem()
	if !ok {
		return
	}
	raw, err := json.Marshal(LaneSpawnParams{
		ItemID: item.ID,
		Brief:  cmp.Or(strings.TrimSpace(item.Description), strings.TrimSpace(item.Title), item.ID),
		Reason: "proactive staffing: nothing needed a decision on idle tick, started next ready piece from objective",
	})
	if err != nil {
		return
	}
	_, _ = d.handleRawToolCall(ctx, runID, state, ToolLaneSpawn, raw)
}

func isIdleTick(events []store.Event) bool {
	if len(events) == 0 {
		return false
	}
	for i := range events {
		if events[i].Kind != store.EventTimer || strings.TrimSpace(events[i].LaneID) != "" {
			return false
		}
	}
	return true
}

func ownerAskedToStart(events []store.Event) bool {
	asked := false
	for i := range events {
		if events[i].Kind != store.EventOwnerMessage {
			continue
		}
		text := strings.TrimSpace(events[i].Summary + " " + string(events[i].Payload))
		if isHoldDirectiveText(text) {
			asked = false
			continue
		}
		if ownerTextAskedToStart(text) {
			asked = true
		}
	}
	return asked
}

var (
	dispatcherStartIntentWords = []string{
		"start",
		"spawn",
		"launch",
		"proceed",
		"resume",
		"unblock",
		"unpause",
	}
	dispatcherStartIntentPhrases = []string{
		"go ahead",
		"clear the hold",
		"clear both hold",
		"clear hold",
		"lift the hold",
		"lift hold",
	}
	dispatcherStartNegationWords = []string{
		"not",
		"no",
		"never",
		"dont",
		"cannot",
		"cant",
		"wont",
		"shouldnt",
		"hold",
		"pause",
		"stop",
		"wait",
		"delay",
		"defer",
		"block",
		"avoid",
		"without",
		"before",
		"until",
		"unless",
	}
	dispatcherHoldLiftWords = []string{
		"clear",
		"cleared",
		"clearing",
		"lift",
		"lifted",
		"lifting",
		"remove",
		"removed",
		"removing",
		"revoke",
		"revoked",
		"revoking",
		"cancel",
		"canceled",
		"cancelled",
		"drop",
		"dropped",
		"undo",
		"unpause",
		"unpaused",
		"resume",
		"resumed",
		"unblock",
		"unblocked",
		"release",
		"released",
	}
	dispatcherExplicitHoldPhrases = []string{
		"do not start",
		"don't start",
		"dont start",
		"do not spawn",
		"don't spawn",
		"dont spawn",
		"do not launch",
		"don't launch",
		"dont launch",
		"no new lanes",
		"start no new lanes",
		"stop spawning",
		"pause spawning",
		"no additional lanes",
		"no more lanes",
		"hold new lanes",
		"hold off",
		"hold on starting",
		"hold starting",
		"block new lanes",
		"pause new lanes",
		"no new work",
		"block all new work",
		"standing hold",
		"standing owner hold",
	}
)

func splitIntentClauses(s string) []string {
	r := strings.NewReplacer(
		"\n", "|",
		".", "|",
		";", "|",
		"!", "|",
		",", "|",
		":", "|",
		" — ", "|",
		" – ", "|",
		" - ", "|",
		" and ", "|",
		" then ", "|",
		" but ", "|",
	)
	return strings.Split(r.Replace(s), "|")
}

func splitHoldClauses(s string) []string {
	r := strings.NewReplacer(
		"\n", "|",
		".", "|",
		";", "|",
		"!", "|",
		",", "|",
		":", "|",
		" — ", "|",
		" – ", "|",
		" - ", "|",
		" and ", "|",
		" then ", "|",
		" but ", "|",
		" until ", "|",
		" unless ", "|",
	)
	return strings.Split(r.Replace(s), "|")
}

func normalizeContractionWords(s string) string {
	return strings.NewReplacer(
		"don't", "dont",
		"can't", "cant",
		"won't", "wont",
		"shouldn't", "shouldnt",
		"didn't", "didnt",
		"isn't", "isnt",
		"wasn't", "wasnt",
	).Replace(s)
}

func hasStartNegationInPrefix(prefix string) bool {
	norm := normalizeContractionWords(prefix)
	for _, neg := range dispatcherStartNegationWords {
		if dispatcherContainsWholeWord(norm, neg) {
			return true
		}
	}
	return false
}

// OwnerTextAskedToStart reports whether an owner message asks to start, spawn,
// or lift a hold on work.
func OwnerTextAskedToStart(text string) bool {
	return ownerTextAskedToStart(text)
}

func ownerTextAskedToStart(text string) bool {
	unquoted := strings.ToLower(stripDispatcherQuotedSpans(text))
	for _, sentence := range strings.FieldsFunc(unquoted, func(r rune) bool {
		return r == '.' || r == ';' || r == '\n' || r == '!'
	}) {
		sentence = strings.TrimSpace(sentence)
		if sentence == "" || strings.HasSuffix(sentence, "?") || strings.Contains(sentence, "?") {
			continue
		}
		for _, clause := range splitIntentClauses(sentence) {
			clause = strings.TrimSpace(clause)
			if clause == "" {
				continue
			}
			for _, phrase := range dispatcherStartIntentPhrases {
				if idx := strings.Index(clause, phrase); idx >= 0 {
					if !hasStartNegationInPrefix(clause[:idx]) {
						return true
					}
				}
			}
			for _, kw := range dispatcherStartIntentWords {
				for start := 0; start < len(clause); {
					idx := strings.Index(clause[start:], kw)
					if idx < 0 {
						break
					}
					pos := start + idx
					end := pos + len(kw)
					leftOK := pos == 0 || !isDispatcherWordChar(clause[pos-1])
					rightOK := end == len(clause) || !isDispatcherWordChar(clause[end])
					if leftOK && rightOK && !hasStartNegationInPrefix(clause[:pos]) {
						return true
					}
					start = pos + 1
				}
			}
		}
	}
	return false
}

// IsHoldDirectiveText reports whether text states an active hold or pause on
// starting new lanes.
func IsHoldDirectiveText(text string) bool {
	return isHoldDirectiveText(text)
}

func isHoldDirectiveText(text string) bool {
	unquoted := strings.ToLower(stripDispatcherQuotedSpans(text))
	for _, sentence := range strings.FieldsFunc(unquoted, func(r rune) bool {
		return r == '.' || r == ';' || r == '\n' || r == '!'
	}) {
		sentence = strings.TrimSpace(sentence)
		if sentence == "" || strings.Contains(sentence, "?") {
			continue
		}
		for _, clause := range splitHoldClauses(sentence) {
			clause = strings.TrimSpace(clause)
			if clause == "" {
				continue
			}
			if strings.Contains(clause, "keep the hold") ||
				strings.Contains(clause, "keep hold") ||
				strings.Contains(clause, "hold on the run") ||
				strings.Contains(clause, "hold on run") ||
				dispatcherContainsWholeWord(clause, "lease") {
				continue
			}
			lifting := false
			for _, lift := range dispatcherHoldLiftWords {
				if dispatcherContainsWholeWord(clause, lift) {
					lifting = true
					break
				}
			}
			if lifting {
				continue
			}
			for _, phrase := range dispatcherExplicitHoldPhrases {
				if idx := strings.Index(clause, phrase); idx >= 0 {
					prefix := normalizeContractionWords(clause[:idx])
					if !dispatcherContainsWholeWord(prefix, "not") &&
						!dispatcherContainsWholeWord(prefix, "never") &&
						!dispatcherContainsWholeWord(prefix, "didnt") {
						return true
					}
				}
			}
			norm := normalizeContractionWords(clause)
			if (dispatcherContainsWholeWord(norm, "hold") || dispatcherContainsWholeWord(norm, "pause")) &&
				!dispatcherContainsWholeWord(norm, "no") &&
				!dispatcherContainsWholeWord(norm, "not") &&
				!dispatcherContainsWholeWord(norm, "never") &&
				!dispatcherContainsWholeWord(norm, "without") {
				return true
			}
		}
	}
	return false
}

var (
	dispatcherOwnerNoNewWorkPhrases = []string{
		"no new lanes",
		"start no new lanes",
		"do not start new lanes",
		"don't start new lanes",
		"do not spawn",
		"don't spawn",
		"stop spawning",
		"pause spawning",
		"no additional lanes",
		"no more lanes",
		"hold new lanes",
		"hold off on new lanes",
		"block new lanes",
		"pause new lanes",
		"no new work",
		"block all new work",
	}
	dispatcherNoNewWorkRulePhrases = append(slices.Clone(dispatcherOwnerNoNewWorkPhrases),
		"standing owner hold",
		"standing hold",
	)
	dispatcherNegatedNoNewWorkPrefixes = []string{
		"not ",
		"no ",
		"never ",
		"did not ",
		"didn't ",
		"do not want",
		"don't want",
		"without ",
		"remove ",
		"forget ",
		"undo ",
		"drop ",
		"revoke ",
		"clear ",
		"isn't ",
		"is not ",
		"wasn't ",
		"was not ",
	}
)

func stripDispatcherQuotedSpans(s string) string {
	var b strings.Builder
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '"' {
			quote = c
			b.WriteByte(' ')
			continue
		}
		if c == '\'' {
			prevAlpha := i > 0 && ((s[i-1] >= 'a' && s[i-1] <= 'z') || (s[i-1] >= 'A' && s[i-1] <= 'Z'))
			nextAlpha := i+1 < len(s) && ((s[i+1] >= 'a' && s[i+1] <= 'z') || (s[i+1] >= 'A' && s[i+1] <= 'Z'))
			if !prevAlpha || !nextAlpha {
				quote = c
				b.WriteByte(' ')
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

func dispatcherOwnerPlainlyStatedNoNewWork(quote string) bool {
	unquoted := strings.ToLower(stripDispatcherQuotedSpans(quote))
	for _, p := range dispatcherOwnerNoNewWorkPhrases {
		idx := strings.Index(unquoted, p)
		for idx >= 0 {
			prefix := unquoted[:idx]
			if sep := strings.LastIndexAny(prefix, ".;\n!?"); sep >= 0 {
				prefix = prefix[sep+1:]
			}
			prefixTrim := strings.TrimSpace(prefix)
			negated := false
			for _, neg := range dispatcherNegatedNoNewWorkPrefixes {
				if strings.Contains(prefix, neg) || strings.HasSuffix(prefixTrim, strings.TrimSpace(neg)) {
					negated = true
					break
				}
			}
			if !negated {
				return true
			}
			rest := unquoted[idx+len(p):]
			next := strings.Index(rest, p)
			if next < 0 {
				break
			}
			idx += len(p) + next
		}
	}
	return false
}

func isDispatcherWordChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func dispatcherContainsWholeWord(s, word string) bool {
	for start := 0; start < len(s); {
		idx := strings.Index(s[start:], word)
		if idx < 0 {
			return false
		}
		pos := start + idx
		end := pos + len(word)
		leftOK := pos == 0 || !isDispatcherWordChar(s[pos-1])
		rightOK := end == len(s) || !isDispatcherWordChar(s[end])
		if leftOK && rightOK {
			return true
		}
		start = pos + 1
	}
	return false
}

func (d *Dispatcher) validateDispatcherNoteFact(ctx context.Context, runID string, batchEvents []store.Event, fact string) error {
	factLower := strings.ToLower(strings.TrimSpace(fact))
	claimsNoNewWork := false
	for _, p := range dispatcherNoNewWorkRulePhrases {
		if strings.Contains(factLower, p) {
			claimsNoNewWork = true
			break
		}
	}
	noteMentionsHold := claimsNoNewWork || (dispatcherContainsWholeWord(factLower, "hold") && !strings.Contains(factLower, "lease"))
	if !noteMentionsHold {
		return nil
	}
	var ownerSources strings.Builder
	for i := range batchEvents {
		ev := &batchEvents[i]
		if ev.Kind == store.EventOwnerMessage &&
			!strings.EqualFold(ev.Source, string(store.DirectiveSourceConcierge)) &&
			!strings.EqualFold(ev.Source, string(store.DirectiveSourcePlanner)) &&
			!strings.EqualFold(ev.Source, string(store.DirectiveSourceDispatcher)) {
			ownerSources.WriteString(ev.Summary + "\n" + string(ev.Payload) + "\n")
		}
	}
	if d.store != nil {
		if dirs, err := d.store.Directives().List(ctx, store.DirectiveFilter{RunID: runID, ActiveOnly: true}); err == nil {
			for i := range dirs {
				dir := &dirs[i]
				if dir.Source == store.DirectiveSourceOwner {
					ownerSources.WriteString(dir.Text + "\n")
				}
			}
		}
	}
	quoteText := ownerSources.String()
	quoteLower := strings.ToLower(quoteText)
	quoteMentionsLeaseHold := (strings.Contains(quoteLower, "keep the hold") ||
		strings.Contains(quoteLower, "keep hold") ||
		strings.Contains(quoteLower, "hold on the run") ||
		strings.Contains(quoteLower, "hold on run"))
	ownerPlainlyStated := dispatcherOwnerPlainlyStatedNoNewWork(quoteText)
	if claimsNoNewWork && !ownerPlainlyStated {
		return &invariantViolationError{msg: "directive.note [run]: cannot block new lanes or work unless the owner stated it plainly (in an adopted run, 'hold' means orca's lease on the run)"}
	}
	if quoteMentionsLeaseHold && noteMentionsHold && !ownerPlainlyStated {
		return &invariantViolationError{msg: "directive.note [run]: 'keep the hold' on an adopted run means keep orca's lease on the run, not a standing rule blocking work"}
	}
	return nil
}
func (d *Dispatcher) applyPlannerRequest(ctx context.Context, runID string, p PlannerRequestParams) (string, error) {
	reqID := d.nextID("planreq")

	text := strings.TrimSpace(p.Reason)
	if p.Urgent {
		text = "[urgent] " + text
	}
	dir := store.Directive{
		ID:     reqID,
		RunID:  runID,
		Kind:   store.DirectiveInstruction,
		Source: store.DirectiveSourceDispatcher,
		Ref:    plannerRefPrefix + cmp.Or(strings.TrimSpace(p.PlanRef), reqID),
		Text:   text,
		Status: store.DirectiveActive,
	}
	if err := d.store.Directives().Add(ctx, dir); err != nil {
		return "", err
	}
	if d.cfg.OnPlannerRequest != nil {
		if err := d.cfg.OnPlannerRequest(ctx, runID, p.Reason, p.PlanRef); err != nil {
			_ = d.store.Directives().UpdateStatus(context.WithoutCancel(ctx), runID, dir.ID, store.DirectiveRevoked)
			return "", err
		}
	}
	return fmt.Sprintf("requested planner turn (%s)", reqID), nil
}

func (d *Dispatcher) applyReadLaneReport(ctx context.Context, runID string, view *StateView, p ReadLaneReportParams) (string, error) {
	ln, ok := view.Lane(p.LaneID)
	if !ok {
		return "", fmt.Errorf("%w: lane %q", store.ErrNotFound, p.LaneID)
	}
	termID := cmp.Or(strings.TrimSpace(p.TerminalID), ln.TerminalID)
	maxBytes := d.effectiveReadLimit(p.MaxBytes)

	if drv := d.laneDriver(); drv != nil && termID != "" {
		if rep, err := drv.ReadReport(ctx, runID, ln.ID, termID); err == nil && rep != nil {
			out := formatPhaseReport(*rep)
			return truncateText(out, maxBytes, nil), nil
		}
	}

	evs, err := d.store.Events().List(ctx, store.EventFilter{
		RunID:  runID,
		LaneID: ln.ID,
		Kinds:  []store.EventKind{store.EventLaneReport},
	})
	if err != nil {
		return "", err
	}
	if len(evs) == 0 {
		return fmt.Sprintf("lane %s: no phase report found", ln.ID), nil
	}
	latest := evs[len(evs)-1]
	out := fmt.Sprintf("lane=%s event=%s seq=%d summary=%s payload=%s",
		ln.ID, latest.ID, latest.Seq, latest.Summary, string(latest.Payload))
	return truncateText(out, maxBytes, nil), nil
}

func (d *Dispatcher) applyReadLane(ctx context.Context, runID string, view *StateView, p ReadLaneParams) (string, error) {
	ln, ok := view.Lane(p.LaneID)
	if !ok {
		return "", fmt.Errorf("%w: lane %q", store.ErrNotFound, p.LaneID)
	}
	maxBytes := d.effectiveReadLimit(p.MaxBytes)
	if d.cfg.LaneInspector != nil {
		out, err := d.cfg.LaneInspector(ctx, runID, ln.ID, p)
		if err != nil {
			return "", err
		}
		return truncateText(out, maxBytes, nil), nil
	}

	invCfg := d.laneInvestigateConfig(view, ln)
	if !invCfg.HasRoots() {
		return d.applyReadLaneReport(ctx, runID, view, ReadLaneReportParams{
			LaneID:   p.LaneID,
			MaxBytes: p.MaxBytes,
		})
	}

	prog := strings.TrimSpace(p.Program)
	var cmdArgs []string
	if prog != "" {
		if len(p.Args) > 0 {
			cmdArgs = slices.Clone(p.Args)
		} else if strings.ContainsAny(prog, " \t") {
			fields, err := splitCommandFields(prog)
			if err != nil {
				return "", fmt.Errorf("read.lane: %w", err)
			}
			if len(fields) > 0 {
				prog = fields[0]
				cmdArgs = fields[1:]
			}
		}
		if len(cmdArgs) == 0 && strings.TrimSpace(p.Path) != "" {
			cmdArgs = []string{strings.TrimSpace(p.Path)}
		}
	} else if cmdText := strings.TrimSpace(p.Command); cmdText != "" {
		fields, err := splitCommandFields(cmdText)
		if err != nil {
			return "", fmt.Errorf("read.lane: %w", err)
		}
		if len(fields) == 0 {
			return "", errors.New("read.lane: command is empty")
		}
		prog = fields[0]
		cmdArgs = fields[1:]
	}
	if prog != "" {
		execCfg := invCfg
		if prog == "git" {
			execCfg = d.laneGitInvestigateConfig(invCfg, view)
		}
		out, err := model.ExecuteReadCmd(ctx, execCfg, model.ReadCmdParams{
			Program: prog,
			Args:    cmdArgs,
		})
		if err != nil {
			return "", err
		}
		return truncateText(out, maxBytes, nil), nil
	}

	targetPath := cmp.Or(strings.TrimSpace(p.Path), ".")
	if explicitRef := strings.TrimSpace(p.Ref); explicitRef != "" {
		if strings.HasPrefix(explicitRef, "-") {
			return "", fmt.Errorf("read.lane: invalid git ref %q", explicitRef)
		}
		gitOut, gitErr := readLaneGitRef(ctx, d.laneGitInvestigateConfig(invCfg, view), explicitRef, targetPath)
		if gitErr != nil {
			return "", gitErr
		}
		return truncateText(gitOut, maxBytes, nil), nil
	}

	out, err := model.ExecuteReadFile(invCfg, model.ReadFileParams{
		Path:     targetPath,
		MaxBytes: maxBytes,
	})
	if err != nil {
		ref := cmp.Or(strings.TrimSpace(ln.PRHead), strings.TrimSpace(ln.Branch))
		if ref != "" && !strings.HasPrefix(ref, "-") && invCfg.HasRoots() {
			if gitOut, gitErr := readLaneGitRef(ctx, d.laneGitInvestigateConfig(invCfg, view), ref, targetPath); gitErr == nil {
				return truncateText(gitOut, maxBytes, nil), nil
			}
		}
		return "", err
	}
	return truncateText(out, maxBytes, nil), nil
}

func readLaneGitRef(ctx context.Context, invCfg model.InvestigateConfig, ref, targetPath string) (string, error) {
	cleanPath := strings.TrimPrefix(targetPath, "./")
	var gitArgs []string
	if cleanPath == "." || cleanPath == "" || strings.HasSuffix(cleanPath, "/") {
		gitArgs = []string{"ls-tree", ref}
		if cleanPath != "." && cleanPath != "" {
			gitArgs = append(gitArgs, cleanPath)
		}
	} else {
		gitArgs = []string{"show", fmt.Sprintf("%s:%s", ref, cleanPath)}
	}
	return model.ExecuteReadCmd(ctx, invCfg, model.ReadCmdParams{
		Program: "git",
		Args:    gitArgs,
	})
}

func splitCommandFields(s string) ([]string, error) {
	var fields []string
	var cur strings.Builder
	inToken := false
	inSingle := false
	inDouble := false
	escaped := false

	for i := 0; i < len(s); i++ {
		ch := s[i]
		if escaped {
			cur.WriteByte(ch)
			inToken = true
			escaped = false
			continue
		}
		if ch == '\\' && !inSingle {
			escaped = true
			inToken = true
			continue
		}
		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			inToken = true
			continue
		}
		if ch == '"' && !inSingle {
			inDouble = !inDouble
			inToken = true
			continue
		}
		if !inSingle && !inDouble && (ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r') {
			if inToken {
				fields = append(fields, cur.String())
				cur.Reset()
				inToken = false
			}
			continue
		}
		cur.WriteByte(ch)
		inToken = true
	}
	if escaped || inSingle || inDouble {
		return nil, errors.New("unterminated quote or escape in command")
	}
	if inToken {
		fields = append(fields, cur.String())
	}
	return fields, nil
}

func (d *Dispatcher) laneInvestigateConfig(view *StateView, ln lane.Lane) model.InvestigateConfig {
	var roots []string
	addRoot := func(dir string) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			return
		}
		roots = append(roots, dir)
	}
	addRoot(ln.Worktree)
	if runDir := strings.TrimSpace(d.cfg.Investigate.RunDir); runDir != "" {
		addRoot(filepath.Join(runDir, "lanes", ln.ID))
		addRoot(filepath.Join(runDir, ln.ID))
		addRoot(runDir)
	}
	addRoot(d.cfg.Investigate.RepoDir)
	if view != nil && view.Run != nil {
		if repo := strings.TrimSpace(view.Run.Repository); repo != "" {
			if info, err := os.Stat(repo); err == nil && info.IsDir() {
				addRoot(repo)
			}
		}
	}
	for _, extra := range d.cfg.Investigate.ExtraRoots {
		addRoot(extra)
	}

	var primaryRunDir string
	for _, r := range roots {
		if info, err := os.Stat(r); err == nil && info.IsDir() {
			primaryRunDir = r
			break
		}
	}
	if primaryRunDir == "" && len(roots) > 0 {
		primaryRunDir = roots[0]
	}
	return model.InvestigateConfig{
		CommandRunner: d.cfg.Investigate.CommandRunner,
		RunDir:        primaryRunDir,
		RepoDir:       primaryRunDir,
		ExtraRoots:    roots,
	}
}

func (d *Dispatcher) laneGitInvestigateConfig(invCfg model.InvestigateConfig, view *StateView) model.InvestigateConfig {
	if model.ResolveGitTopLevel(invCfg.RepoDir) != "" {
		return invCfg
	}
	var candidates []string
	candidates = append(candidates, d.cfg.Investigate.RepoDir)
	if view != nil && view.Run != nil {
		candidates = append(candidates, view.Run.Repository)
	}
	candidates = append(candidates, invCfg.ExtraRoots...)
	for _, cand := range candidates {
		cand = strings.TrimSpace(cand)
		if cand == "" {
			continue
		}
		if model.ResolveGitTopLevel(cand) != "" {
			invCfg.RepoDir = cand
			return invCfg
		}
	}
	if repo := strings.TrimSpace(d.cfg.Investigate.RepoDir); repo != "" {
		invCfg.RepoDir = repo
	}
	return invCfg
}

func (d *Dispatcher) applyReadPR(_ context.Context, _ string, view *StateView, p ReadPRParams) (string, error) {
	ln, ok := view.Lane(p.LaneID)
	if !ok {
		return "", fmt.Errorf("%w: lane %q", store.ErrNotFound, p.LaneID)
	}
	maxBytes := d.effectiveReadLimit(p.MaxBytes)
	out := fmt.Sprintf("lane=%s status=%s phase=%s round=%d pr=#%d branch=%s base=%s head=%s approval=%s approval_stale=%t merge_sha=%s checks=%s",
		ln.ID,
		ln.Status,
		cmp.Or(ln.Phase, "-"),
		ln.Round,
		ln.PR,
		cmp.Or(ln.Branch, "-"),
		cmp.Or(ln.BaseBranch, "-"),
		cmp.Or(ln.PRHead, "-"),
		cmp.Or(ln.ApprovalSHA, "-"),
		ln.ApprovalStale(),
		cmp.Or(ln.MergeSHA, "-"),
		cmp.Or(ln.Checks, "-"),
	)
	return truncateText(out, maxBytes, nil), nil
}

func (d *Dispatcher) applyReadCI(ctx context.Context, runID string, view *StateView, p ReadCIParams) (string, error) {
	ln, ok := view.Lane(p.LaneID)
	if !ok {
		return "", fmt.Errorf("%w: lane %q", store.ErrNotFound, p.LaneID)
	}
	maxBytes := d.effectiveReadLimit(p.MaxBytes)

	evs, err := d.store.Events().List(ctx, store.EventFilter{
		RunID:  runID,
		LaneID: ln.ID,
		Kinds:  []store.EventKind{store.EventCI},
	})
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "lane=%s pr=#%d head=%s checks=%s ci_events=%d",
		ln.ID, ln.PR, cmp.Or(ln.PRHead, "-"), cmp.Or(ln.Checks, "-"), len(evs))
	for i := max(len(evs)-3, 0); i < len(evs); i++ {
		ev := evs[i]
		fmt.Fprintf(&b, "\n- [%s seq=%d] %s %s", ev.ID, ev.Seq, ev.Summary, string(ev.Payload))
	}
	return truncateText(b.String(), maxBytes, nil), nil
}

func (d *Dispatcher) effectiveReadLimit(requested int) int {
	limit := d.cfg.MaxReadBytes
	if limit <= 0 {
		limit = DefaultMaxReadBytes
	}
	if requested > 0 && requested < limit {
		return requested
	}
	return limit
}

func formatPhaseReport(rep lane.PhaseReport) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("Outcome: %s", rep.Outcome))
	if rep.Phase != "" {
		parts = append(parts, fmt.Sprintf("Phase: %s (round %d)", rep.Phase, max(rep.Round, 1)))
	}
	if rep.HeadSHA != "" {
		parts = append(parts, fmt.Sprintf("HEAD: %s", rep.HeadSHA))
	}
	if rep.PR > 0 {
		parts = append(parts, fmt.Sprintf("PR: #%d", rep.PR))
	}
	if rep.Reason != "" {
		parts = append(parts, fmt.Sprintf("Reason: %s", rep.Reason))
	}
	if rep.Breaks != "" {
		parts = append(parts, fmt.Sprintf("Breaks: %s", rep.Breaks))
	}
	if rep.For != "" {
		parts = append(parts, fmt.Sprintf("For: %s", rep.For))
	}
	if rep.Ask != "" {
		parts = append(parts, fmt.Sprintf("Ask: %s", rep.Ask))
	}
	if rep.RiskiestStep != "" {
		parts = append(parts, fmt.Sprintf("Riskiest step: %s", rep.RiskiestStep))
	}
	if rep.Observed != "" {
		parts = append(parts, fmt.Sprintf("Observed: %s", rep.Observed))
	}
	if rep.Summary != "" {
		parts = append(parts, rep.Summary)
	} else if rep.Raw != "" {
		parts = append(parts, rep.Raw)
	}
	return strings.Join(parts, "\n")
}

// CompileRecipeOverride applies a deterministic natural-language override to
// base, producing a per-lane Recipe.
func CompileRecipeOverride(base recipe.Recipe, override string) recipe.Recipe {
	if len(base.Phases) == 0 {
		base = recipe.Default()
	}
	out := base.Clone()
	out.BaseRecipe = base.Name
	out.BaseVersion = base.Version
	trimmed := strings.TrimSpace(override)
	if trimmed == "" {
		return out
	}
	out.Version = 0
	if out.Source != "" {
		out.Source = out.Source + " | Override: " + trimmed
	} else {
		out.Source = trimmed
	}

	lower := strings.ToLower(trimmed)

	if containsOverridePhrase(lower, "no clean") || containsOverridePhrase(lower, "skip clean") || containsOverridePhrase(lower, "without clean") {
		out = removePhaseFromRecipe(out, recipe.PhaseClean)
	}

	if containsOverridePhrase(lower, "no code review") || containsOverridePhrase(lower, "no review") ||
		containsOverridePhrase(lower, "skip review") || containsOverridePhrase(lower, "without review") {
		out = removePhaseFromRecipe(out, recipe.PhaseReview)
		out.MergePolicy.RequireReviewPass = false
		for i := range out.Phases {
			out.Phases[i].Gates = slices.DeleteFunc(out.Phases[i].Gates, func(g recipe.Gate) bool {
				return g.Name == "review_approved" || g.Name == "review_head_pinned"
			})
		}
	}

	switch {
	case containsOverridePhrase(lower, "merge themselves") || containsOverridePhrase(lower, "self merge") ||
		containsOverridePhrase(lower, "self-merge") || containsOverridePhrase(lower, "merge_mode=self"):
		out.MergePolicy.Mode = recipe.MergeModeSelf
	case containsOverridePhrase(lower, "manual merge") || containsOverridePhrase(lower, "merge_mode=manual"):
		out.MergePolicy.Mode = recipe.MergeModeManual
	case containsOverridePhrase(lower, "analysis only") || containsOverridePhrase(lower, "no merge") ||
		containsOverridePhrase(lower, "skip merge") || containsOverridePhrase(lower, "without merge") ||
		containsOverridePhrase(lower, "merge_mode=none") || store.IsNoMergeText(lower):
		out = recipe.WithoutIntegrate(out)
	}

	return out
}

func containsOverridePhrase(s, phrase string) bool {
	offset := 0
	for offset <= len(s)-len(phrase) {
		idx := strings.Index(s[offset:], phrase)
		if idx < 0 {
			return false
		}
		start := offset + idx
		end := start + len(phrase)
		leftOK := start == 0 || !isOverrideWordChar(s[start-1])
		if leftOK && isOverrideClauseEnd(s[end:]) {
			return true
		}
		offset = start + 1
	}
	return false
}

func isOverrideWordChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_' || b == '-'
}

func isOverrideClauseEnd(rest string) bool {
	if rest == "" {
		return true
	}
	if isOverrideWordChar(rest[0]) {
		return false
	}
	trimmed := strings.TrimLeft(rest, " \t")
	for _, suffix := range []string{"phase", "step"} {
		if strings.HasPrefix(trimmed, suffix) {
			after := trimmed[len(suffix):]
			if after == "" || !isOverrideWordChar(after[0]) {
				trimmed = strings.TrimLeft(after, " \t")
				break
			}
		}
	}
	if trimmed == "" {
		return true
	}
	switch trimmed[0] {
	case ',', ';', '.', '|', '\n', '\r', ')', ':':
		return true
	default:
		return false
	}
}

func removePhaseFromRecipe(r recipe.Recipe, phaseToRemove string) recipe.Recipe {
	if len(r.Phases) <= 1 {
		return r
	}
	var nextPhase string
	for _, tr := range r.Transitions {
		if tr.From == phaseToRemove && tr.On.Verdict == recipe.OutcomeDone {
			nextPhase = tr.To
			break
		}
	}
	if nextPhase == "" {
		nextPhase = recipe.TargetComplete
	}

	r.Phases = slices.DeleteFunc(r.Phases, func(p recipe.Phase) bool {
		return p.Name == phaseToRemove
	})

	var updatedTransitions []recipe.Transition
	for _, tr := range r.Transitions {
		if tr.From == phaseToRemove {
			continue
		}
		if tr.To == phaseToRemove {
			tr.To = nextPhase
		}
		updatedTransitions = append(updatedTransitions, tr)
	}
	r.Transitions = updatedTransitions
	return r
}

//nolint:govet // logical field grouping
type batchLaneMail struct {
	event store.Event
	msg   mailbox.Message
}

func extractBatchLaneMails(events []store.Event) []batchLaneMail {
	return extractBatchLaneMailsFiltered(events, false)
}

func extractBatchLaneMailsIncludeClosed(events []store.Event) []batchLaneMail {
	return extractBatchLaneMailsFiltered(events, true)
}

func extractBatchLaneMailsFiltered(events []store.Event, includeClosed bool) []batchLaneMail {
	var out []batchLaneMail
	for i := range events {
		ev := events[i]
		if ev.Kind != store.EventMail || len(ev.Payload) == 0 {
			continue
		}
		var msg mailbox.Message
		if err := json.Unmarshal(ev.Payload, &msg); err != nil {
			continue
		}
		msg.Normalize()
		if msg.LaneID == "" && ev.LaneID != "" {
			msg.LaneID = ev.LaneID
		}
		if msg.From == "owner" || msg.LaneID == "" {
			continue
		}
		if !includeClosed && msg.IsClosedOrAnswered() {
			continue
		}
		out = append(out, batchLaneMail{event: ev, msg: msg})
	}
	return out
}

func matchLaneMailSlice(
	mails []batchLaneMail,
	messageID, laneID string,
	requireTarget bool,
	pred func(batchLaneMail) bool,
) (batchLaneMail, bool, error) {
	messageID = strings.TrimSpace(messageID)
	laneID = strings.TrimSpace(laneID)
	if messageID != "" {
		for i := range mails {
			bm := &mails[i]
			if bm.msg.ID == messageID || bm.event.ID == messageID {
				if pred != nil && !pred(*bm) {
					return batchLaneMail{}, false, fmt.Errorf("%w: mail %q does not match expected mail kind", store.ErrInvalidArgument, messageID)
				}
				if laneID != "" && bm.msg.LaneID != "" && bm.msg.LaneID != laneID {
					return batchLaneMail{}, false, fmt.Errorf("%w: mail %q belongs to lane %q, not %q", store.ErrInvalidArgument, messageID, bm.msg.LaneID, laneID)
				}
				return *bm, true, nil
			}
		}
		return batchLaneMail{}, false, nil
	}
	if !requireTarget && laneID == "" {
		return batchLaneMail{}, false, nil
	}
	var candidates []batchLaneMail
	for i := range mails {
		if pred == nil || pred(mails[i]) {
			candidates = append(candidates, mails[i])
		}
	}
	if laneID != "" {
		var matched batchLaneMail
		count := 0
		for i := range candidates {
			bm := &candidates[i]
			if bm.msg.LaneID == laneID {
				matched = *bm
				count++
			}
		}
		if count == 1 {
			return matched, true, nil
		}
		if count > 1 && requireTarget {
			return batchLaneMail{}, false, fmt.Errorf("%w: multiple open lane mails for lane %q in batch; specify message_id", store.ErrInvalidArgument, laneID)
		}
		return batchLaneMail{}, false, nil
	}
	if len(candidates) == 1 {
		return candidates[0], true, nil
	}
	if len(candidates) > 1 && requireTarget {
		return batchLaneMail{}, false, fmt.Errorf("%w: multiple open lane mails in batch; specify message_id or lane_id", store.ErrInvalidArgument)
	}
	return batchLaneMail{}, false, nil
}

func (d *Dispatcher) resolveLaneMailTarget(
	ctx context.Context,
	runID string,
	batchEvents []store.Event,
	messageID, laneID string,
	requireTarget bool,
	pred func(batchLaneMail) bool,
) (batchLaneMail, bool, error) {
	messageID = strings.TrimSpace(messageID)
	laneID = strings.TrimSpace(laneID)
	bm, ok, err := matchLaneMailSlice(extractBatchLaneMails(batchEvents), messageID, laneID, requireTarget, pred)
	if err != nil {
		return bm, false, err
	}
	if ok {
		if mailbox.IsAnswered(ctx, d.store, runID, bm.msg.ID, bm.event.ID) {
			return batchLaneMail{}, false, fmt.Errorf("%w: lane mail %q is already answered", store.ErrInvalidArgument, cmp.Or(messageID, bm.msg.ID, bm.event.ID))
		}
		return bm, true, nil
	}
	if messageID == "" {
		return batchLaneMail{}, false, nil
	}
	if d.store != nil {
		evs, listErr := d.store.Events().List(ctx, store.EventFilter{
			RunID: runID,
			Kinds: []store.EventKind{store.EventMail},
		})
		if listErr != nil {
			return batchLaneMail{}, false, listErr
		}
		bm, ok, err = matchLaneMailSlice(extractBatchLaneMails(evs), messageID, laneID, requireTarget, pred)
		if err != nil {
			return batchLaneMail{}, false, err
		}
		if ok {
			if mailbox.IsAnswered(ctx, d.store, runID, bm.msg.ID, bm.event.ID) {
				return batchLaneMail{}, false, fmt.Errorf("%w: lane mail %q is already answered", store.ErrInvalidArgument, messageID)
			}
			return bm, true, nil
		}
		for i := range evs {
			var msg mailbox.Message
			if len(evs[i].Payload) > 0 && json.Unmarshal(evs[i].Payload, &msg) == nil {
				msg.Normalize()
				if (msg.ID == messageID || evs[i].ID == messageID) && msg.IsClosedOrAnswered() {
					return batchLaneMail{}, false, fmt.Errorf("%w: lane mail %q is already closed or answered", store.ErrInvalidArgument, messageID)
				}
			}
		}
	}
	if !requireTarget && laneID == "" && isRunLevelBatchEventID(batchEvents, messageID) {
		return batchLaneMail{}, false, nil
	}
	return batchLaneMail{}, false, fmt.Errorf("%w: unknown lane mail message_id %q", store.ErrNotFound, messageID)
}

func isRunLevelBatchEventID(events []store.Event, id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	for i := range events {
		if events[i].LaneID != "" {
			continue
		}
		if events[i].ID == id {
			return true
		}
		var msg mailbox.Message
		if len(events[i].Payload) > 0 && json.Unmarshal(events[i].Payload, &msg) == nil {
			msg.Normalize()
			if msg.LaneID == "" && msg.ID == id {
				return true
			}
		}
	}
	return false
}

func (d *Dispatcher) applyMailAnswer(
	ctx context.Context,
	runID string,
	state *turnExecState,
	view *StateView,
	p MailAnswerParams,
) (string, error) {
	msgID := strings.TrimSpace(p.MessageID)
	laneID := strings.TrimSpace(p.LaneID)
	answer := strings.TrimSpace(p.Answer)

	var batchEvents []store.Event
	if state != nil {
		batchEvents = state.events
	}
	bm, hasMail, err := d.resolveLaneMailTarget(ctx, runID, batchEvents, msgID, laneID, true, nil)
	if err != nil {
		return "", err
	}
	if !hasMail {
		return "", errors.New("mail.answer requires a message_id or matching lane mail in the batch")
	}
	if msgID == "" {
		msgID = bm.msg.ID
	}
	if laneID == "" {
		laneID = bm.msg.LaneID
	}
	delivered, err := d.respondToLaneMail(ctx, runID, bm, answer)
	if err != nil {
		return "", err
	}
	if bm.msg.IsQuestion() && laneID != "" && view != nil {
		if ln, ok := view.Lane(laneID); ok {
			if !delivered {
				if drv := d.laneDriver(); drv != nil && (ln.SessionID != "" || ln.TerminalID != "") {
					if err := drv.Steer(ctx, lane.SteerRequest{
						RunID:      runID,
						LaneID:     ln.ID,
						SessionID:  ln.SessionID,
						TerminalID: ln.TerminalID,
						Message:    answer,
					}); err != nil {
						return "", err
					}
				}
			}
			if ln.Status == lane.StatusBlocked {
				ln.Status = lane.StatusRunning
				ln.LastEventAt = d.clock.Now().UTC()
				if err := d.store.Lanes().Upsert(ctx, ln); err != nil {
					return "", err
				}
			}
		}
	}
	return fmt.Sprintf("answered lane mail %s for lane %s", cmp.Or(msgID, bm.event.ID, "-"), cmp.Or(laneID, "-")), nil
}

const mailAnswerNotePrefix = "mail answer ("

func (d *Dispatcher) respondToLaneMail(ctx context.Context, runID string, bm batchLaneMail, answer string) (bool, error) {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return false, nil
	}
	msg := bm.msg
	msg.RunID = cmp.Or(msg.RunID, runID)
	boxes := d.mailboxes()
	for _, box := range boxes {
		if err := mailbox.Respond(ctx, box, msg, answer); err != nil {
			return false, err
		}
	}
	if err := mailbox.MarkAnswered(ctx, d.store, runID, msg.LaneID, msg.ID, bm.event.ID, answer); err != nil {
		return false, err
	}
	_, _ = d.store.Notes().Append(ctx, store.RunNote{
		RunID:     runID,
		TaskID:    msg.LaneID,
		Text:      fmt.Sprintf("%s%s): %s", mailAnswerNotePrefix, cmp.Or(msg.ID, bm.event.ID), answer),
		CreatedAt: d.clock.Now().UTC(),
	})
	return len(boxes) > 0, nil
}

func (d *Dispatcher) applyMailDismiss(
	ctx context.Context,
	runID string,
	state *turnExecState,
	_ *StateView,
	p MailDismissParams,
) (string, error) {
	msgID := strings.TrimSpace(p.MessageID)
	laneID := strings.TrimSpace(p.LaneID)
	reason := strings.TrimSpace(p.Reason)

	var batchEvents []store.Event
	if state != nil {
		batchEvents = state.events
	}
	bm, hasMail, err := d.resolveLaneMailForDismiss(ctx, runID, batchEvents, msgID, laneID)
	if err != nil {
		return "", err
	}
	if !hasMail {
		return "", errors.New("mail.dismiss requires a message_id or matching lane mail in the batch")
	}
	if msgID == "" {
		msgID = bm.msg.ID
	}
	if laneID == "" {
		laneID = bm.msg.LaneID
	}
	if err := d.dismissBatchQuestion(ctx, runID, bm, reason); err != nil {
		return "", err
	}
	return fmt.Sprintf("dismissed lane mail %s for lane %s (%s)", cmp.Or(msgID, bm.event.ID, "-"), cmp.Or(laneID, "-"), reason), nil
}

func (d *Dispatcher) resolveLaneMailForDismiss(
	ctx context.Context,
	runID string,
	batchEvents []store.Event,
	messageID, laneID string,
) (batchLaneMail, bool, error) {
	messageID = strings.TrimSpace(messageID)
	laneID = strings.TrimSpace(laneID)
	bm, ok, err := matchLaneMailSlice(extractBatchLaneMailsIncludeClosed(batchEvents), messageID, laneID, true, nil)
	if err != nil {
		return bm, false, err
	}
	if ok {
		return bm, true, nil
	}
	if messageID == "" || d.store == nil {
		return batchLaneMail{}, false, nil
	}
	evs, listErr := d.store.Events().List(ctx, store.EventFilter{
		RunID: runID,
		Kinds: []store.EventKind{store.EventMail},
	})
	if listErr != nil {
		return batchLaneMail{}, false, listErr
	}
	bm, ok, err = matchLaneMailSlice(extractBatchLaneMailsIncludeClosed(evs), messageID, laneID, true, nil)
	if err != nil {
		return batchLaneMail{}, false, err
	}
	if ok {
		return bm, true, nil
	}
	return batchLaneMail{}, false, fmt.Errorf("%w: unknown lane mail message_id %q", store.ErrNotFound, messageID)
}

func (d *Dispatcher) dismissBatchQuestion(ctx context.Context, runID string, bm batchLaneMail, reason string) error {
	reason = cmp.Or(strings.TrimSpace(reason), "stale or obsolete lane question")
	msg := bm.msg
	msg.RunID = cmp.Or(msg.RunID, runID)
	for _, box := range d.mailboxes() {
		if err := mailbox.Dismiss(ctx, box, msg, reason); err != nil {
			return err
		}
	}
	if err := mailbox.MarkAnswered(ctx, d.store, runID, msg.LaneID, msg.ID, bm.event.ID, "dismissed: "+reason); err != nil {
		return err
	}
	if d.store != nil {
		if dirs, err := d.store.Directives().List(ctx, store.DirectiveFilter{RunID: runID, ActiveOnly: true}); err == nil {
			for i := range dirs {
				dir := &dirs[i]
				if !strings.HasPrefix(dir.Ref, ownerAskRefPrefix) {
					continue
				}
				var ask controller.OwnerAsk
				if json.Unmarshal([]byte(dir.Text), &ask) != nil {
					continue
				}
				if (msg.ID != "" && (ask.SourceMessageID == msg.ID || ask.ID == msg.ID)) ||
					(bm.event.ID != "" && ask.SourceEventID == bm.event.ID) {
					_ = d.store.Directives().UpdateStatus(ctx, runID, dir.ID, store.DirectiveHandled)
				}
			}
		}
	}
	return nil
}

func (d *Dispatcher) dismissStaleBatchQuestions(
	ctx context.Context,
	runID string,
	events []store.Event,
	state *turnExecState,
	summary string,
) {
	mails := extractBatchLaneMailsIncludeClosed(events)
	if len(mails) == 0 {
		return
	}
	for i := range mails {
		bm := mails[i]
		if !bm.msg.IsQuestion() {
			continue
		}
		if state != nil && slices.ContainsFunc(state.accepted, func(a ProposedAction) bool {
			return a.Tool == ToolMailAnswer || a.Tool == ToolMailDismiss ||
				(a.Tool == ToolOwnerAsk && a.OwnerAsk != nil &&
					(a.OwnerAsk.MessageID == bm.msg.ID || a.OwnerAsk.LaneID == bm.msg.LaneID))
		}) {
			continue
		}
		if !mailbox.IsQuestionObsoleteOrClosed(ctx, d.store, runID, bm.msg.LaneID, bm.msg.ID, bm.event.ID, bm.msg.Body) {
			continue
		}
		reason := cmp.Or(strings.TrimSpace(summary), "stale or obsolete lane question")
		if err := d.dismissBatchQuestion(ctx, runID, bm, reason); err != nil {
			continue
		}
		rawArgs, _ := json.Marshal(MailDismissParams{
			MessageID: bm.msg.ID,
			LaneID:    bm.msg.LaneID,
			Reason:    reason,
		})
		act := ProposedAction{
			Tool:   ToolMailDismiss,
			LaneID: bm.msg.LaneID,
			Reason: reason,
			Args:   rawArgs,
		}
		_ = d.recordDecision(ctx, runID, state, act, true, fmt.Sprintf("dismissed lane mail %s for lane %s (%s)", cmp.Or(bm.msg.ID, bm.event.ID, "-"), cmp.Or(bm.msg.LaneID, "-"), reason), 0)
	}
}
