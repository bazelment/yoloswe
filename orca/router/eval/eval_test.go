package eval

import (
	"context"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/orca/replay"
	"github.com/bazelment/yoloswe/orca/router"
)

type sequenceRouter struct{ decisions []router.Decision }

func (r *sequenceRouter) Route(context.Context, string) (router.Decision, error) {
	d := r.decisions[0]
	r.decisions = r.decisions[1:]
	return d, nil
}

func TestEvaluateAcceptsMappedHandlers(t *testing.T) {
	cases := []EvalCase{
		{ID: "memory", Destinations: []string{"memory"}},
		{ID: "answer", Destinations: []string{"answer-open-question"}},
		{ID: "multipart-front-desk", Destinations: []string{"planner", "dispatcher"}},
		{ID: "multipart-planner", Destinations: []string{"planner", "dispatcher"}},
		{ID: "wrong", Destinations: []string{"planner"}},
	}
	r := &sequenceRouter{decisions: []router.Decision{{Handler: router.Concierge}, {Handler: router.Dispatcher, Fallback: true}, {Handler: router.Concierge}, {Handler: router.Planner}, {Handler: router.Dispatcher}}}
	s := Evaluate(t.Context(), r, cases)
	if s.Correct != 4 || s.Fallbacks != 1 || s.Errors != 0 || s.MeetsTargets() {
		t.Fatal(s)
	}
}
func TestScorecardTargets(t *testing.T) {
	s := EvalScorecard{Results: make([]EvalResult, 10), Correct: 9}
	if !s.MeetsTargets() {
		t.Fatal(s)
	}
	s.Errors = 1
	if s.MeetsTargets() {
		t.Fatal(s)
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
	covered := map[string]bool{}
	ambiguous, multipart := 0, 0
	for _, c := range cases {
		if seen[c.ID] || c.Text == "" || c.Rationale == "" || !strings.HasPrefix(c.SourceMailID, "s:") || len(c.Destinations) == 0 {
			t.Fatalf("invalid case: %+v", c)
		}
		seen[c.ID] = true
		for _, d := range c.Destinations {
			if !validEvalDestination(d) {
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
	if len(covered) != 6 || ambiguous != 0 || multipart < 2 {
		t.Fatalf("coverage: destinations=%d ambiguous=%d multipart=%d", len(covered), ambiguous, multipart)
	}
}

func TestOwnerChatSplitMatchesReplayHashSplit(t *testing.T) {
	tune, err := OwnerChatCasesSplit(replay.SplitTune)
	if err != nil {
		t.Fatal(err)
	}
	holdout, err := OwnerChatCasesSplit(replay.SplitHoldout)
	if err != nil {
		t.Fatal(err)
	}
	if len(tune) != 30 || len(holdout) != 30 {
		t.Fatalf("split sizes: %d/%d", len(tune), len(holdout))
	}
}

func TestAnswerOpenQuestionCasesIncludeContext(t *testing.T) {
	cases, err := OwnerChatCases()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		for _, d := range c.Destinations {
			if d == "answer-open-question" && c.Context == "" {
				t.Errorf("%s expects answer-open-question without context", c.ID)
			}
		}
	}
}

func validEvalDestination(d string) bool {
	switch d {
	case "concierge", "answer-open-question", "planner", "memory", "dispatcher", "controller":
		return true
	default:
		return false
	}
}
