package router

import (
	"context"
	"strings"
	"testing"
)

type sequenceRouter struct{ decisions []Decision }

func (r *sequenceRouter) Route(context.Context, string) (Decision, error) {
	d := r.decisions[0]
	r.decisions = r.decisions[1:]
	return d, nil
}

func TestEvaluateScoresWholeMessageAndClarification(t *testing.T) {
	cases := []EvalCase{
		{ID: "multipart", Destinations: []Destination{Planner, Memory}},
		{ID: "ambiguous", Destinations: []Destination{Dispatcher}, NeedsClarification: true},
		{ID: "needless-ask", Destinations: []Destination{Concierge}},
		{ID: "missing-part", Destinations: []Destination{Planner, Dispatcher}},
	}
	backend := &sequenceRouter{decisions: []Decision{
		{Parts: []Decision{{Destination: Memory}, {Destination: Planner}, {Destination: Memory}}},
		{Destination: Dispatcher, NeedsClarification: true},
		{Destination: Concierge, NeedsClarification: true},
		{Destination: Planner},
	}}
	s := Evaluate(context.Background(), backend, cases)
	if s.Correct != 2 || s.ClearCases != 3 || s.UnnecessaryClarifications != 1 || s.MeetsTargets() {
		t.Fatalf("bad accounting: %s", s)
	}
	if !strings.Contains(s.String(), "2/4 (50.00%)") {
		t.Fatal(s.String())
	}
}

func TestScorecardStrictClarificationBar(t *testing.T) {
	s := EvalScorecard{Results: make([]EvalResult, 10), Correct: 9, ClearCases: 10}
	if !s.MeetsTargets() {
		t.Fatal("90% must pass")
	}
	s.UnnecessaryClarifications = 1
	if s.MeetsTargets() {
		t.Fatal("10% needless clarification must fail")
	}
	s.UnnecessaryClarifications = 0
	s.Errors = 1
	if s.MeetsTargets() {
		t.Fatal("backend failures must fail")
	}
}

func TestOwnerChatDatasetCoverage(t *testing.T) {
	cases, err := OwnerChatCases()
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 60 {
		t.Fatalf("want 60 cases, got %d", len(cases))
	}
	seen := map[string]bool{}
	covered := map[Destination]bool{}
	ambiguous, multipart := 0, 0
	for _, c := range cases {
		if seen[c.ID] || c.Text == "" || c.Rationale == "" || !strings.HasPrefix(c.SourceMailID, "s:") || len(c.Destinations) == 0 {
			t.Fatalf("invalid case: %+v", c)
		}
		seen[c.ID] = true
		for _, d := range c.Destinations {
			if !validDestination(d) {
				t.Fatalf("invalid destination: %s", d)
			}
			covered[d] = true
		}
		if c.NeedsClarification {
			ambiguous++
		}
		if len(c.Destinations) > 1 {
			multipart++
		}
	}
	if len(covered) != 6 || ambiguous < 5 || multipart < 2 {
		t.Fatalf("coverage: destinations=%d ambiguous=%d multipart=%d", len(covered), ambiguous, multipart)
	}
}
