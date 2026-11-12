package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/orch/clock"
	"github.com/bazelment/yoloswe/orch/model"
	"github.com/bazelment/yoloswe/orch/recipe"
	"github.com/bazelment/yoloswe/orch/store"
)

// Concierge role constants and tool names.
const (
	ActorConcierge          = "concierge"
	ToolConciergeStatus     = "status"
	ToolConciergeLaneShow   = "lane.show"
	ToolConciergeDecisions  = "decisions.since"
	ToolConciergePRCI       = "pr.ci"
	ToolDirectiveSubmit     = "directive.submit"
	DefaultConciergeMaxTurn = 6
)

// ConciergeSystemPrompt is the system prompt for owner chat sessions.
const ConciergeSystemPrompt = `You are the Nebula Swarm Concierge.
Answer the owner's questions accurately and concisely using read-only Store tools (status, lane.show, decisions.since, pr.ci).
Never invent facts not present in the Store. When the owner issues a rule, instruction, recipe edit, or plan import, call directive.submit.`

// ConciergeStatusParams is the input schema for the `status` tool.
type ConciergeStatusParams struct {
	IncludeMetrics bool `json:"include_metrics,omitempty" jsonschema:"description=Include decision, token, cost, and idle metrics"`
}

// ConciergeLaneShowParams is the input schema for the `lane.show` tool.
type ConciergeLaneShowParams struct {
	LaneID string `json:"lane_id" jsonschema:"required,description=Lane ID to inspect"`
}

// ConciergeDecisionsParams is the input schema for the `decisions.since` tool.
type ConciergeDecisionsParams struct {
	Category string `json:"category,omitempty" jsonschema:"description=Optional category filter (auto or judgment)"`
	LaneID   string `json:"lane_id,omitempty" jsonschema:"description=Optional lane ID filter"`
	SinceSeq int64  `json:"since_seq,omitempty" jsonschema:"description=Return decisions with seq > since_seq"`
	Limit    int    `json:"limit,omitempty" jsonschema:"description=Max decisions to return (default 25)"`
}

// ConciergePRCIParams is the input schema for the `pr.ci` tool.
type ConciergePRCIParams struct {
	LaneID string `json:"lane_id" jsonschema:"required,description=Lane ID to inspect PR and CI checks"`
}

// DirectiveSubmitParams is the input schema for the `directive.submit` write tool.
type DirectiveSubmitParams struct {
	Kind   string `json:"kind,omitempty" jsonschema:"enum=standing,enum=instruction,enum=recipe_edit,enum=plan_import,description=Directive kind (standing, instruction, recipe_edit, plan_import)"`
	Text   string `json:"text" jsonschema:"required,description=Directive rule, instruction, recipe delta, or plan summary"`
	Ref    string `json:"ref,omitempty" jsonschema:"description=Optional plan reference or target recipe name"`
	Scope  string `json:"scope,omitempty" jsonschema:"description=Optional lane ID or scope"`
	Expiry string `json:"expiry,omitempty" jsonschema:"description=Optional RFC3339 timestamp or Go duration (e.g. 24h)"`
}

// Concierge hosts a fresh owner chat thread with read-only Store tools and `directive.submit`.
type Concierge struct {
	store     store.Store
	clock     clock.Clock
	client    model.ModelClient
	runID     string
	modelName string
	threadID  string
	history   []model.Message
	seq       int64
	mu        sync.Mutex
}

// NewConcierge creates a new Concierge instance for runID.
func NewConcierge(
	runID string,
	st store.Store,
	clk clock.Clock,
	client model.ModelClient,
	modelName string,
) *Concierge {
	if clk == nil {
		clk = clock.NewRealClock()
	}
	return &Concierge{
		runID:     strings.TrimSpace(runID),
		store:     st,
		clock:     clk,
		client:    client,
		modelName: cmp.Or(strings.TrimSpace(modelName), DefaultConciergeModel),
	}
}

func (c *Concierge) nextID(prefix string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	return fmt.Sprintf("%s-%d", prefix, c.seq)
}

func (c *Concierge) ensureThread(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.threadID != "" {
		id := c.threadID
		c.mu.Unlock()
		return id, nil
	}
	c.seq++
	candidateID := fmt.Sprintf("concierge-%s-%d", c.runID, time.Now().UTC().UnixNano()+c.seq)
	c.mu.Unlock()

	th, err := c.store.Threads().StartThread(ctx, store.Thread{
		ID:     candidateID,
		RunID:  c.runID,
		Role:   store.ThreadRoleConcierge,
		Goal:   "Owner chat for run " + c.runID,
		Status: store.ThreadStatusIdle,
	})
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.threadID = th.ID
	c.mu.Unlock()
	return th.ID, nil
}

// Close archives the Concierge conversation thread.
func (c *Concierge) Close(ctx context.Context) error {
	c.mu.Lock()
	threadID := c.threadID
	c.mu.Unlock()
	if threadID == "" {
		return nil
	}
	return c.store.Threads().UpdateThreadStatus(ctx, threadID, store.ThreadStatusArchived)
}

// Ask executes one conversational turn on the Concierge thread and returns the answer text.
func (c *Concierge) Ask(ctx context.Context, question string) (string, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return "", nil
	}

	threadID, err := c.ensureThread(ctx)
	if err != nil {
		return "", err
	}
	turnID := c.nextID(threadID + "-turn")
	if _, err := c.store.Threads().StartTurn(ctx, store.Turn{
		ID:       turnID,
		ThreadID: threadID,
		RunID:    c.runID,
		Input:    question,
	}); err != nil {
		return "", err
	}

	cleanupCtx := context.WithoutCancel(ctx)
	_, _ = c.store.Threads().AppendItem(cleanupCtx, store.Item{
		ThreadID: threadID,
		TurnID:   turnID,
		Kind:     store.ItemKindUserMessage,
		Content:  question,
	})

	if c.client == nil {
		report, repErr := c.FormatStatusReport(ctx)
		if repErr != nil {
			_ = c.store.Threads().CompleteTurn(cleanupCtx, threadID, turnID, store.TurnStatusFailed, model.Usage{}, repErr.Error())
			return "", repErr
		}
		_, _ = c.store.Threads().AppendItem(cleanupCtx, store.Item{
			ThreadID: threadID,
			TurnID:   turnID,
			Kind:     store.ItemKindAssistantMessage,
			Content:  report,
		})
		_ = c.store.Threads().CompleteTurn(cleanupCtx, threadID, turnID, store.TurnStatusCompleted, model.Usage{}, "")
		return report, nil
	}

	c.mu.Lock()
	msgs := append(slices.Clone(c.history), model.NewUserMessage(question))
	c.mu.Unlock()

	reg := c.buildToolRegistry(threadID, turnID)
	req := model.Request{
		Model:     c.modelName,
		System:    ConciergeSystemPrompt,
		Messages:  msgs,
		Tools:     reg.Definitions(),
		MaxTokens: model.DefaultMaxTokens,
	}

	loopRes, loopErr := model.RunToolLoop(
		ctx,
		c.client,
		req,
		reg,
		model.WithMaxTurns(DefaultConciergeMaxTurn),
		model.WithOnTurn(func(_ int, resp model.Response) {
			if txt := strings.TrimSpace(resp.Text()); txt != "" {
				_, _ = c.store.Threads().AppendItem(cleanupCtx, store.Item{
					ThreadID: threadID,
					TurnID:   turnID,
					Kind:     store.ItemKindAssistantMessage,
					Content:  txt,
				})
			}
		}),
		model.WithOnToolCall(func(_ int, call model.ToolCall, resBlock model.ContentBlock) {
			_, _ = c.store.Threads().AppendItem(cleanupCtx, store.Item{
				ThreadID: threadID,
				TurnID:   turnID,
				Kind:     store.ItemKindToolCall,
				Name:     call.Name,
				CallID:   call.ID,
				Payload:  call.Input,
			})
			_, _ = c.store.Threads().AppendItem(cleanupCtx, store.Item{
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

	var (
		usage  model.Usage
		answer string
	)
	if loopRes != nil {
		usage = loopRes.Usage
		answer = strings.TrimSpace(loopRes.Response.Text())
		c.mu.Lock()
		c.history = slices.Clone(loopRes.Messages)
		c.mu.Unlock()
	}

	if loopErr != nil {
		_ = c.store.Threads().CompleteTurn(cleanupCtx, threadID, turnID, store.TurnStatusFailed, usage, loopErr.Error())
		return answer, loopErr
	}
	if err := c.store.Threads().CompleteTurn(cleanupCtx, threadID, turnID, store.TurnStatusCompleted, usage, ""); err != nil {
		return answer, err
	}
	return answer, nil
}

// RunInteractive runs an interactive terminal chat loop reading questions from in and writing answers to out.
func (c *Concierge) RunInteractive(ctx context.Context, in io.Reader, out io.Writer) error {
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		switch strings.ToLower(line) {
		case "/exit", "/quit", "exit", "quit":
			return nil
		}
		ans, err := c.Ask(ctx, line)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, ans); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (c *Concierge) buildToolRegistry(threadID, turnID string) *model.ToolRegistry {
	reg := model.NewToolRegistry()

	model.AddTool[ConciergeStatusParams](reg, ToolConciergeStatus,
		"Read structured run status, backlog, lanes, active directives, and decision/token/cost metrics",
		func(ctx context.Context, _ ConciergeStatusParams) (string, error) {
			return c.FormatStatusReport(ctx)
		},
	)

	laneShowHandler := func(ctx context.Context, p ConciergeLaneShowParams) (string, error) {
		return c.handleLaneShow(ctx, p.LaneID)
	}
	model.AddTool[ConciergeLaneShowParams](reg, ToolConciergeLaneShow,
		"Show detailed state, rounds, terminals, PR/CI, and recent decisions for a lane",
		laneShowHandler,
	)

	decisionsHandler := func(ctx context.Context, p ConciergeDecisionsParams) (string, error) {
		return c.handleDecisionsSince(ctx, p)
	}
	model.AddTool[ConciergeDecisionsParams](reg, ToolConciergeDecisions,
		"List decisions from the append-only DecisionStore since a sequence number",
		decisionsHandler,
	)

	prCIHandler := func(ctx context.Context, p ConciergePRCIParams) (string, error) {
		return c.handlePRCI(ctx, p.LaneID)
	}
	model.AddTool[ConciergePRCIParams](reg, ToolConciergePRCI,
		"Read PR metadata, review approval status, and CI check status for a lane",
		prCIHandler,
	)

	directiveHandler := func(ctx context.Context, p DirectiveSubmitParams) (string, error) {
		return c.handleDirectiveSubmit(ctx, threadID, turnID, p)
	}
	model.AddTool[DirectiveSubmitParams](reg, ToolDirectiveSubmit,
		"Submit a typed owner directive (standing, instruction, recipe_edit, plan_import) for the Controller/Dispatcher/Planner",
		directiveHandler,
	)

	return reg
}

// FormatStatusReport renders a structured status summary of the run from Store state and DecisionStore metrics.
func (c *Concierge) FormatStatusReport(ctx context.Context) (string, error) {
	run, err := c.store.Runs().Get(ctx, c.runID)
	if err != nil {
		return "", err
	}
	backlog, err := c.store.Backlog().List(ctx, store.BacklogFilter{RunID: c.runID})
	if err != nil {
		return "", err
	}
	lanes, err := c.store.Lanes().List(ctx, store.LaneFilter{RunID: c.runID})
	if err != nil {
		return "", err
	}
	dirs, err := c.store.Directives().List(ctx, store.DirectiveFilter{RunID: c.runID, ActiveOnly: true})
	if err != nil {
		return "", err
	}
	metrics, err := LatestMetricsFromDecisions(ctx, c.store, c.clock, c.runID)
	if err != nil {
		return "", err
	}

	counts := make(map[store.BacklogStatus]int)
	for i := range backlog {
		counts[backlog[i].Status]++
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Run: %s\n", run.ID)
	fmt.Fprintf(&b, "Objective: %s\n", cmp.Or(run.Objective, "(none)"))
	if run.PlanRef != "" {
		fmt.Fprintf(&b, "PlanRef: %s\n", run.PlanRef)
	}
	if run.Lease != nil {
		fmt.Fprintf(&b, "Lease: holder=%s expires=%s\n", run.Lease.HolderID, run.Lease.ExpiresAt.Format(time.RFC3339))
	} else {
		b.WriteString("Lease: released\n")
	}

	fmt.Fprintf(&b, "\nBacklog (%d total: open=%d in_progress=%d blocked=%d done=%d dropped=%d):\n",
		len(backlog),
		counts[store.BacklogOpen],
		counts[store.BacklogInProgress],
		counts[store.BacklogBlocked],
		counts[store.BacklogDone],
		counts[store.BacklogDropped],
	)
	for i := range backlog {
		it := backlog[i]
		fmt.Fprintf(&b, "- %s [%s %s] lane=%s: %s\n",
			it.ID, it.Priority, it.Status, cmp.Or(it.LaneID, "-"), it.Title)
	}

	fmt.Fprintf(&b, "\nLanes (%d):\n", len(lanes))
	for i := range lanes {
		ln := lanes[i]
		rec := cmp.Or(ln.RecipeName, recipe.DefaultName)
		if ln.RecipeVersion > 0 {
			rec = fmt.Sprintf("%s@v%d", rec, ln.RecipeVersion)
		}
		fmt.Fprintf(&b, "- %s: item=%s status=%s phase=%s round=%d recipe=%s pr=#%d head=%s approval=%s merge=%s checks=%s\n",
			ln.ID,
			cmp.Or(ln.ItemID, "-"),
			ln.Status,
			cmp.Or(ln.Phase, "-"),
			ln.Round,
			rec,
			ln.PR,
			cmp.Or(ln.PRHead, "-"),
			cmp.Or(ln.ApprovalSHA, "-"),
			cmp.Or(ln.MergeSHA, "-"),
			cmp.Or(ln.Checks, "-"),
		)
	}

	fmt.Fprintf(&b, "\nActive Directives (%d):\n", len(dirs))
	for i := range dirs {
		d := dirs[i]
		fmt.Fprintf(&b, "- [%s] kind=%s source=%s scope=%s: %s\n",
			d.ID, d.Kind, d.Source, cmp.Or(d.Scope, "run"), d.Text)
	}

	fmt.Fprintf(&b, "\nMetrics: %s\n", metrics.Summary())
	return b.String(), nil
}

func (c *Concierge) handleLaneShow(ctx context.Context, laneID string) (string, error) {
	laneID = strings.TrimSpace(laneID)
	ln, err := c.store.Lanes().Get(ctx, c.runID, laneID)
	if err != nil {
		return "", err
	}
	decs, err := c.store.Decisions().List(ctx, store.DecisionFilter{
		RunID:  c.runID,
		LaneID: laneID,
		Limit:  10,
	})
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Lane %s (item=%s status=%s phase=%s round=%d attempt=%d recipe=%s@v%d)\n",
		ln.ID, cmp.Or(ln.ItemID, "-"), ln.Status, cmp.Or(ln.Phase, "-"), ln.Round, ln.Attempt,
		cmp.Or(ln.RecipeName, recipe.DefaultName), max(ln.RecipeVersion, 1))
	fmt.Fprintf(&b, "PR: #%d branch=%s head=%s approval=%s merge=%s checks=%s\n",
		ln.PR, cmp.Or(ln.Branch, "-"), cmp.Or(ln.PRHead, "-"), cmp.Or(ln.ApprovalSHA, "-"),
		cmp.Or(ln.MergeSHA, "-"), cmp.Or(ln.Checks, "-"))
	fmt.Fprintf(&b, "Decisions (%d):\n", len(decs))
	for i := range decs {
		d := decs[i]
		fmt.Fprintf(&b, "- [seq=%d %s %s] %s: %s\n", d.Seq, d.Category, d.Actor, d.Tool, d.Result)
	}
	return b.String(), nil
}

func (c *Concierge) handleDecisionsSince(ctx context.Context, p ConciergeDecisionsParams) (string, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = 25
	}
	filter := store.DecisionFilter{
		RunID:    c.runID,
		LaneID:   strings.TrimSpace(p.LaneID),
		Category: store.DecisionCategory(strings.TrimSpace(p.Category)),
		SinceSeq: p.SinceSeq,
		Limit:    limit,
	}
	decs, err := c.store.Decisions().List(ctx, filter)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Decisions (%d):\n", len(decs))
	for i := range decs {
		d := decs[i]
		fmt.Fprintf(&b, "- [seq=%d %s actor=%s lane=%s tool=%s tokens=%d cost=$%.4f] %s -> %s\n",
			d.Seq, d.Category, d.Actor, cmp.Or(d.LaneID, "-"), d.Tool,
			d.InputTokens+d.OutputTokens, d.CostUSD, d.Reason, d.Result)
	}
	return b.String(), nil
}

func (c *Concierge) handlePRCI(ctx context.Context, laneID string) (string, error) {
	laneID = strings.TrimSpace(laneID)
	ln, err := c.store.Lanes().Get(ctx, c.runID, laneID)
	if err != nil {
		return "", err
	}
	evs, err := c.store.Events().List(ctx, store.EventFilter{
		RunID:  c.runID,
		LaneID: laneID,
		Kinds:  []store.EventKind{store.EventCI},
	})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Lane %s PR #%d head=%s approval=%s stale=%t merge=%s checks=%s ci_events=%d\n",
		ln.ID, ln.PR, cmp.Or(ln.PRHead, "-"), cmp.Or(ln.ApprovalSHA, "-"), ln.ApprovalStale(),
		cmp.Or(ln.MergeSHA, "-"), cmp.Or(ln.Checks, "-"), len(evs))
	for i := max(len(evs)-5, 0); i < len(evs); i++ {
		ev := evs[i]
		fmt.Fprintf(&b, "- [seq=%d %s] %s %s\n", ev.Seq, ev.ID, ev.Summary, string(ev.Payload))
	}
	return b.String(), nil
}

func (c *Concierge) handleDirectiveSubmit(
	ctx context.Context,
	threadID, turnID string,
	p DirectiveSubmitParams,
) (string, error) {
	text := strings.TrimSpace(p.Text)
	if text == "" {
		return "", fmt.Errorf("%w: directive text is required", store.ErrInvalidArgument)
	}
	kind := store.DirectiveKind(strings.ToLower(strings.TrimSpace(p.Kind)))
	switch kind {
	case store.DirectiveStanding, store.DirectiveInstruction, store.DirectiveRecipeEdit, store.DirectivePlanImport:
	case "":
		kind = store.DirectiveInstruction
	default:
		return "", fmt.Errorf("%w: invalid directive kind %q", store.ErrInvalidArgument, p.Kind)
	}

	now := c.clock.Now().UTC()
	var expiresAt *time.Time
	if expStr := strings.TrimSpace(p.Expiry); expStr != "" {
		if d, err := time.ParseDuration(expStr); err == nil {
			t := now.Add(d)
			expiresAt = &t
		} else if ts, err := time.Parse(time.RFC3339, expStr); err == nil {
			t := ts.UTC()
			expiresAt = &t
		} else {
			return "", fmt.Errorf("%w: invalid expiry %q", store.ErrInvalidArgument, p.Expiry)
		}
	}

	dirID := c.nextID("dir-concierge")
	dir := store.Directive{
		ID:        dirID,
		RunID:     c.runID,
		Kind:      kind,
		Source:    store.DirectiveSourceConcierge,
		Scope:     strings.TrimSpace(p.Scope),
		Ref:       strings.TrimSpace(p.Ref),
		Text:      text,
		ExpiresAt: expiresAt,
		Status:    store.DirectiveActive,
	}
	if err := c.store.Directives().Add(ctx, dir); err != nil {
		return "", err
	}

	payloadBytes, _ := json.Marshal(dir)
	if _, err := c.store.Events().Append(ctx, store.Event{
		RunID:   c.runID,
		Kind:    store.EventOwnerMessage,
		Class:   store.EventClassJudgment,
		LaneID:  dir.Scope,
		Source:  ActorConcierge,
		Summary: fmt.Sprintf("owner directive (%s): %s", dir.Kind, dir.Text),
		Payload: payloadBytes,
		Urgent:  true,
	}); err != nil {
		return "", err
	}

	rawArgs, _ := json.Marshal(p)
	_, _ = c.store.Decisions().Append(ctx, store.Decision{
		RunID:    c.runID,
		Category: store.DecisionJudgment,
		Actor:    ActorConcierge,
		ThreadID: threadID,
		TurnID:   turnID,
		LaneID:   dir.Scope,
		Tool:     ToolDirectiveSubmit,
		Reason:   fmt.Sprintf("submitted %s directive", dir.Kind),
		Result:   dir.ID,
		Args:     rawArgs,
		Vetted:   true,
	})

	return fmt.Sprintf("submitted directive %s (kind=%s)", dir.ID, dir.Kind), nil
}
