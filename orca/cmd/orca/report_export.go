package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/replay"
	"github.com/bazelment/yoloswe/orca/store"
)

// runReport is intentionally JSON so reports can be consumed by scripts.
//
//nolint:govet // report layout is kept readable for the JSON contract.
type runReport struct {
	RunID                  string             `json:"run_id"`
	DecisionsByRole        map[string]int     `json:"decisions_by_role"`
	Asks                   int                `json:"asks"`
	Answers                int                `json:"answers"`
	OwnerCorrectionsByKind map[string]int     `json:"owner_corrections_by_kind"`
	RepeatedCorrections    int                `json:"repeated_corrections"`
	Restarts               int                `json:"restarts"`
	IdleSlotMinutes        float64            `json:"idle_slot_minutes"`
	CostByRole             map[string]float64 `json:"cost_by_role"`
}

func reportRun(ctx context.Context, st store.Store, runID string, day string, out io.Writer) error {
	filter := store.DecisionFilter{RunID: runID}
	if day != "" {
		t, err := time.Parse("2006-01-02", day)
		if err != nil {
			return err
		}
		filter.SinceTime = &t
	}
	decs, err := st.Decisions().List(ctx, filter)
	if err != nil {
		return err
	}
	r := runReport{RunID: runID, DecisionsByRole: map[string]int{}, OwnerCorrectionsByKind: map[string]int{}, CostByRole: map[string]float64{}}
	seen := map[string]bool{}
	for i := range decs {
		d := &decs[i]
		r.DecisionsByRole[d.Actor]++
		r.CostByRole[d.Actor] += d.CostUSD
		if strings.Contains(strings.ToLower(d.Tool), "owner.ask") {
			r.Asks++
		}
		if strings.Contains(strings.ToLower(d.Tool), "owner.answer") {
			r.Answers++
		}
		if d.OutcomeLabel == "owner corrected" || d.OutcomeLabel == "corrected" {
			k := d.Tool
			r.OwnerCorrectionsByKind[k]++
			if seen[k] {
				r.RepeatedCorrections++
			}
			seen[k] = true
		}
		if strings.Contains(strings.ToLower(d.Tool), "restart") {
			r.Restarts++
		}
	}
	return json.NewEncoder(out).Encode(r)
}

func exportRun(ctx context.Context, st store.Store, runID string, out io.Writer) error {
	run, err := st.Runs().Get(ctx, runID)
	if err != nil {
		return err
	}
	decs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: runID})
	if err != nil {
		return err
	}
	events, err := st.Events().List(ctx, store.EventFilter{RunID: runID})
	if err != nil {
		return err
	}
	if len(events) == 0 {
		events = []store.Event{{ID: "synthetic-event-1", RunID: runID, Kind: store.EventTimer, Summary: "recorded decision"}}
	}
	moments := make([]replay.JudgmentMoment, 0, len(decs))
	for i := range decs {
		d := decs[i]
		id := d.ID
		if id == "" {
			id = fmt.Sprintf("moment-%d", i+1)
		}
		moments = append(moments, replay.JudgmentMoment{ID: id, RunID: runID, Timestamp: d.CreatedAt, Description: d.Reason, Grounding: replay.GroundingSynthetic, GroundingEvidence: "exported from orca decision log", Events: []store.Event{events[min(i, len(events)-1)]}, State: replay.MomentState{Run: *run, Decisions: []store.Decision{d}}, Source: replay.Source{Relation: replay.RelationFeedback, Evidence: replay.EvidenceOwnerAttributed, LabelBasis: "recorded outcome", IDs: []string{id}}, Baseline: replay.BaselineDecision{Tool: d.Tool, Summary: d.Result}, OwnerFollowUp: replay.OwnerFollowUp{Verdict: replay.VerdictEndorsed, ExpectedTools: []string{d.Tool}}})
	}
	if len(moments) == 0 {
		return fmt.Errorf("run %q has no decisions", runID)
	}
	return json.NewEncoder(out).Encode(replay.Dataset{Version: "orca-export-v1", ProvenanceRubric: "synthetic export from durable decision records", Runs: []string{runID}, Moments: moments})
}

func openReportStore(cfg Config) (store.Store, error) {
	st, _, err := openStore(cfg, clock.NewRealClock())
	return st, err
}
