package eval

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/bazelment/yoloswe/orca/replay"
	"github.com/bazelment/yoloswe/orca/router"
)

//go:embed testdata/owner_chat.json
var ownerChatData []byte

// EvalCase preserves a scrubbed source citation and a frozen human label.
//
//nolint:govet // JSON fixture fields follow the human-readable label schema.
type EvalCase struct {
	ID                 string   `json:"id"`
	SourceMailID       string   `json:"source_mail_id"`
	Text               string   `json:"text"`
	Context            string   `json:"context"`
	Destinations       []string `json:"destinations"`
	Rationale          string   `json:"rationale"`
	NeedsClarification bool     `json:"needs_clarification"`
	Excerpt            bool     `json:"excerpt"`
}

// OwnerChatCases loads the frozen owner-mail labels. Labels are not model input.
func OwnerChatCases() ([]EvalCase, error) {
	var cases []EvalCase
	err := json.Unmarshal(ownerChatData, &cases)
	return cases, err
}

// OwnerChatCasesSplit applies the same stable SHA-256 half split used by
// orca/replay. Tune cases may be iterated on; holdout cases are reserved for
// the final measurement.
func OwnerChatCasesSplit(split replay.Split) ([]EvalCase, error) {
	cases, err := OwnerChatCases()
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(cases))
	byID := make(map[string]EvalCase, len(cases))
	for i := range cases {
		ids[i] = cases[i].ID
		byID[cases[i].ID] = cases[i]
	}
	tune, holdout := replay.SplitMomentIDs(ids)
	keep := tune
	if split == replay.SplitHoldout {
		keep = holdout
	}
	if split != replay.SplitTune && split != replay.SplitHoldout {
		return nil, fmt.Errorf("orca/router/eval: unknown split %q", split)
	}
	out := make([]EvalCase, 0, len(keep))
	for _, id := range keep {
		out = append(out, byID[id])
	}
	return out, nil
}

// EvalResult records a handler routing result.
//
//nolint:govet
type EvalResult struct {
	ID           string
	SourceMailID string
	Expected     []router.Handler
	Got          router.Handler
	Decision     router.Decision
	Latency      time.Duration
	Error        string
	Correct      bool
}
type EvalScorecard struct {
	Results    []EvalResult
	Correct    int
	Errors     int
	Fallbacks  int
	P50Latency time.Duration
	P95Latency time.Duration
}

// AcceptedHandlers maps the frozen six-destination labels onto model handlers.
func AcceptedHandlers(destinations []string) []router.Handler {
	out := []router.Handler{}
	for _, d := range destinations {
		h := router.Handler(d)
		switch d {
		case "memory", "controller":
			h = router.Concierge
		case "answer-open-question":
			h = router.Dispatcher
		}
		out = append(out, h)
	}
	if len(destinations) > 1 {
		out = append(out, router.Concierge)
	}
	slices.Sort(out)
	return slices.Compact(out)
}
func Evaluate(ctx context.Context, backend router.Router, cases []EvalCase) EvalScorecard {
	var score EvalScorecard
	var latencies []time.Duration
	for _, c := range cases {
		start := time.Now()
		d, err := backend.Route(ctx, router.FormatInput(c.Text, c.Context))
		elapsed := time.Since(start)
		r := EvalResult{ID: c.ID, SourceMailID: c.SourceMailID, Expected: AcceptedHandlers(c.Destinations), Got: d.Handler, Decision: d, Latency: elapsed}
		if err != nil {
			r.Error = "routing backend error"
			score.Errors++
		}
		r.Correct = err == nil && slices.Contains(r.Expected, r.Got)
		if r.Correct {
			score.Correct++
		}
		if d.Fallback {
			score.Fallbacks++
		}
		score.Results = append(score.Results, r)
		latencies = append(latencies, elapsed)
	}
	if len(latencies) > 0 {
		slices.Sort(latencies)
		score.P50Latency = latencies[(len(latencies)-1)/2]
		score.P95Latency = latencies[(95*len(latencies)+99)/100-1]
	}
	return score
}
func (s EvalScorecard) MeetsTargets() bool {
	return len(s.Results) > 0 && s.Correct*100 >= 90*len(s.Results) && s.Errors == 0
}
func (s EvalScorecard) String() string {
	return fmt.Sprintf("accuracy=%d/%d errors=%d fallbacks=%d p50=%s p95=%s targets_met=%t", s.Correct, len(s.Results), s.Errors, s.Fallbacks, s.P50Latency, s.P95Latency, s.MeetsTargets())
}
