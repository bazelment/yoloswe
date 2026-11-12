package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/orch/clock"
	"github.com/bazelment/yoloswe/orch/controller"
	"github.com/bazelment/yoloswe/orch/dispatcher"
	"github.com/bazelment/yoloswe/orch/lane"
	"github.com/bazelment/yoloswe/orch/store"
)

// ToolMetricsSnapshot is the Decision.Tool name used for metrics records written to DecisionStore.
const (
	ToolMetricsSnapshot = "metrics.snapshot"
	ActorMetrics        = "metrics"
)

// DayMetrics aggregates decision counts, token usage, and USD cost for a single UTC calendar day.
type DayMetrics struct {
	Date               string  `json:"date"`
	TotalDecisions     int     `json:"total_decisions"`
	AutoDecisions      int     `json:"auto_decisions"`
	JudgmentDecisions  int     `json:"judgment_decisions"`
	InputTokens        int64   `json:"input_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
	TotalTokens        int64   `json:"total_tokens"`
	CostUSD            float64 `json:"cost_usd"`
	TokensPerDecision  float64 `json:"tokens_per_decision"`
	CostPerDecisionUSD float64 `json:"cost_per_decision_usd"`
}

// LaneIdleMetric captures the actionable-event idle time for a single lane.
type LaneIdleMetric struct {
	LaneID           string `json:"lane_id"`
	UnhandledEventID string `json:"unhandled_event_id,omitempty"`
	IdleMs           int64  `json:"idle_ms"`
}

// RunMetrics holds the run-level orchestration metrics written to DecisionStore
// and surfaced by the Concierge.
type RunMetrics struct {
	ComputedAt                 time.Time        `json:"computed_at"`
	RunID                      string           `json:"run_id"`
	Days                       []DayMetrics     `json:"days,omitempty"`
	LaneIdle                   []LaneIdleMetric `json:"lane_idle,omitempty"`
	TotalDecisions             int              `json:"total_decisions"`
	AutoDecisions              int              `json:"auto_decisions"`
	JudgmentDecisions          int              `json:"judgment_decisions"`
	TotalInputTokens           int64            `json:"total_input_tokens"`
	TotalOutputTokens          int64            `json:"total_output_tokens"`
	TotalTokens                int64            `json:"total_tokens"`
	TotalCostUSD               float64          `json:"cost_usd"`
	TokensPerDecision          float64          `json:"tokens_per_decision"`
	TokensPerJudgmentDecision  float64          `json:"tokens_per_judgment_decision"`
	CostPerDecisionUSD         float64          `json:"cost_per_decision_usd"`
	CostPerJudgmentDecisionUSD float64          `json:"cost_per_judgment_decision_usd"`
	OwnerAsks                  int              `json:"owner_asks"`
	OwnerAskDefaultsApplied    int              `json:"owner_ask_defaults_applied"`
	OwnerNudges                int              `json:"owner_nudges"`
	OwnerCorrections           int              `json:"owner_corrections"`
	LaneIdleTimeMs             int64            `json:"lane_idle_time_ms"`
	MaxLaneIdleTimeMs          int64            `json:"max_lane_idle_time_ms"`
	AvgEventToDecisionMs       int64            `json:"avg_event_to_decision_ms"`
	MaxEventToDecisionMs       int64            `json:"max_event_to_decision_ms"`
}

// Summary formats a concise single-line summary of m suitable for Decision.Result and logs.
func (m RunMetrics) Summary() string {
	return fmt.Sprintf(
		"decisions=%d (auto=%d judgment=%d) tokens=%d (in=%d out=%d avg=%.1f/dec) cost=$%.4f ($%.4f/dec) asks=%d nudges=%d corrections=%d idle_ms=%d (max=%d) event_to_decision_ms=%d",
		m.TotalDecisions,
		m.AutoDecisions,
		m.JudgmentDecisions,
		m.TotalTokens,
		m.TotalInputTokens,
		m.TotalOutputTokens,
		m.TokensPerDecision,
		m.TotalCostUSD,
		m.CostPerDecisionUSD,
		m.OwnerAsks,
		m.OwnerNudges,
		m.OwnerCorrections,
		m.LaneIdleTimeMs,
		m.MaxLaneIdleTimeMs,
		m.AvgEventToDecisionMs,
	)
}

// ComputeMetrics calculates RunMetrics from Store records (decisions, events, directives, lanes, threads).
func ComputeMetrics(ctx context.Context, st store.Store, clk clock.Clock, runID string) (RunMetrics, error) {
	if clk == nil {
		clk = clock.NewRealClock()
	}
	now := clk.Now().UTC()

	decs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: runID})
	if err != nil {
		return RunMetrics{}, err
	}
	events, err := st.Events().List(ctx, store.EventFilter{RunID: runID})
	if err != nil {
		return RunMetrics{}, err
	}
	dirs, err := st.Directives().List(ctx, store.DirectiveFilter{RunID: runID})
	if err != nil {
		return RunMetrics{}, err
	}
	lanes, err := st.Lanes().List(ctx, store.LaneFilter{RunID: runID})
	if err != nil {
		return RunMetrics{}, err
	}

	eventsByID := make(map[string]store.Event, len(events))
	for i := range events {
		eventsByID[events[i].ID] = events[i]
	}

	dayMap := make(map[string]*DayMetrics)
	handledEventAt := make(map[string]time.Time)

	var (
		out               = RunMetrics{RunID: runID, ComputedAt: now}
		seenAskRefs       = make(map[string]bool)
		eventLatencySumMs int64
		eventLatencyCount int64
	)

	for i := range decs {
		d := decs[i]
		if d.Tool == ToolMetricsSnapshot {
			continue
		}

		out.TotalDecisions++
		switch d.Category {
		case store.DecisionJudgment:
			out.JudgmentDecisions++
		default:
			out.AutoDecisions++
		}

		toks := max(d.InputTokens, 0) + max(d.OutputTokens, 0)
		out.TotalInputTokens += max(d.InputTokens, 0)
		out.TotalOutputTokens += max(d.OutputTokens, 0)
		out.TotalTokens += toks
		out.TotalCostUSD += max(d.CostUSD, 0)

		dayKey := d.CreatedAt.UTC().Format("2006-01-02")
		if d.CreatedAt.IsZero() {
			dayKey = now.Format("2006-01-02")
		}
		dm, ok := dayMap[dayKey]
		if !ok {
			dm = &DayMetrics{Date: dayKey}
			dayMap[dayKey] = dm
		}
		dm.TotalDecisions++
		if d.Category == store.DecisionJudgment {
			dm.JudgmentDecisions++
		} else {
			dm.AutoDecisions++
		}
		dm.InputTokens += max(d.InputTokens, 0)
		dm.OutputTokens += max(d.OutputTokens, 0)
		dm.TotalTokens += toks
		dm.CostUSD += max(d.CostUSD, 0)

		if d.Tool == dispatcher.ToolOwnerAsk {
			out.OwnerAsks++
			var p struct {
				ID string `json:"id"`
			}
			if len(d.Args) > 0 && json.Unmarshal(d.Args, &p) == nil && p.ID != "" {
				seenAskRefs["owner_ask:"+p.ID] = true
			}
		}
		if d.Tool == controller.ToolOwnerAskDefault {
			out.OwnerAskDefaultsApplied++
		}

		// Track event-to-decision latency from referenced EventID / EventIDs or LatencyMs.
		var cArgs controller.DecisionArgs
		if len(d.Args) > 0 && json.Unmarshal(d.Args, &cArgs) == nil {
			refIDs := make([]string, 0, 1+len(cArgs.EventIDs))
			if cArgs.EventID != "" {
				refIDs = append(refIDs, cArgs.EventID)
			}
			refIDs = append(refIDs, cArgs.EventIDs...)
			for _, evID := range refIDs {
				if ev, exists := eventsByID[evID]; exists {
					if prev, has := handledEventAt[evID]; !has || d.CreatedAt.Before(prev) {
						handledEventAt[evID] = d.CreatedAt
					}
					if !ev.CreatedAt.IsZero() && !d.CreatedAt.Before(ev.CreatedAt) {
						latMs := d.CreatedAt.Sub(ev.CreatedAt).Milliseconds()
						eventLatencySumMs += latMs
						eventLatencyCount++
						if latMs > out.MaxEventToDecisionMs {
							out.MaxEventToDecisionMs = latMs
						}
					}
				}
			}
		}
		if d.LatencyMs > 0 {
			eventLatencySumMs += d.LatencyMs
			eventLatencyCount++
			if d.LatencyMs > out.MaxEventToDecisionMs {
				out.MaxEventToDecisionMs = d.LatencyMs
			}
		}
	}

	if out.TotalDecisions > 0 {
		out.TokensPerDecision = float64(out.TotalTokens) / float64(out.TotalDecisions)
		out.CostPerDecisionUSD = out.TotalCostUSD / float64(out.TotalDecisions)
	}
	if out.JudgmentDecisions > 0 {
		out.TokensPerJudgmentDecision = float64(out.TotalTokens) / float64(out.JudgmentDecisions)
		out.CostPerJudgmentDecisionUSD = out.TotalCostUSD / float64(out.JudgmentDecisions)
	}
	if eventLatencyCount > 0 {
		out.AvgEventToDecisionMs = eventLatencySumMs / eventLatencyCount
	}

	dayKeys := slices.Sorted(maps.Keys(dayMap))
	out.Days = make([]DayMetrics, 0, len(dayKeys))
	for _, k := range dayKeys {
		dm := dayMap[k]
		if dm.TotalDecisions > 0 {
			dm.TokensPerDecision = float64(dm.TotalTokens) / float64(dm.TotalDecisions)
			dm.CostPerDecisionUSD = dm.CostUSD / float64(dm.TotalDecisions)
		}
		out.Days = append(out.Days, *dm)
	}

	for i := range dirs {
		dir := dirs[i]
		if strings.HasPrefix(dir.Ref, "owner_ask:") && !seenAskRefs[dir.Ref] {
			seenAskRefs[dir.Ref] = true
			out.OwnerAsks++
		}
		if dir.Source == store.DirectiveSourceOwner || dir.Source == store.DirectiveSourceConcierge {
			switch dir.Kind {
			case store.DirectiveStanding, store.DirectiveRecipeEdit:
				out.OwnerCorrections++
			case store.DirectiveInstruction:
				if !strings.HasPrefix(dir.Ref, "owner_ask:") {
					out.OwnerNudges++
				}
			}
		}
	}
	for i := range events {
		if events[i].Kind == store.EventOwnerMessage {
			out.OwnerNudges++
		}
	}

	// Compute lane idle time with an actionable event (either waiting to be handled or latency until handled).
	laneIdleByID := make(map[string]*LaneIdleMetric, len(lanes))
	for i := range lanes {
		laneIdleByID[lanes[i].ID] = &LaneIdleMetric{LaneID: lanes[i].ID}
	}
	lanesByID := make(map[string]lane.Lane, len(lanes))
	for i := range lanes {
		lanesByID[lanes[i].ID] = lanes[i]
	}

	for i := range events {
		ev := events[i]
		if ev.LaneID == "" || !isActionableLaneEvent(ev) {
			continue
		}
		lim, ok := laneIdleByID[ev.LaneID]
		if !ok {
			lim = &LaneIdleMetric{LaneID: ev.LaneID}
			laneIdleByID[ev.LaneID] = lim
		}
		if handledAt, handled := handledEventAt[ev.ID]; handled {
			if !ev.CreatedAt.IsZero() && !handledAt.Before(ev.CreatedAt) {
				durMs := handledAt.Sub(ev.CreatedAt).Milliseconds()
				lim.IdleMs += durMs
			}
			continue
		}
		// If the lane is still non-terminal and the event is unhandled, accumulate time waiting since ev.CreatedAt.
		if ln, exists := lanesByID[ev.LaneID]; !exists || !ln.Status.Terminal() {
			if !ev.CreatedAt.IsZero() && !now.Before(ev.CreatedAt) {
				durMs := now.Sub(ev.CreatedAt).Milliseconds()
				lim.IdleMs += durMs
				lim.UnhandledEventID = ev.ID
			}
		}
	}

	laneIDs := slices.Sorted(maps.Keys(laneIdleByID))
	for _, id := range laneIDs {
		lim := laneIdleByID[id]
		out.LaneIdleTimeMs += lim.IdleMs
		if lim.IdleMs > out.MaxLaneIdleTimeMs {
			out.MaxLaneIdleTimeMs = lim.IdleMs
		}
		out.LaneIdle = append(out.LaneIdle, *lim)
	}

	return out, nil
}

func isActionableLaneEvent(ev store.Event) bool {
	switch ev.Kind {
	case store.EventLaneReport, store.EventPhaseExit, store.EventCI, store.EventStall, store.EventOwnerAskExpired:
		return true
	default:
		return false
	}
}

// RecordMetricsDecision computes the current RunMetrics for runID and appends
// a structured metrics.snapshot record to store.DecisionStore.
func RecordMetricsDecision(
	ctx context.Context,
	st store.Store,
	clk clock.Clock,
	runID, reason string,
) (RunMetrics, store.Decision, error) {
	m, err := ComputeMetrics(ctx, st, clk, runID)
	if err != nil {
		return RunMetrics{}, store.Decision{}, err
	}
	rawArgs, err := json.Marshal(m)
	if err != nil {
		return m, store.Decision{}, err
	}
	dec, err := st.Decisions().Append(ctx, store.Decision{
		RunID:        runID,
		Category:     store.DecisionAuto,
		Actor:        ActorMetrics,
		Tool:         ToolMetricsSnapshot,
		Reason:       cmp.Or(strings.TrimSpace(reason), "metrics snapshot"),
		Result:       m.Summary(),
		Args:         rawArgs,
		InputTokens:  m.TotalInputTokens,
		OutputTokens: m.TotalOutputTokens,
		CostUSD:      m.TotalCostUSD,
		Vetted:       true,
	})
	if err != nil {
		return m, store.Decision{}, err
	}
	return m, dec, nil
}

// LatestMetricsFromDecisions returns the most recent RunMetrics recorded in
// store.DecisionStore, falling back to ComputeMetrics if none has been written yet.
func LatestMetricsFromDecisions(ctx context.Context, st store.Store, clk clock.Clock, runID string) (RunMetrics, error) {
	decs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: runID})
	if err != nil {
		return RunMetrics{}, err
	}
	for i := len(decs) - 1; i >= 0; i-- {
		if decs[i].Tool == ToolMetricsSnapshot && len(decs[i].Args) > 0 {
			var m RunMetrics
			if err := json.Unmarshal(decs[i].Args, &m); err == nil {
				return m, nil
			}
		}
	}
	return ComputeMetrics(ctx, st, clk, runID)
}
