package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bazelment/yoloswe/orca/adopt"
	"github.com/bazelment/yoloswe/orca/chat"
	"github.com/bazelment/yoloswe/orca/clock"
	memorychat "github.com/bazelment/yoloswe/orca/concierge"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
)

const (
	ActorConcierge          = "concierge"
	ToolConciergeStatus     = "status"
	ToolConciergeLaneShow   = "lane.show"
	ToolConciergeDecisions  = "decisions.since"
	ToolConciergePRCI       = "pr.ci"
	ToolDirectiveSubmit     = "directive.submit"
	ToolMemorySubmit        = "memory.submit"
	ToolMemoryUndo          = "memory.undo"
	ToolMemoryRead          = "memory.read"
	ToolRunSettings         = memorychat.ToolRunSettings
	ToolRunStart            = "run.start"
	ToolSwarms              = "swarms"
	DefaultConciergeMaxTurn = 6
	// conciergeHistoryMessages bounds resumed thread history.
	conciergeHistoryMessages = 40
	// swarmsReadBytes bounds the swarms tool output.
	swarmsReadBytes = 12000
)

// ConciergeSystemPrompt is the system prompt for owner chat sessions.
const ConciergeSystemPrompt = `You are the Nebula Swarm Concierge.
When the owner states a new goal or asks to start work, call run.start with the goal text. Do not submit a generic directive for a new goal.
You are the front desk for unclear or mixed messages: clarify with one question only if you must; otherwise save rules with memory.submit and forward plan changes or orders with directive.submit. Use run.control to pause, resume or stop execution. Use run.settings to tune concurrency and stall timeout.
Each answer must address only the question asked; never open with or repeat status or conclusions from earlier, unrelated turns or other runs.
Answer the owner's questions accurately and concisely using read-only Store tools (status, lane.show, decisions.since, pr.ci). For runs this chat holds (including adopted runs), answer from the Store and live probes via status, lane.show, or pr.ci—do not call swarms for a held run. For questions about other swarms on this machine, call swarms once (pass the run filter when asking about a specific swarm) and answer from it.
	Never invent facts not present in the Store. Owner corrections become memory.submit proposals saved immediately with a source quote; memory.undo reverts the last change.`

type ConciergeStatusParams struct {
	RunID          string `json:"run_id,omitempty" jsonschema:"description=Optional run ID to inspect"`
	IncludeMetrics bool   `json:"include_metrics,omitempty" jsonschema:"description=Include decision, token, cost, and idle metrics"`
}

type ConciergeLaneShowParams struct {
	LaneID string `json:"lane_id" jsonschema:"required,description=Lane ID to inspect"`
	RunID  string `json:"run_id,omitempty" jsonschema:"description=Optional run ID that owns the lane"`
}

type ConciergeDecisionsParams struct {
	RunID    string `json:"run_id,omitempty" jsonschema:"description=Optional run ID filter"`
	Category string `json:"category,omitempty" jsonschema:"description=Optional category filter (auto or judgment)"`
	LaneID   string `json:"lane_id,omitempty" jsonschema:"description=Optional lane ID filter"`
	SinceSeq int64  `json:"since_seq,omitempty" jsonschema:"description=Return decisions with seq > since_seq"`
	Limit    int    `json:"limit,omitempty" jsonschema:"description=Max decisions to return (default 25)"`
}

type ConciergePRCIParams struct {
	LaneID string `json:"lane_id" jsonschema:"required,description=Lane ID to inspect PR and CI checks"`
	RunID  string `json:"run_id,omitempty" jsonschema:"description=Optional run ID that owns the lane"`
}

type DirectiveSubmitParams struct {
	Kind   string `json:"kind,omitempty" jsonschema:"enum=standing,enum=instruction,enum=recipe_edit,enum=plan_import,description=Directive kind (standing, instruction, recipe_edit, plan_import)"`
	Text   string `json:"text" jsonschema:"required,description=Directive rule, instruction, recipe delta, or plan summary"`
	Ref    string `json:"ref,omitempty" jsonschema:"description=Optional plan reference or target recipe name"`
	Scope  string `json:"scope,omitempty" jsonschema:"description=Optional lane ID or scope"`
	Expiry string `json:"expiry,omitempty" jsonschema:"description=Optional RFC3339 timestamp or Go duration (e.g. 24h)"`
	RunID  string `json:"run_id,omitempty" jsonschema:"description=Run the directive is about when the owner names one; defaults to the run this chat holds"`
}

// SwarmsParams optionally filters swarms by run ID or substring.
type SwarmsParams struct {
	Run string `json:"run,omitempty" jsonschema:"description=Optional run ID or substring filter (e.g. trial-m1)"`
}

type RunStartParams struct {
	Goal string `json:"goal" jsonschema:"required,description=Goal for the new run"`
}

// Concierge hosts a fresh owner chat thread with read-only Store tools and `directive.submit`.
//
//nolint:govet
type Concierge struct {
	store        store.Store
	clock        clock.Clock
	client       model.ModelClient
	runID        string
	modelName    string
	threadID     string
	history      []model.Message
	memoryEditor memorychat.MemoryEditor
	mu           sync.Mutex
	// startAsked is set when run.start was refused and the concierge asked
	// the owner a question; a plain yes on the next turn confirms.
	startAsked bool
	RunStart   func(context.Context, string) (string, error)
	// HeldRuns lists the run IDs currently held by this chat (attached run and
	// any adopted runs). When nil, Concierge infers held runs from Store.
	HeldRuns func(context.Context) ([]string, error)
	// HostRun reads optional host-directory metadata for an adopted run ID.
	HostRun func(string) (adopt.Run, error)
	// Probe reads live Nebula terminal state and GitHub PR state for held lanes.
	Probe     chat.HostProbe
	probeFrom func() chat.HostProbe
	// Swarms lists this machine's orca runs and host swarms for the swarms
	// tool; nil leaves the tool out.
	Swarms       func(context.Context) chat.SwarmList
	SwarmsFilter func(context.Context, string) chat.SwarmList
	// OwnerRun picks the run a directive is about from the run the tool
	// names and the owner's words; nil keeps directives on the chat's run.
	OwnerRun func(ctx context.Context, run, text string) (string, error)
	// Wake asks the controller of a run to act on a new directive now.
	Wake func(runID string)
	// Logger receives one INFO line per tool call and a WARN on a budget stop.
	Logger *slog.Logger
}

// directiveReceipt is a directive this turn wrote and the run it is on.
type directiveReceipt struct{ id, run string }

// withReceipts tells the owner where each directive was stored when needed.
func withReceipts(answer string, receipts []directiveReceipt) (string, string) {
	var lines []string
	for _, r := range receipts {
		if !chat.Mentions(answer, r.run) {
			lines = append(lines, fmt.Sprintf("Directive %s is saved on run %s.", r.id, r.run))
		}
	}
	if len(lines) == 0 {
		return answer, ""
	}
	extra := strings.Join(lines, "\n")
	return strings.TrimSpace(answer + "\n" + extra), extra
}

// ownerTurn holds per-turn start permission and directive receipts.
type ownerTurn struct {
	receipts         []directiveReceipt
	allowed, refused bool
}

// runStartRefusal is the tool result when the owner did not ask to start.
const runStartRefusal = "Not started: the owner did not explicitly ask to start a run. Do not start one. If they may want one, ask them whether to start a run for this goal; a plain yes confirms."

// Attach selects a run for the Concierge.
func (c *Concierge) Attach(st store.Store, runID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store, c.runID, c.threadID, c.history, c.startAsked = st, strings.TrimSpace(runID), "", nil, false
}

// ResetHistory clears in-memory turn history when the held-run set changes
// (e.g., on /adopt or /release) so earlier turns do not bleed into new answers.
func (c *Concierge) ResetHistory() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.threadID, c.history, c.startAsked = "", nil, false
}

// BindBackend connects the Concierge to a StoreBackend's held runs, live
// probes, swarm discovery, directive routing, wakeups, and attach/adopt/release
// lifecycle hooks.
func (c *Concierge) BindBackend(b *chat.StoreBackend) {
	if b == nil {
		return
	}
	c.Swarms = b.LiveSwarms
	c.SwarmsFilter = b.LiveSwarmsFiltered
	c.OwnerRun = b.OwnerRun
	c.Wake = b.Wake
	c.HeldRuns = b.HeldRuns
	c.HostRun = b.ReadHostRun
	c.probeFrom = func() chat.HostProbe {
		if c.Probe.TerminalState != nil || c.Probe.PR != nil || len(c.Probe.Off) > 0 {
			return c.Probe
		}
		return b.HostProbe
	}
	b.OnAttach = c.Attach
	b.OnHoldChange = c.ResetHistory
}

func (c *Concierge) hostProbe() chat.HostProbe {
	if c.probeFrom != nil {
		return c.probeFrom()
	}
	return c.Probe
}

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
	return nextUniqueID(prefix, c.clock)
}

func (c *Concierge) ensureThread(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.threadID != "" {
		id := c.threadID
		c.mu.Unlock()
		return id, nil
	}
	c.mu.Unlock()

	candidateID := c.nextID("concierge-" + c.runID)
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

// Resume continues the most recent concierge thread and returns its history.
func (c *Concierge) Resume(ctx context.Context) ([]model.Message, error) {
	threads, err := c.store.Threads().ListThreads(ctx, store.ThreadFilter{RunID: c.runID, Role: store.ThreadRoleConcierge})
	if err != nil || len(threads) == 0 {
		return nil, err
	}
	latest := slices.MaxFunc(threads, func(a, b store.Thread) int {
		return cmp.Or(a.UpdatedAt.Compare(b.UpdatedAt), cmp.Compare(a.ID, b.ID))
	})
	items, err := c.store.Threads().ListItems(ctx, latest.ID, "", 0, 0)
	if err != nil {
		return nil, err
	}
	var history []model.Message
	for i := range items {
		it := &items[i]
		if strings.TrimSpace(it.Content) == "" {
			continue
		}
		switch it.Kind {
		case store.ItemKindUserMessage:
			history = append(history, model.NewUserMessage(it.Content))
		case store.ItemKindAssistantMessage:
			history = append(history, model.NewAssistantMessage(model.NewTextBlock(it.Content)))
		}
	}
	history = history[max(len(history)-conciergeHistoryMessages, 0):]
	// A failed turn can leave consecutive user messages; normalize the tail so
	// providers that require alternating roles can accept resumed history.
	var normalized []model.Message
	for _, msg := range history {
		if len(normalized) > 0 && normalized[len(normalized)-1].Role == msg.Role {
			prev := normalized[len(normalized)-1].Text()
			joined := strings.TrimSpace(prev + "\n" + msg.Text())
			if msg.Role == model.RoleUser {
				normalized[len(normalized)-1] = model.NewUserMessage(joined)
			} else {
				normalized[len(normalized)-1] = model.NewAssistantMessage(model.NewTextBlock(joined))
			}
			continue
		}
		normalized = append(normalized, msg)
	}
	history = normalized
	if len(history) > 0 && history[0].Role == model.RoleAssistant {
		history = history[1:]
	}
	if latest.Status == store.ThreadStatusArchived {
		if err := c.store.Threads().UpdateThreadStatus(ctx, latest.ID, store.ThreadStatusIdle); err != nil {
			return nil, err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.threadID, c.history = latest.ID, slices.Clone(history)
	return history, nil
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
	return c.ask(ctx, question, nil)
}

// AskStream emits each assistant response as it arrives during the tool loop.
func (c *Concierge) AskStream(ctx context.Context, question string, emit func(string)) error {
	ctx, stream := withChatStream(ctx, emit)
	_, err := c.ask(ctx, question, stream.complete)
	return err
}
func (c *Concierge) ask(ctx context.Context, question string, emit func(string)) (string, error) {
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

	if strings.EqualFold(question, "undo") || strings.EqualFold(question, "forget") {
		answer, undoErr := c.memoryEditor.Undo(ctx, c.store)
		status, errText := store.TurnStatusCompleted, ""
		if undoErr != nil {
			status, errText = store.TurnStatusFailed, undoErr.Error()
		}
		_, _ = c.store.Threads().AppendItem(cleanupCtx, store.Item{ThreadID: threadID, TurnID: turnID, Kind: store.ItemKindAssistantMessage, Content: answer})
		if err := c.store.Threads().CompleteTurn(cleanupCtx, threadID, turnID, status, model.Usage{}, errText); err != nil {
			return "", err
		}
		return answer, undoErr
	}
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
		if emit != nil {
			emit(report)
		}
		return report, nil
	}

	c.mu.Lock()
	prior := slices.Clone(c.history)
	start := &ownerTurn{allowed: chat.IsStartRequest(question) || (c.startAsked && chat.IsConfirmation(question))}
	c.startAsked = false
	c.mu.Unlock()

	held := c.heldRunIDs(ctx)
	msgs := append(c.relevantHistory(ctx, prior, question, held), model.NewUserMessage(question))

	memoryEcho := ""
	reg := c.buildToolRegistry(threadID, turnID, question, &memoryEcho, start)
	systemPrompt := ConciergeSystemPrompt
	if len(held) > 0 && (len(held) > 1 || held[0] != frontDeskRunID) {
		systemPrompt += "\n\n## Held Runs\nThis chat currently holds in its Store: " + strings.Join(held, ", ") + ". Use status, lane.show, and pr.ci (not swarms) to answer questions about held runs."
	}
	if memories, memErr := c.store.Memories().List(ctx, store.MemoryFilter{RunID: c.runID}); memErr == nil {
		systemPrompt += "\n\n## Canonical Memory Index\n" + store.MemoryIndex(memories)
	}
	req := model.Request{
		Model:     c.modelName,
		System:    systemPrompt,
		Messages:  msgs,
		Tools:     reg.Definitions(),
		MaxTokens: model.DefaultMaxTokens,
	}

	loopRes, loopErr := model.RunToolLoop(
		ctx,
		c.client,
		req,
		reg,
		// The last turn of the budget is kept for a forced answer.
		model.WithMaxTurns(DefaultConciergeMaxTurn-1),
		model.WithToolCallLog(c.Logger, ActorConcierge),
		model.WithOnTurn(func(_ int, resp model.Response) {
			if txt := strings.TrimSpace(resp.Text()); txt != "" {
				if emit != nil {
					emit(txt)
				}
				if memoryEcho != "" {
					txt = memoryEcho
				}
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
	if errors.Is(loopErr, model.ErrMaxTurnsExceeded) && loopRes != nil {
		var final model.Usage
		answer, final = c.budgetAnswer(ctx, req, question, loopRes.Messages[len(msgs):])
		usage, loopErr = loopRes.Usage.Add(final), nil
		if emit != nil {
			emit(answer)
		}
		_, _ = c.store.Threads().AppendItem(cleanupCtx, store.Item{ThreadID: threadID, TurnID: turnID, Kind: store.ItemKindAssistantMessage, Content: answer})
		// The transcript ends on an unanswered tool call; keep the turn as
		// plain text so the next request is well formed.
		c.mu.Lock()
		c.history = append(plainHistory(prior), model.NewUserMessage(question), model.NewAssistantMessage(model.NewTextBlock(answer)))
		c.mu.Unlock()
	} else if loopRes != nil {
		usage = loopRes.Usage
		answer = strings.TrimSpace(loopRes.Response.Text())
		c.mu.Lock()
		c.history = append(plainHistory(prior), model.NewUserMessage(question), model.NewAssistantMessage(model.NewTextBlock(answer)))
		c.startAsked = start.refused && strings.HasSuffix(answer, "?")
		c.mu.Unlock()
	}

	if memoryEcho != "" {
		answer = memoryEcho
	}
	if quoted, extra := withReceipts(answer, start.receipts); extra != "" {
		answer = quoted
		if emit != nil {
			emit(extra)
		}
		_, _ = c.store.Threads().AppendItem(cleanupCtx, store.Item{ThreadID: threadID, TurnID: turnID, Kind: store.ItemKindAssistantMessage, Content: extra})
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

// budgetPrompt asks for an answer once the tool budget is spent.
const budgetPrompt = "Your tool budget for this question is spent. Answer the owner now, using only the tool results above; say plainly what you could not check."

// budgetAnswer asks once without tools after the loop spends its turns, then
// falls back to reporting the collected results.
func (c *Concierge) budgetAnswer(ctx context.Context, req model.Request, question string, turn []model.Message) (string, model.Usage) {
	results := toolResults(turn)
	if c.Logger != nil {
		c.Logger.Warn("concierge turn budget spent", "turns", DefaultConciergeMaxTurn-1, "tool_results", len(results))
	}
	var prompt strings.Builder
	fmt.Fprintf(&prompt, "Owner question: %s\n\nTool results so far:\n", question)
	for _, r := range results {
		prompt.WriteString(r + "\n")
	}
	if len(results) == 0 {
		prompt.WriteString("(none)\n")
	}
	prompt.WriteString("\n" + budgetPrompt)
	final := req.Clone()
	final.Tools = nil
	final.Messages = append(plainHistory(req.Messages[:len(req.Messages)-1]), model.NewUserMessage(prompt.String()))
	var usage model.Usage
	resp, err := c.client.CreateMessage(ctx, final)
	if err == nil && resp != nil {
		usage = resp.Usage
		if txt := strings.TrimSpace(resp.Text()); txt != "" && len(resp.ToolCalls()) == 0 {
			return txt, usage
		}
	}
	if c.Logger != nil {
		c.Logger.Warn("concierge forced answer failed; reporting raw tool results", "error", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "I used my %d-turn budget before I could finish answering. What I found so far:", DefaultConciergeMaxTurn)
	if len(results) == 0 {
		b.WriteString(" nothing yet.")
	}
	for _, r := range results {
		b.WriteString("\n" + r)
	}
	return b.String(), usage
}

// toolResults renders unique tool results in encounter order.
func toolResults(msgs []model.Message) []string {
	names := map[string]string{}
	seen := map[string]struct{}{}
	var out []string
	for i := range msgs {
		for j := range msgs[i].Content {
			blk := &msgs[i].Content[j]
			switch {
			case blk.Type == model.BlockTypeToolUse:
				names[blk.ID] = blk.Name
			case blk.Type == model.BlockTypeToolResult:
				limit := 2000
				if names[blk.ToolUseID] == ToolSwarms {
					limit = swarmsReadBytes
				}
				r := fmt.Sprintf("[%s] %s", cmp.Or(names[blk.ToolUseID], "tool"), clipText(strings.TrimSpace(blk.Content), limit))
				if blk.IsError {
					r = fmt.Sprintf("[%s] error: %s", cmp.Or(names[blk.ToolUseID], "tool"), clipText(strings.TrimSpace(blk.Content), 300))
				}
				if _, ok := seen[r]; !ok {
					seen[r] = struct{}{}
					out = append(out, r)
				}
			}
		}
	}
	return out
}

// plainHistory removes tool blocks from earlier messages.
func plainHistory(msgs []model.Message) []model.Message {
	var out []model.Message
	for i := range msgs {
		txt := strings.TrimSpace(msgs[i].Text())
		if txt == "" {
			continue
		}
		if msgs[i].Role == model.RoleAssistant {
			out = append(out, model.NewAssistantMessage(model.NewTextBlock(txt)))
		} else {
			out = append(out, model.NewUserMessage(txt))
		}
	}
	return out
}

// relevantHistory drops earlier user/assistant turns that are about a different
// run than the current question so unrelated run status never bleeds into the
// current answer, while keeping follow-up context for the active subject run.
// When the owner sends a new instruction rather than a question or confirmation,
// earlier question/answer turns are dropped so the reply addresses the instruction.
func (c *Concierge) relevantHistory(ctx context.Context, history []model.Message, question string, held []string) []model.Message {
	plain := plainHistory(history)
	if len(plain) == 0 || chat.IsConfirmation(question) || asksAboutPriorConversation(question) {
		return plain
	}
	if isOwnerInstruction(question) {
		return nil
	}
	knownRuns := slices.Clone(held)
	laneToRun := map[string]string{}
	if c.store != nil {
		if runs, err := c.store.Runs().List(ctx); err == nil {
			for i := range runs {
				if runs[i].ID != "" && !slices.Contains(knownRuns, runs[i].ID) {
					knownRuns = append(knownRuns, runs[i].ID)
				}
			}
		}
		for _, rID := range knownRuns {
			if rID == "" || rID == frontDeskRunID {
				continue
			}
			if lanes, err := c.store.Lanes().List(ctx, store.LaneFilter{RunID: rID}); err == nil {
				for i := range lanes {
					laneToRun[strings.ToLower(lanes[i].ID)] = strings.ToLower(rID)
				}
			}
		}
	}
	tokensForText := func(lower string) []string {
		toks := matchedRunTokens(lower, knownRuns)
		for laneID, rID := range laneToRun {
			if chat.Mentions(lower, laneID) {
				for _, t := range matchedRunTokens(rID, knownRuns) {
					if !slices.Contains(toks, t) {
						toks = append(toks, t)
					}
				}
			}
		}
		return toks
	}
	type turnSpan struct {
		runs       []string
		start, end int
	}
	var spans []turnSpan
	for i := 0; i < len(plain); {
		end := i + 1
		if end < len(plain) && plain[i].Role == model.RoleUser && plain[end].Role == model.RoleAssistant {
			end++
		}
		userLower := strings.ToLower(plain[i].Text())
		runs := tokensForText(userLower)
		if len(runs) == 0 && isFollowUpQuestion(plain[i].Text()) && end > i+1 {
			runs = tokensForText(strings.ToLower(plain[i+1].Text()))
		}
		spans = append(spans, turnSpan{
			start: i,
			end:   end,
			runs:  runs,
		})
		i = end
	}
	qLower := strings.ToLower(question)
	targetRuns := tokensForText(qLower)
	effectiveTargets := slices.Clone(targetRuns)
	if len(effectiveTargets) == 0 {
		for _, id := range held {
			if id == "" || id == frontDeskRunID {
				continue
			}
			for _, tok := range matchedRunTokens(strings.ToLower(id), knownRuns) {
				if !slices.Contains(effectiveTargets, tok) {
					effectiveTargets = append(effectiveTargets, tok)
				}
			}
		}
	}
	if len(effectiveTargets) == 0 {
		for i := len(spans) - 1; i >= 0; i-- {
			if len(spans[i].runs) > 0 {
				effectiveTargets = slices.Clone(spans[i].runs)
				break
			}
		}
	}
	var out []model.Message
	for _, sp := range spans {
		keep := true
		switch {
		case len(sp.runs) > 0 && len(effectiveTargets) > 0:
			keep = false
			for _, tok := range sp.runs {
				if slices.Contains(effectiveTargets, tok) {
					keep = true
					break
				}
			}
		case len(sp.runs) == 0 && (len(targetRuns) > 0 || !isFollowUpQuestion(question)):
			keep = false
		}
		if keep {
			out = append(out, plain[sp.start:sp.end]...)
		}
	}
	return out
}

func asksAboutPriorConversation(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	for _, phrase := range []string{"what did i ask", "earlier", "before?", "previous question", "last question"} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

func isFollowUpQuestion(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	if lower == "thanks" || lower == "thank you" || lower == "ok" || lower == "okay" {
		return true
	}
	for _, prefix := range []string{"and ", "also ", "what about ", "how about "} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return strings.Contains(trimmed, "?") && len(matchedRunTokens(lower, nil)) == 0
}

func isOwnerInstruction(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || strings.Contains(trimmed, "?") || chat.IsConfirmation(trimmed) {
		return false
	}
	lower := strings.ToLower(trimmed)
	switch lower {
	case "hello", "hi", "hey", "thanks", "thank you", "status":
		return false
	}
	fields := strings.FieldsFunc(lower, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '\'' && r != '-'
	})
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "what", "what's", "whats", "which", "how", "how's", "hows", "where", "where's", "wheres",
		"why", "who", "when", "is", "are", "can", "could", "would", "should", "do", "does", "did",
		"and", "also", "status", "show", "list", "describe", "explain", "tell":
		return false
	}
	return true
}

// matchedRunTokens extracts run identifiers or hyphenated run slugs mentioned
// in lower-cased text.
func matchedRunTokens(lower string, knownRuns []string) []string {
	var out []string
	add := func(s string) {
		s = strings.ToLower(strings.TrimSpace(s))
		if len(s) >= 3 && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	for _, id := range knownRuns {
		if id == "" || id == frontDeskRunID {
			continue
		}
		idLower := strings.ToLower(id)
		if strings.Contains(lower, idLower) {
			add(idLower)
		}
		if slug := runSlug(idLower); slug != "" && strings.Contains(lower, slug) {
			add(slug)
		}
	}
	fields := strings.FieldsFunc(lower, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-'
	})
	for idx, w := range fields {
		w = strings.Trim(w, "-")
		if !strings.Contains(w, "-") || len(w) < 4 {
			continue
		}
		nearRunWord := (idx > 0 && (fields[idx-1] == "run" || fields[idx-1] == "swarm" || fields[idx-1] == "hold" || fields[idx-1] == "adopt")) ||
			(idx+1 < len(fields) && (fields[idx+1] == "run" || fields[idx+1] == "swarm" || fields[idx+1] == "hold"))
		if nearRunWord || isDatedRunID(w) {
			add(w)
			if slug := runSlug(w); slug != "" {
				add(slug)
			}
		}
	}
	return out
}

func isDatedRunID(s string) bool {
	if len(s) < 10 {
		return false
	}
	for i := range 8 {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s[8] == '-'
}

func runSlug(id string) string {
	if !isDatedRunID(id) {
		return ""
	}
	rest := id[9:]
	if len(rest) > 7 {
		digits := true
		for i := range 6 {
			if rest[i] < '0' || rest[i] > '9' {
				digits = false
				break
			}
		}
		if digits && rest[6] == '-' {
			return rest[7:]
		}
	}
	return rest
}

// heldRunIDs returns the runs currently held by this chat (attached run and any
// adopted runs reported by HeldRuns, or inferred from Store when HeldRuns is nil).
func (c *Concierge) heldRunIDs(ctx context.Context) []string {
	var out []string
	seen := map[string]bool{}
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if c.HeldRuns != nil {
		ids, err := c.HeldRuns(ctx)
		if err != nil && c.Logger != nil {
			c.Logger.Warn("list held runs failed", "error", err)
		}
		for _, id := range ids {
			if id != frontDeskRunID {
				add(id)
			}
		}
	} else if c.store != nil {
		now := c.clock.Now()
		if runs, err := c.store.Runs().List(ctx); err == nil {
			var nonFrontDesk []string
			for i := range runs {
				r := &runs[i]
				if r.ID == "" || r.ID == frontDeskRunID {
					continue
				}
				nonFrontDesk = append(nonFrontDesk, r.ID)
				if r.DefaultRecipe == "adopted" || (r.Lease != nil && !r.Lease.Expired(now)) {
					add(r.ID)
				}
			}
			if len(out) == 0 && len(nonFrontDesk) == 1 {
				add(nonFrontDesk[0])
			}
		}
	}
	if c.runID != "" && (c.runID != frontDeskRunID || len(out) == 0) {
		add(c.runID)
	}
	return out
}

func (c *Concierge) defaultTargetRun(ctx context.Context, text string) string {
	held := c.heldRunIDs(ctx)
	var nonFront []string
	for _, id := range held {
		if id != "" && id != frontDeskRunID {
			nonFront = append(nonFront, id)
		}
	}
	if len(nonFront) > 0 {
		lower := strings.ToLower(text)
		for _, id := range nonFront {
			idLower := strings.ToLower(id)
			if strings.Contains(lower, idLower) {
				return id
			}
			if slug := runSlug(idLower); slug != "" && strings.Contains(lower, slug) {
				return id
			}
		}
		if c.store != nil {
			for _, id := range nonFront {
				if lanes, err := c.store.Lanes().List(ctx, store.LaneFilter{RunID: id}); err == nil {
					for i := range lanes {
						if chat.Mentions(text, lanes[i].ID) {
							return id
						}
					}
				}
			}
		}
		if c.runID == frontDeskRunID || len(nonFront) == 1 {
			return nonFront[0]
		}
	}
	return c.runID
}

// clipText bounds s to n bytes without splitting a rune.
func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "...[truncated]"
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

// buildToolRegistry builds the turn's tools. run.start launches only when
// start allows it: an explicit request or confirmation after a refusal.
func (c *Concierge) buildToolRegistry(threadID, turnID, quote string, memoryEcho *string, start *ownerTurn) *model.ToolRegistry {
	reg := model.NewToolRegistry()
	model.AddTool[RunStartParams](reg, ToolRunStart, "Start a new run for an owner goal", func(ctx context.Context, p RunStartParams) (string, error) {
		goal := strings.TrimSpace(p.Goal)
		if goal == "" {
			return "", fmt.Errorf("run.start requires a goal")
		}
		if start == nil || !start.allowed {
			if start != nil {
				start.refused = true
			}
			return runStartRefusal, nil
		}
		if c.RunStart == nil {
			return "", fmt.Errorf("run.start unavailable")
		}
		return c.RunStart(ctx, goal)
	})
	model.AddTool[memorychat.RunControlParams](reg, memorychat.ToolRunControl, "Pause, resume or stop execution", func(ctx context.Context, p memorychat.RunControlParams) (string, error) {
		targetRun := c.defaultTargetRun(ctx, quote)
		backend := chat.NewStoreBackend(c.store, targetRun)
		return memorychat.Control(ctx, backend.Command, p)
	})
	model.AddTool[memorychat.RunSettingsParams](reg, memorychat.ToolRunSettings,
		"Read or change live run settings", func(ctx context.Context, p memorychat.RunSettingsParams) (string, error) {
			targetRun := c.defaultTargetRun(ctx, quote+" "+p.Words)
			current, err := c.store.Runs().GetSettings(ctx, targetRun)
			if err != nil {
				return "", err
			}
			if p.Action == "get" || p.Action == "" {
				return fmt.Sprintf("run settings: concurrency_cap=%d stall_timeout=%s paused=%t", current.ConcurrencyCap, current.StallTimeout, current.Paused), nil
			}
			if p.Action != "set" {
				return "", fmt.Errorf("run.settings action must be get or set")
			}
			next := current
			if p.ConcurrencyCap != nil {
				if *p.ConcurrencyCap < 0 || *p.ConcurrencyCap > 16 {
					return "", fmt.Errorf("run.settings concurrency cap must be 0-16")
				}
				next.ConcurrencyCap = *p.ConcurrencyCap
			}
			if p.StallTimeout != "" {
				d, e := time.ParseDuration(p.StallTimeout)
				if e != nil || d < 5*time.Minute || d > 24*time.Hour {
					return "", fmt.Errorf("run.settings stall timeout must be 5m-24h")
				}
				next.StallTimeout = d
			}
			if p.Paused != nil {
				next.Paused = *p.Paused
			}
			if err := c.store.Runs().UpdateSettings(ctx, targetRun, next); err != nil {
				return "", err
			}
			words := cmp.Or(strings.TrimSpace(p.Words), "owner updated run settings")
			_, err = c.store.Decisions().Append(ctx, store.Decision{ID: c.nextID("settings"), RunID: targetRun, Category: store.DecisionJudgment, Actor: ActorConcierge, Tool: memorychat.ToolRunSettings, Reason: words, Result: fmt.Sprintf("concurrency_cap=%d stall_timeout=%s paused=%t", next.ConcurrencyCap, next.StallTimeout, next.Paused), Vetted: true})
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("run settings updated: concurrency_cap=%d stall_timeout=%s paused=%t", next.ConcurrencyCap, next.StallTimeout, next.Paused), nil
		})

	model.AddTool[ConciergeStatusParams](reg, ToolConciergeStatus,
		"Read structured run status, backlog, lanes, sessions, terminals, live terminal agent state, live GitHub PR state, active directives, and metrics for held runs",
		func(ctx context.Context, p ConciergeStatusParams) (string, error) {
			if runID := strings.TrimSpace(p.RunID); runID != "" {
				return c.formatRunStatusReport(ctx, runID)
			}
			return c.FormatStatusReport(ctx)
		},
	)

	if c.Swarms != nil || c.SwarmsFilter != nil {
		model.AddTool[SwarmsParams](reg, ToolSwarms,
			"List swarms on this machine (or filter by run): orca runs and host swarms, recent-first, each with its goal, lanes by status and phase, last activity and orchestrator",
			func(ctx context.Context, p SwarmsParams) (string, error) {
				var list chat.SwarmList
				switch {
				case c.SwarmsFilter != nil:
					list = c.SwarmsFilter(ctx, p.Run)
				case c.Swarms != nil:
					list = c.Swarms(ctx).Filter(p.Run)
					if list.Live == nil || strings.TrimSpace(p.Run) != "" {
						probe := c.hostProbe()
						if probe.TerminalState != nil || probe.PR != nil || len(probe.Off) > 0 {
							list.Probe(ctx, probe)
						}
					}
				}
				return clipText(list.Render(c.clock.Now(), true), swarmsReadBytes), nil
			},
		)
	}

	model.AddTool[ConciergeLaneShowParams](reg, ToolConciergeLaneShow,
		"Show detailed state, rounds, terminals, PR/CI, and recent decisions for a lane",
		func(ctx context.Context, p ConciergeLaneShowParams) (string, error) {
			return c.handleLaneShow(ctx, p.RunID, p.LaneID, quote)
		},
	)

	model.AddTool[ConciergeDecisionsParams](reg, ToolConciergeDecisions,
		"List decisions from the append-only DecisionStore since a sequence number",
		func(ctx context.Context, p ConciergeDecisionsParams) (string, error) {
			return c.handleDecisionsSince(ctx, p, quote)
		},
	)

	model.AddTool[ConciergePRCIParams](reg, ToolConciergePRCI,
		"Read PR metadata, review approval status, and CI check status for a lane",
		func(ctx context.Context, p ConciergePRCIParams) (string, error) {
			return c.handlePRCI(ctx, p.RunID, p.LaneID, quote)
		},
	)

	model.AddTool[DirectiveSubmitParams](reg, ToolDirectiveSubmit,
		"Submit a typed owner directive (standing, instruction, recipe_edit, plan_import) for the Controller/Dispatcher/Planner",
		func(ctx context.Context, p DirectiveSubmitParams) (string, error) {
			r, out, err := c.handleDirectiveSubmit(ctx, threadID, turnID, quote, p)
			if err == nil && start != nil {
				start.receipts = append(start.receipts, r)
			}
			return out, err
		},
	)
	model.AddTool[MemorySubmitParams](reg, ToolMemorySubmit, "Save an owner correction as memory", func(ctx context.Context, p MemorySubmitParams) (string, error) {
		targetRun := c.defaultTargetRun(ctx, quote)
		echo, err := c.memoryEditor.Submit(ctx, c.store, targetRun, c.nextID("memory"), quote, p)
		if err == nil {
			*memoryEcho = echo
		}
		return echo, err
	})
	model.AddTool[MemoryUndoParams](reg, ToolMemoryUndo, "Undo last memory change", func(ctx context.Context, _ MemoryUndoParams) (string, error) {
		echo, err := c.memoryEditor.Undo(ctx, c.store)
		if err == nil {
			*memoryEcho = echo
		}
		return echo, err
	})
	model.AddTool[MemoryReadParams](reg, ToolMemoryRead, "Read memory body", func(ctx context.Context, p MemoryReadParams) (string, error) {
		m, e := c.store.Memories().Get(ctx, "", p.ID)
		if e != nil {
			return "", e
		}
		if m.RunID != "" && m.RunID != c.runID && !slices.Contains(c.heldRunIDs(ctx), m.RunID) {
			return "", store.ErrNotFound
		}
		return m.Body, nil
	})

	return reg
}

type MemorySubmitParams = memorychat.MemorySubmitParams
type MemoryUndoParams = memorychat.MemoryUndoParams
type MemoryReadParams struct {
	ID string `json:"id"`
}

// FormatStatusReport renders a structured status summary of held runs from
// Store state, DecisionStore metrics, and live Nebula/GitHub probes.
func (c *Concierge) FormatStatusReport(ctx context.Context) (string, error) {
	runIDs := c.heldRunIDs(ctx)
	if len(runIDs) == 0 {
		runIDs = []string{c.runID}
	}
	var reports []string
	var firstErr error
	for _, runID := range runIDs {
		rep, err := c.formatRunStatusReport(ctx, runID)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		reports = append(reports, rep)
	}
	if len(reports) == 0 {
		return "", firstErr
	}
	return strings.Join(reports, "\n\n"), nil
}

type heldLaneProbeResult struct {
	agent        string
	agentErr     string
	prErr        string
	pr           chat.PRStatus
	agentChecked bool
	prChecked    bool
}

func (c *Concierge) formatRunStatusReport(ctx context.Context, runID string) (string, error) {
	run, err := c.store.Runs().Get(ctx, runID)
	if err != nil {
		return "", err
	}
	backlog, err := c.store.Backlog().List(ctx, store.BacklogFilter{RunID: runID})
	if err != nil {
		return "", err
	}
	lanes, err := c.store.Lanes().List(ctx, store.LaneFilter{RunID: runID})
	if err != nil {
		return "", err
	}
	dirs, err := c.store.Directives().List(ctx, store.DirectiveFilter{RunID: runID, ActiveOnly: true})
	if err != nil {
		return "", err
	}
	metrics, err := ComputeMetrics(ctx, c.store, c.clock, runID)
	if err != nil {
		return "", err
	}
	settings, err := c.store.Runs().GetSettings(ctx, runID)
	if err != nil {
		return "", err
	}

	var hostRun adopt.Run
	hostLanes := map[string]adopt.Lane{}
	if c.HostRun != nil {
		if hr, hrErr := c.HostRun(runID); hrErr == nil {
			hostRun = hr
			for i := range hr.Lanes {
				hostLanes[hr.Lanes[i].ID] = hr.Lanes[i]
			}
		}
	}
	repo := cmp.Or(run.Repository, hostRun.Repo)
	runDir := hostRun.Dir
	probe := c.hostProbe()
	probed := c.probeHeldLanes(ctx, probe, repo, runDir, lanes, hostLanes)

	counts := make(map[store.BacklogStatus]int)
	for i := range backlog {
		counts[backlog[i].Status]++
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Run: %s\n", run.ID)
	fmt.Fprintf(&b, "Objective: %s\n", cmp.Or(run.Objective, hostRun.Goal, "(none)"))
	if repo != "" {
		fmt.Fprintf(&b, "Repository: %s\n", repo)
	}
	fmt.Fprintf(&b, "Settings: concurrency_cap=%d stall_timeout=%s paused=%t\n", settings.ConcurrencyCap, settings.StallTimeout, settings.Paused)
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

	fmt.Fprintf(&b, "\nLanes (%s):\n", formatStoreLaneCounts(lanes))
	var runningNow, unknownNow []string
	for i := range lanes {
		ln := lanes[i]
		hl := hostLanes[ln.ID]
		live := probed[ln.ID]
		rec := cmp.Or(ln.RecipeName, recipe.DefaultName)
		if ln.RecipeVersion > 0 {
			rec = fmt.Sprintf("%s@v%d", rec, ln.RecipeVersion)
		}
		sessionID := cmp.Or(ln.SessionID, hl.Session)
		terminalID := cmp.Or(ln.TerminalID, hl.Terminal)
		title := cmp.Or(ln.Title, hl.Title)
		line := fmt.Sprintf("- %s: item=%s status=%s phase=%s round=%d session=%s terminal=%s recipe=%s pr=#%d head=%s approval=%s merge=%s checks=%s",
			ln.ID,
			cmp.Or(ln.ItemID, "-"),
			ln.Status,
			cmp.Or(ln.Phase, "-"),
			ln.Round,
			cmp.Or(sessionID, "-"),
			cmp.Or(terminalID, "-"),
			rec,
			ln.PR,
			cmp.Or(ln.PRHead, "-"),
			cmp.Or(ln.ApprovalSHA, "-"),
			cmp.Or(ln.MergeSHA, "-"),
			cmp.Or(ln.Checks, "-"),
		)
		if title != "" {
			line += " " + title
		}
		b.WriteString(line + "\n")

		nowParts := c.laneNowParts(ctx, runID, &ln, hl, terminalID, sessionID, live)
		nextStep := c.laneNextStep(ctx, run, &ln, dirs, hl)
		fmt.Fprintf(&b, "    now: %s\n", strings.Join(nowParts, "; "))
		fmt.Fprintf(&b, "    next: %s\n", nextStep)
		if ln.PR > 0 {
			switch {
			case live.prChecked && live.prErr == "":
				fmt.Fprintf(&b, "    pr #%d (GitHub): %s\n", ln.PR, live.pr)
			case live.prChecked:
				fmt.Fprintf(&b, "    pr #%d (GitHub): unreadable: %s\n", ln.PR, live.prErr)
			default:
				fmt.Fprintf(&b, "    pr #%d: store checks=%s head=%s approval=%s merge=%s (GitHub state not checked)\n",
					ln.PR, cmp.Or(ln.Checks, "-"), cmp.Or(ln.PRHead, "-"), cmp.Or(ln.ApprovalSHA, "-"), cmp.Or(ln.MergeSHA, "-"))
			}
		}
		if !ln.Status.Terminal() {
			switch {
			case live.agentChecked && live.agentErr == "" && !isIdleTerminalState(live.agent):
				runningNow = append(runningNow, fmt.Sprintf("%s (%s, agent %s)", ln.ID, cmp.Or(ln.Phase, "-"), live.agent))
			case live.agentChecked && live.agentErr == "":
				// Checked idle.
			default:
				unknownNow = append(unknownNow, fmt.Sprintf("%s (%s)", ln.ID, cmp.Or(ln.Phase, string(ln.Status))))
			}
		}
	}
	if len(lanes) > 0 {
		switch {
		case len(runningNow) > 0:
			fmt.Fprintf(&b, "Running now: %s\n", strings.Join(runningNow, ", "))
		case len(unknownNow) > 0:
			fmt.Fprintf(&b, "Running now: %s\n", strings.Join(unknownNow, ", "))
		default:
			b.WriteString("Running now: nothing (every lane is finished or its terminal checked idle)\n")
		}
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

func (c *Concierge) probeHeldLanes(
	ctx context.Context,
	probe chat.HostProbe,
	repo, runDir string,
	lanes []store.Lane,
	hostLanes map[string]adopt.Lane,
) map[string]heldLaneProbeResult {
	out := make(map[string]heldLaneProbeResult, len(lanes))
	if probe.TerminalState == nil && probe.PR == nil {
		return out
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range lanes {
		ln := lanes[i]
		hl := hostLanes[ln.ID]
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var res heldLaneProbeResult
			terminalID := cmp.Or(ln.TerminalID, hl.Terminal)
			if probe.TerminalState != nil && terminalID != "" && !ln.Status.Terminal() {
				res.agentChecked = true
				st, err := probe.TerminalState(ctx, terminalID)
				if err != nil {
					res.agentErr = err.Error()
				} else {
					res.agent = cmp.Or(st, "unknown")
				}
			}
			if probe.PR != nil && ln.PR > 0 {
				res.prChecked = true
				pr, err := probe.PR(ctx, repo, runDir, strconv.Itoa(ln.PR))
				if err != nil {
					res.prErr = err.Error()
				} else {
					res.pr = pr
				}
			}
			mu.Lock()
			out[ln.ID] = res
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

func (c *Concierge) laneNowParts(
	ctx context.Context,
	runID string,
	ln *store.Lane,
	hl adopt.Lane,
	terminalID, sessionID string,
	live heldLaneProbeResult,
) []string {
	parts := []string{fmt.Sprintf("%s round %d", cmp.Or(ln.Phase, "-"), max(ln.Round, 1))}
	if sessionID != "" {
		parts = append(parts, "session="+sessionID)
	}
	switch {
	case terminalID == "":
		parts = append(parts, "no terminal recorded")
	case ln.Status.Terminal():
		parts = append(parts, fmt.Sprintf("terminal %s (lane %s, not probed)", terminalID, ln.Status))
	case live.agentChecked && live.agentErr == "":
		parts = append(parts, fmt.Sprintf("terminal %s agent=%s", terminalID, live.agent))
	case live.agentChecked:
		parts = append(parts, fmt.Sprintf("terminal %s agent state unreadable: %s", terminalID, live.agentErr))
	default:
		parts = append(parts, fmt.Sprintf("terminal %s agent state not checked", terminalID))
	}
	if hl.TerminalState != "" {
		parts = append(parts, "ledger terminal_state="+hl.TerminalState)
	}
	lastEvent := hl.LastEvent
	if lastEvent == "" && c.store != nil {
		if evs, err := c.store.Events().List(ctx, store.EventFilter{RunID: runID, LaneID: ln.ID}); err == nil && len(evs) > 0 {
			lastEvent = evs[len(evs)-1].Summary
		}
	}
	if lastEvent != "" {
		parts = append(parts, "last event: "+lastEvent)
	}
	if hl.Report != "" {
		parts = append(parts, fmt.Sprintf("latest report %s: %s", filepath.Base(hl.ReportPath), hl.Report))
	}
	return parts
}

func (c *Concierge) laneNextStep(
	ctx context.Context,
	run *store.Run,
	ln *store.Lane,
	dirs []store.Directive,
	hl adopt.Lane,
) string {
	if ln.Status.Terminal() {
		return fmt.Sprintf("nothing (lane %s)", ln.Status)
	}
	for i := range dirs {
		d := &dirs[i]
		if d.Scope == ln.ID && strings.HasPrefix(d.Ref, "owner_ask:") {
			var ask controller.OwnerAsk
			if err := json.Unmarshal([]byte(d.Text), &ask); err == nil && ask.Question != "" {
				return "awaiting owner answer: " + ask.Question
			}
			return "awaiting owner answer"
		}
	}
	if hl.Next != "" {
		return hl.Next
	}
	rec := recipe.Default()
	if c.store != nil && run != nil {
		recName := cmp.Or(ln.RecipeName, run.DefaultRecipe, recipe.DefaultName)
		if loaded, err := c.store.Recipes().Get(ctx, run.ID, recName, ln.RecipeVersion); err == nil && loaded != nil {
			rec = *loaded
		} else if loaded, err := c.store.Recipes().Get(ctx, run.ID, recName, 0); err == nil && loaded != nil {
			rec = *loaded
		}
	}
	if ln.Phase == "" && len(rec.Phases) > 0 {
		return "start phase: " + rec.Phases[0].Name
	}
	for i := range rec.Transitions {
		tr := &rec.Transitions[i]
		if tr.From == ln.Phase && tr.On.Verdict == recipe.OutcomeDone {
			if tr.To == recipe.TargetComplete {
				if rec.MergePolicy.Mode == recipe.MergeModeManual {
					return fmt.Sprintf("manual merge after %s", cmp.Or(ln.Phase, "review"))
				}
				return fmt.Sprintf("complete after %s", cmp.Or(ln.Phase, "current phase"))
			}
			if tr.To != "" {
				return "next phase: " + tr.To
			}
		}
	}
	for i := range rec.Phases {
		if rec.Phases[i].Name == ln.Phase {
			if i+1 < len(rec.Phases) {
				return "next phase: " + rec.Phases[i+1].Name
			}
			if rec.MergePolicy.Mode == recipe.MergeModeManual {
				return fmt.Sprintf("manual merge after %s", ln.Phase)
			}
			return fmt.Sprintf("complete after %s", ln.Phase)
		}
	}
	return "continue " + cmp.Or(ln.Phase, "work")
}

func formatStoreLaneCounts(lanes []store.Lane) string {
	if len(lanes) == 0 {
		return "0"
	}
	statuses, phases := map[string]int{}, map[string]int{}
	for i := range lanes {
		statuses[cmp.Or(string(lanes[i].Status), "-")]++
		phases[cmp.Or(lanes[i].Phase, "-")]++
	}
	return fmt.Sprintf("%d (%s; %s)", len(lanes), sortedMapCounts(statuses), sortedMapCounts(phases))
}

func sortedMapCounts(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s:%d", k, counts[k])
	}
	return strings.Join(parts, ", ")
}

func isIdleTerminalState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "idle", "exited", "stopped", "released", "ended", "done", "completed", "finished", "sleeping":
		return true
	}
	return false
}

func (c *Concierge) findHeldLane(ctx context.Context, runHint, laneID, quote string) (string, *store.Lane, error) {
	laneID = strings.TrimSpace(laneID)
	runHint = strings.TrimSpace(runHint)

	var candidates []string
	addCandidate := func(id string) {
		id = strings.TrimSpace(id)
		if id != "" && !slices.Contains(candidates, id) {
			candidates = append(candidates, id)
		}
	}
	held := c.heldRunIDs(ctx)
	var allRuns []string
	allRuns = append(allRuns, held...)
	if c.store != nil {
		if runs, err := c.store.Runs().List(ctx); err == nil {
			for i := range runs {
				if runs[i].ID != "" && !slices.Contains(allRuns, runs[i].ID) {
					allRuns = append(allRuns, runs[i].ID)
				}
			}
		}
	}
	resolveRunName := func(token string) string {
		tLower := strings.ToLower(strings.TrimSpace(token))
		if tLower == "" {
			return ""
		}
		for _, id := range allRuns {
			idLower := strings.ToLower(id)
			if idLower == tLower || runSlug(idLower) == tLower {
				return id
			}
		}
		return token
	}
	if runHint != "" {
		addCandidate(resolveRunName(runHint))
	}
	if quote != "" {
		for _, tok := range matchedRunTokens(strings.ToLower(quote), allRuns) {
			addCandidate(resolveRunName(tok))
		}
	}
	for _, runID := range held {
		if runID != frontDeskRunID {
			addCandidate(runID)
		}
	}
	for _, runID := range allRuns {
		if runID != frontDeskRunID {
			addCandidate(runID)
		}
	}
	addCandidate(c.runID)

	for _, runID := range candidates {
		if c.store != nil {
			if ln, err := c.store.Lanes().Get(ctx, runID, laneID); err == nil {
				return runID, ln, nil
			}
		}
		if c.HostRun != nil {
			if hr, err := c.HostRun(runID); err == nil {
				for i := range hr.Lanes {
					hl := &hr.Lanes[i]
					if hl.ID == laneID {
						prNum, _ := strconv.Atoi(hl.PR)
						return runID, &store.Lane{
							ID:         hl.ID,
							RunID:      runID,
							ItemID:     hl.ID,
							Title:      hl.Title,
							Branch:     hl.Branch,
							Phase:      hl.Phase,
							Status:     store.LaneStatus(hl.Status),
							SessionID:  hl.Session,
							TerminalID: hl.Terminal,
							PR:         prNum,
							Round:      hl.Round,
							Attempt:    1,
						}, nil
					}
				}
			}
		}
	}
	return "", nil, store.ErrNotFound
}

func (c *Concierge) handleLaneShow(ctx context.Context, runHint, laneID, quote string) (string, error) {
	runID, ln, err := c.findHeldLane(ctx, runHint, laneID, quote)
	if err != nil {
		return "", err
	}
	decs, err := c.store.Decisions().List(ctx, store.DecisionFilter{
		RunID:  runID,
		LaneID: ln.ID,
		Limit:  10,
	})
	if err != nil {
		return "", err
	}
	run, _ := c.store.Runs().Get(ctx, runID)
	dirs, _ := c.store.Directives().List(ctx, store.DirectiveFilter{RunID: runID, ActiveOnly: true})
	var hl adopt.Lane
	var repo, runDir string
	if run != nil {
		repo = run.Repository
	}
	if c.HostRun != nil {
		if hr, hrErr := c.HostRun(runID); hrErr == nil {
			repo = cmp.Or(repo, hr.Repo)
			runDir = hr.Dir
			for i := range hr.Lanes {
				if hr.Lanes[i].ID == ln.ID {
					hl = hr.Lanes[i]
					break
				}
			}
		}
	}
	probed := c.probeHeldLanes(ctx, c.hostProbe(), repo, runDir, []store.Lane{*ln}, map[string]adopt.Lane{ln.ID: hl})
	live := probed[ln.ID]
	sessionID := cmp.Or(ln.SessionID, hl.Session)
	terminalID := cmp.Or(ln.TerminalID, hl.Terminal)

	var b strings.Builder
	fmt.Fprintf(&b, "Lane %s (run=%s item=%s status=%s phase=%s round=%d attempt=%d session=%s terminal=%s recipe=%s@v%d)\n",
		ln.ID, runID, cmp.Or(ln.ItemID, "-"), ln.Status, cmp.Or(ln.Phase, "-"), ln.Round, ln.Attempt,
		cmp.Or(sessionID, "-"), cmp.Or(terminalID, "-"),
		cmp.Or(ln.RecipeName, recipe.DefaultName), max(ln.RecipeVersion, 1))
	fmt.Fprintf(&b, "Now: %s\n", strings.Join(c.laneNowParts(ctx, runID, ln, hl, terminalID, sessionID, live), "; "))
	fmt.Fprintf(&b, "Next: %s\n", c.laneNextStep(ctx, run, ln, dirs, hl))
	fmt.Fprintf(&b, "PR: #%d branch=%s head=%s approval=%s merge=%s checks=%s\n",
		ln.PR, cmp.Or(ln.Branch, "-"), cmp.Or(ln.PRHead, "-"), cmp.Or(ln.ApprovalSHA, "-"),
		cmp.Or(ln.MergeSHA, "-"), cmp.Or(ln.Checks, "-"))
	if ln.PR > 0 && live.prChecked {
		if live.prErr == "" {
			fmt.Fprintf(&b, "GitHub PR #%d: %s\n", ln.PR, live.pr)
		} else {
			fmt.Fprintf(&b, "GitHub PR #%d: unreadable: %s\n", ln.PR, live.prErr)
		}
	}
	fmt.Fprintf(&b, "Decisions (%d):\n", len(decs))
	for i := range decs {
		d := decs[i]
		fmt.Fprintf(&b, "- [seq=%d %s %s] %s: %s\n", d.Seq, d.Category, d.Actor, d.Tool, d.Result)
	}
	return b.String(), nil
}

func (c *Concierge) handleDecisionsSince(ctx context.Context, p ConciergeDecisionsParams, quote string) (string, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = 25
	}
	runID := strings.TrimSpace(p.RunID)
	if laneID := strings.TrimSpace(p.LaneID); laneID != "" {
		if rID, _, err := c.findHeldLane(ctx, runID, laneID, quote); err == nil {
			runID = rID
		}
	}
	if runID == "" {
		runID = c.defaultTargetRun(ctx, quote)
	}
	filter := store.DecisionFilter{
		RunID:    runID,
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

func (c *Concierge) handlePRCI(ctx context.Context, runHint, laneID, quote string) (string, error) {
	runID, ln, err := c.findHeldLane(ctx, runHint, laneID, quote)
	if err != nil {
		return "", err
	}
	evs, err := c.store.Events().List(ctx, store.EventFilter{
		RunID:  runID,
		LaneID: ln.ID,
		Kinds:  []store.EventKind{store.EventCI},
	})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Lane %s PR #%d head=%s approval=%s stale=%t merge=%s checks=%s ci_events=%d\n",
		ln.ID, ln.PR, cmp.Or(ln.PRHead, "-"), cmp.Or(ln.ApprovalSHA, "-"), ln.ApprovalStale(),
		cmp.Or(ln.MergeSHA, "-"), cmp.Or(ln.Checks, "-"), len(evs))
	if ln.PR > 0 {
		var repo, runDir string
		if run, rErr := c.store.Runs().Get(ctx, runID); rErr == nil {
			repo = run.Repository
		}
		if c.HostRun != nil {
			if hr, hrErr := c.HostRun(runID); hrErr == nil {
				repo = cmp.Or(repo, hr.Repo)
				runDir = hr.Dir
			}
		}
		probed := c.probeHeldLanes(ctx, c.hostProbe(), repo, runDir, []store.Lane{*ln}, nil)
		if live := probed[ln.ID]; live.prChecked {
			if live.prErr == "" {
				fmt.Fprintf(&b, "GitHub PR #%d: %s\n", ln.PR, live.pr)
			} else {
				fmt.Fprintf(&b, "GitHub PR #%d: unreadable: %s\n", ln.PR, live.prErr)
			}
		}
	}
	for i := max(len(evs)-5, 0); i < len(evs); i++ {
		ev := evs[i]
		fmt.Fprintf(&b, "- [seq=%d %s] %s %s\n", ev.Seq, ev.ID, ev.Summary, string(ev.Payload))
	}
	return b.String(), nil
}

func (c *Concierge) handleDirectiveSubmit(
	ctx context.Context,
	threadID, turnID, quote string,
	p DirectiveSubmitParams,
) (directiveReceipt, string, error) {
	text := strings.TrimSpace(p.Text)
	if text == "" {
		return directiveReceipt{}, "", fmt.Errorf("%w: directive text is required", store.ErrInvalidArgument)
	}
	kind := store.DirectiveKind(strings.ToLower(strings.TrimSpace(p.Kind)))
	switch kind {
	case store.DirectiveStanding, store.DirectiveInstruction, store.DirectiveRecipeEdit, store.DirectivePlanImport:
	case "":
		kind = store.DirectiveInstruction
	default:
		return directiveReceipt{}, "", fmt.Errorf("%w: invalid directive kind %q", store.ErrInvalidArgument, p.Kind)
	}
	// The directive goes to the run it is about, which at the front desk is
	// an adopted run, not the chat's own.
	var runID string
	combinedText := strings.TrimSpace(quote + "\n" + p.Scope + "\n" + text)
	if c.OwnerRun != nil {
		var err error
		if runID, err = c.OwnerRun(ctx, p.RunID, combinedText); err != nil {
			return directiveReceipt{}, "", err
		}
	} else if named := strings.TrimSpace(p.RunID); named != "" {
		held := c.heldRunIDs(ctx)
		if named != c.runID && !slices.Contains(held, named) {
			return directiveReceipt{}, "", fmt.Errorf("run %s is not this chat's run %s", named, c.runID)
		}
		runID = named
	} else {
		runID = c.defaultTargetRun(ctx, combinedText)
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
			return directiveReceipt{}, "", fmt.Errorf("%w: invalid expiry %q", store.ErrInvalidArgument, p.Expiry)
		}
	}

	dirID := c.nextID("dir-concierge")
	dir := store.Directive{
		ID:        dirID,
		RunID:     runID,
		Kind:      kind,
		Source:    store.DirectiveSourceConcierge,
		Scope:     strings.TrimSpace(p.Scope),
		Ref:       strings.TrimSpace(p.Ref),
		Text:      text,
		ExpiresAt: expiresAt,
		Status:    store.DirectiveActive,
	}
	if err := c.store.Directives().Add(ctx, dir); err != nil {
		return directiveReceipt{}, "", err
	}
	echo := "directive is time-limited"
	if expiresAt == nil {
		var err error
		memoryScope := dir.Scope
		if memoryScope != "owner" && memoryScope != "repo" && memoryScope != "run" {
			memoryScope = "run"
		}
		echo, err = c.memoryEditor.Submit(ctx, c.store, runID, dir.ID, quote, MemorySubmitParams{Action: "create", ID: dir.ID, Name: "Directive: " + text, Description: text, Body: text, Scope: memoryScope})
		if err != nil {
			_ = c.store.Directives().UpdateStatus(context.WithoutCancel(ctx), runID, dir.ID, store.DirectiveRevoked)
			return directiveReceipt{}, "", err
		}
	}

	payloadBytes, _ := json.Marshal(dir)
	if _, err := c.store.Events().Append(ctx, store.Event{
		RunID:   runID,
		Kind:    store.EventOwnerMessage,
		Class:   store.EventClassJudgment,
		LaneID:  dir.Scope,
		Source:  ActorConcierge,
		Summary: fmt.Sprintf("owner directive (%s): %s", dir.Kind, dir.Text),
		Payload: payloadBytes,
		Urgent:  true,
	}); err != nil {
		return directiveReceipt{}, "", err
	}
	if c.Wake != nil {
		c.Wake(runID)
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
		Reason:   fmt.Sprintf("submitted %s directive on run %s", dir.Kind, runID),
		Result:   dir.ID,
		Args:     rawArgs,
		Vetted:   true,
	})

	return directiveReceipt{id: dir.ID, run: runID}, fmt.Sprintf("Submitted directive %s on run %s (kind=%s); %s", dir.ID, runID, dir.Kind, echo), nil
}
