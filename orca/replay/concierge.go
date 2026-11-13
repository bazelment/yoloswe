package replay

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/store"
)

const ConciergeSystemPromptVersion = "concierge-v1"

const ConciergeSystemPrompt = `# Nebula Orchestrator Concierge (` + ConciergeSystemPromptVersion + `)

You are the **Concierge** for a Nebula swarm run — a fast, read-mostly conversational agent sitting in front of the Store so the owner gets immediate, factually accurate answers without blocking the Dispatcher.

## Rules
1. **Always query the Store first**: The initial prompt gives only the Run ID, timestamp, and Owner Question. You **must** call ` + "`status`" + ` (and ` + "`lane.show`" + `, ` + "`pr.status`" + `, ` + "`ci.status`" + `, or ` + "`decisions.since`" + ` as needed) to read the Store state before answering.
2. **Strict Store grounding**: Every lane ID, status, phase, round, PR number, head SHA, CI check state, blocker, backlog item, and decision you mention **must** match the Store tool output exactly. Never invent or guess PR numbers, lane states, or blockers that are not in the Store.
3. **Cite concrete facts**: When answering questions like "where are we?", "how is lane X doing?", or "why is lane Y blocked/idle?", explicitly cite the relevant lane IDs, their current status and phase/round, PR numbers (e.g. ` + "`#5127`" + `), CI check state, backlog status/blockers, and recent Dispatcher decisions/reasons.
4. **Tools available**:
   - ` + "`status`" + `: Query full run status (lanes by phase, backlog items, active directives, pending owner asks).
   - ` + "`lane.show`" + `: Inspect a specific lane's details, PR/CI status, and latest phase report.
   - ` + "`decisions.since`" + `: List Dispatcher/Controller decisions and reasons.
   - ` + "`pr.status`" + ` / ` + "`ci.status`" + `: Inspect PR approval and CI check state for a lane.
   - ` + "`directive.submit`" + `: Record an owner directive when the owner explicitly issues a command or override.
5. Keep answers concise, direct, and grounded in the exact Store state at the current timestamp.
`

// ConciergeAnswer captures the Concierge's response and usage for a single status question.
type ConciergeAnswer struct {
	Answer    string
	ToolCalls []string
	Usage     model.Usage
	TurnCount int
}

// ConciergeRunner evaluates an owner status question against a seeded Store.
type ConciergeRunner interface {
	Answer(ctx context.Context, st store.Store, drv lane.LaneDriver, clk clock.Clock, runID, question string) (ConciergeAnswer, error)
}

// StoreConcierge is the default ConciergeRunner backed by store.Store and model.ModelClient.
type StoreConcierge struct {
	Client       model.ModelClient
	Model        string
	SystemPrompt string
	MaxTurns     int
	MaxTokens    int
	dirSeq       atomic.Int64
}

const DefaultEvalModel = "claude-haiku-4-5-20251001"

func NewStoreConcierge(client model.ModelClient) *StoreConcierge {
	return &StoreConcierge{
		Client:       NewWireToolClient(client),
		Model:        DefaultEvalModel,
		SystemPrompt: ConciergeSystemPrompt,
		MaxTurns:     4,
		MaxTokens:    1024,
	}
}

type statusToolParams struct{}

type laneShowParams struct {
	LaneID string `json:"lane_id" jsonschema:"description=Lane ID to inspect"`
}

type decisionsSinceParams struct {
	Since  string `json:"since,omitempty" jsonschema:"description=Minimum decision sequence integer or RFC3339 timestamp"`
	LaneID string `json:"lane_id,omitempty" jsonschema:"description=Optional lane ID filter"`
	Limit  int    `json:"limit,omitempty" jsonschema:"description=Maximum number of recent decisions to return (default 20)"`
}

type directiveSubmitParams struct {
	Kind  string `json:"kind" jsonschema:"enum=standing,enum=instruction,enum=note,description=Directive kind: standing, instruction, or note"`
	Text  string `json:"text" jsonschema:"description=Directive text"`
	Scope string `json:"scope,omitempty" jsonschema:"description=Optional lane or scope identifier"`
}

const (
	toolConciergeStatus          = "status"
	toolConciergeLaneShow        = "lane.show"
	toolConciergeDecisions       = "decisions.since"
	toolConciergePRStatus        = "pr.status"
	toolConciergeCIStatus        = "ci.status"
	toolConciergeDirectiveSubmit = "directive.submit"
)

var conciergeToolDefs = []model.ToolDefinition{
	{Name: toolConciergeStatus, Description: "Query run status: lanes by phase, backlog items, directives, and pending asks", InputSchema: model.SchemaFor[statusToolParams]()},
	{Name: toolConciergeLaneShow, Description: "Show detailed state, PR/CI status, and last phase report for a lane", InputSchema: model.SchemaFor[laneShowParams]()},
	{Name: toolConciergeDecisions, Description: "Query recent Dispatcher and Controller decisions and reasons", InputSchema: model.SchemaFor[decisionsSinceParams]()},
	{Name: toolConciergePRStatus, Description: "Show PR number, head SHA, approval SHA, and merge status for a lane", InputSchema: model.SchemaFor[laneShowParams]()},
	{Name: toolConciergeCIStatus, Description: "Show CI check state and recent CI events for a lane", InputSchema: model.SchemaFor[laneShowParams]()},
	{Name: toolConciergeDirectiveSubmit, Description: "Submit an owner directive (standing, instruction, note) to the Store", InputSchema: model.SchemaFor[directiveSubmitParams]()},
}

// Answer runs the Concierge over the seeded Store to answer question for runID.
func (c *StoreConcierge) Answer(
	ctx context.Context,
	st store.Store,
	drv lane.LaneDriver,
	clk clock.Clock,
	runID, question string,
) (ConciergeAnswer, error) {
	if c == nil || c.Client == nil {
		return ConciergeAnswer{}, errors.New("orca/replay: concierge model client is required")
	}
	if clk == nil {
		clk = clock.NewRealClock()
	}

	if _, err := st.Runs().Get(ctx, runID); err != nil {
		return ConciergeAnswer{}, err
	}

	var toolCalls []string
	reg := model.NewToolRegistry()
	for _, def := range conciergeToolDefs {
		reg.Register(def, func(callCtx context.Context, raw json.RawMessage) (string, error) {
			toolCalls = append(toolCalls, def.Name)
			return c.executeTool(callCtx, st, drv, clk, runID, def.Name, raw)
		})
	}

	prompt := fmt.Sprintf("Run: %s\nNow: %s\n\n## Owner Question\n%s",
		runID,
		clk.Now().UTC().Format(time.RFC3339),
		strings.TrimSpace(question),
	)

	req := model.Request{
		Model:     cmp.Or(strings.TrimSpace(c.Model), DefaultEvalModel),
		System:    cmp.Or(c.SystemPrompt, ConciergeSystemPrompt),
		Messages:  []model.Message{model.NewUserMessage(prompt)},
		Tools:     reg.Definitions(),
		MaxTokens: cmp.Or(c.MaxTokens, 1024),
	}

	maxTurns := cmp.Or(c.MaxTurns, 4)
	res, err := model.RunToolLoop(ctx, c.Client, req, reg, model.WithMaxTurns(maxTurns))
	if err != nil {
		return ConciergeAnswer{}, err
	}
	return ConciergeAnswer{
		Answer:    strings.TrimSpace(res.Response.Text()),
		ToolCalls: toolCalls,
		Usage:     res.Usage,
		TurnCount: res.TurnCount,
	}, nil
}

func buildConciergeStatusView(
	ctx context.Context,
	st store.Store,
	drv lane.LaneDriver,
	clk clock.Clock,
	runID string,
) (string, error) {
	var b strings.Builder
	run, err := st.Runs().Get(ctx, runID)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "Run: %s\nObjective: %s\n", run.ID, run.Objective)

	dirs, err := st.Directives().List(ctx, store.DirectiveFilter{RunID: runID, ActiveOnly: true})
	if err != nil {
		return "", err
	}
	if len(dirs) > 0 {
		b.WriteString("\nActive Directives & Pending Asks:\n")
		for i := range dirs {
			d := &dirs[i]
			fmt.Fprintf(&b, "- [%s/%s] id=%s ref=%s: %s\n", d.Kind, d.Source, d.ID, d.Ref, d.Text)
		}
	}

	items, err := st.Backlog().List(ctx, store.BacklogFilter{RunID: runID})
	if err != nil {
		return "", err
	}
	if len(items) > 0 {
		b.WriteString("\nBacklog Items:\n")
		for i := range items {
			item := &items[i]
			fmt.Fprintf(&b, "- %s [%s] status=%s lane=%s title=%q",
				item.ID, item.Priority, item.Status, cmp.Or(item.LaneID, "-"), item.Title)
			if len(item.DependsOn) > 0 {
				fmt.Fprintf(&b, " depends_on=%v", item.DependsOn)
			}
			if item.Description != "" {
				fmt.Fprintf(&b, " desc=%q", item.Description)
			}
			b.WriteByte('\n')
		}
	}

	lanes, err := st.Lanes().List(ctx, store.LaneFilter{RunID: runID})
	if err != nil {
		return "", err
	}
	if len(lanes) > 0 {
		b.WriteString("\nLanes:\n")
		for i := range lanes {
			ln := &lanes[i]
			age := clk.Now().Sub(ln.LastEventAt).Round(time.Minute)
			fmt.Fprintf(&b, "- %s: status=%s phase=%s round=%d item=%s pr=#%d pr_head=%s approval_sha=%s checks=%s merge_sha=%s idle=%s title=%q\n",
				ln.ID,
				ln.Status,
				cmp.Or(ln.Phase, "-"),
				ln.Round,
				cmp.Or(ln.ItemID, "-"),
				ln.PR,
				cmp.Or(ln.PRHead, "-"),
				cmp.Or(ln.ApprovalSHA, "-"),
				cmp.Or(ln.Checks, "-"),
				cmp.Or(ln.MergeSHA, "-"),
				age,
				ln.Title,
			)
			if repLine := readLaneReportSummary(ctx, drv, runID, *ln); repLine != "" {
				fmt.Fprintf(&b, "  %s\n", repLine)
			}
		}
	}

	decs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: runID, Limit: 20})
	if err != nil {
		return "", err
	}
	if len(decs) > 0 {
		b.WriteString("\nRecent Decisions:\n")
		for i := range decs {
			d := &decs[i]
			fmt.Fprintf(&b, "- seq=%d actor=%s tool=%s lane=%s vetted=%t reason=%q result=%q\n",
				d.Seq, d.Actor, d.Tool, cmp.Or(d.LaneID, "-"), d.Vetted, d.Reason, d.Result)
		}
	}

	evs, err := st.Events().List(ctx, store.EventFilter{RunID: runID, Limit: 20})
	if err != nil {
		return "", err
	}
	if len(evs) > 0 {
		b.WriteString("\nRecent Events:\n")
		for i := range evs {
			ev := &evs[i]
			fmt.Fprintf(&b, "- seq=%d kind=%s lane=%s summary=%q\n",
				ev.Seq, ev.Kind, cmp.Or(ev.LaneID, "-"), ev.Summary)
		}
	}
	return b.String(), nil
}

func readLaneReportSummary(ctx context.Context, drv lane.LaneDriver, runID string, ln lane.Lane) string {
	if drv == nil || ln.TerminalID == "" {
		return ""
	}
	rep, err := drv.ReadReport(ctx, runID, ln.ID, ln.TerminalID)
	if err != nil || rep == nil {
		return ""
	}
	return fmt.Sprintf("report(outcome=%s, breaks=%q, reason=%q, summary=%q)",
		rep.Outcome, rep.Breaks, rep.Reason, rep.Summary)
}

func lookupConciergeLane(ctx context.Context, st store.Store, runID string, raw json.RawMessage) (*lane.Lane, error) {
	var p laneShowParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
	}
	return st.Lanes().Get(ctx, runID, strings.TrimSpace(p.LaneID))
}

func (c *StoreConcierge) executeTool(
	ctx context.Context,
	st store.Store,
	drv lane.LaneDriver,
	clk clock.Clock,
	runID, toolName string,
	raw json.RawMessage,
) (string, error) {
	switch toolName {
	case toolConciergeStatus:
		return buildConciergeStatusView(ctx, st, drv, clk, runID)
	case toolConciergeLaneShow:
		ln, err := lookupConciergeLane(ctx, st, runID, raw)
		if err != nil {
			return "", err
		}
		out := fmt.Sprintf("lane=%s status=%s phase=%s round=%d item=%s pr=#%d pr_head=%s approval_sha=%s checks=%s merge_sha=%s title=%q",
			ln.ID, ln.Status, ln.Phase, ln.Round, ln.ItemID, ln.PR, ln.PRHead, ln.ApprovalSHA, ln.Checks, ln.MergeSHA, ln.Title)
		if repLine := readLaneReportSummary(ctx, drv, runID, *ln); repLine != "" {
			out += "\n" + repLine
		}
		return out, nil
	case toolConciergePRStatus:
		ln, err := lookupConciergeLane(ctx, st, runID, raw)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("lane=%s pr=#%d pr_head=%s approval_sha=%s approval_stale=%t merge_sha=%s status=%s title=%q",
			ln.ID, ln.PR, cmp.Or(ln.PRHead, "-"), cmp.Or(ln.ApprovalSHA, "-"), ln.ApprovalStale(), cmp.Or(ln.MergeSHA, "-"), ln.Status, ln.Title), nil
	case toolConciergeCIStatus:
		ln, err := lookupConciergeLane(ctx, st, runID, raw)
		if err != nil {
			return "", err
		}
		out := fmt.Sprintf("lane=%s pr=#%d pr_head=%s checks=%s",
			ln.ID, ln.PR, cmp.Or(ln.PRHead, "-"), cmp.Or(ln.Checks, "-"))
		evs, err := st.Events().List(ctx, store.EventFilter{RunID: runID, LaneID: ln.ID})
		if err != nil {
			return "", err
		}
		var ciEvs []store.Event
		for i := range evs {
			ev := evs[i]
			lowerKind := strings.ToLower(string(ev.Kind))
			if ev.Kind == store.EventCI || strings.HasPrefix(lowerKind, "ci") {
				ciEvs = append(ciEvs, ev)
			}
		}
		if len(ciEvs) > 10 {
			ciEvs = ciEvs[len(ciEvs)-10:]
		}
		for i := range ciEvs {
			ev := &ciEvs[i]
			out += fmt.Sprintf("\nci_event seq=%d kind=%s summary=%q", ev.Seq, ev.Kind, ev.Summary)
		}
		return out, nil
	case toolConciergeDecisions:
		var p decisionsSinceParams
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &p); err != nil {
				return "", err
			}
		}
		filter := store.DecisionFilter{
			RunID:  runID,
			LaneID: strings.TrimSpace(p.LaneID),
		}
		if s := strings.TrimSpace(p.Since); s != "" {
			if seq, err := strconv.ParseInt(s, 10, 64); err == nil {
				filter.SinceSeq = seq
			} else if ts, err := time.Parse(time.RFC3339, s); err == nil {
				filter.SinceTime = &ts
			} else {
				return "", fmt.Errorf("invalid since %q: want sequence integer or RFC3339 timestamp", p.Since)
			}
		}
		decs, err := st.Decisions().List(ctx, filter)
		if err != nil {
			return "", err
		}
		limit := cmp.Or(p.Limit, 20)
		if limit > 0 && len(decs) > limit {
			decs = decs[len(decs)-limit:]
		}
		var lines []string
		for i := range decs {
			d := &decs[i]
			lines = append(lines, fmt.Sprintf("seq=%d tool=%s lane=%s reason=%q result=%q", d.Seq, d.Tool, d.LaneID, d.Reason, d.Result))
		}
		return strings.Join(lines, "\n"), nil
	case toolConciergeDirectiveSubmit:
		var p directiveSubmitParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return "", err
		}
		kind := store.DirectiveKind(cmp.Or(strings.TrimSpace(p.Kind), string(store.DirectiveInstruction)))
		switch kind {
		case store.DirectiveStanding, store.DirectiveInstruction, store.DirectiveNote:
		default:
			return "", fmt.Errorf("invalid directive kind %q: want standing, instruction, or note", p.Kind)
		}
		seq := c.dirSeq.Add(1)
		dir := store.Directive{
			ID:     fmt.Sprintf("dir-concierge-%d-%d", clk.Now().UnixNano(), seq),
			RunID:  runID,
			Kind:   kind,
			Source: store.DirectiveSourceOwner,
			Scope:  p.Scope,
			Text:   p.Text,
			Status: store.DirectiveActive,
		}
		if err := st.Directives().Add(ctx, dir); err != nil {
			return "", err
		}
		return fmt.Sprintf("submitted directive %s", dir.ID), nil
	default:
		return "", fmt.Errorf("unknown concierge tool %q", toolName)
	}
}
