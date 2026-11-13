package eval

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/bazelment/yoloswe/orca/router"
)

//go:embed testdata/owner_chat.json
var ownerChatData []byte

// EvalCase preserves a scrubbed source citation and an unaudited human label.
//
//nolint:govet // JSON fixture fields follow the human-readable label schema.
type EvalCase struct {
	ID                 string               `json:"id"`
	SourceMailID       string               `json:"source_mail_id"`
	Text               string               `json:"text"`
	Context            string               `json:"context"`
	Destinations       []router.Destination `json:"destinations"`
	Rationale          string               `json:"rationale"`
	NeedsClarification bool                 `json:"needs_clarification"`
	Excerpt            bool                 `json:"excerpt"`
}

// OwnerChatCases loads the frozen owner-mail labels. Labels are not model input.
func OwnerChatCases() ([]EvalCase, error) {
	var cases []EvalCase
	err := json.Unmarshal(ownerChatData, &cases)
	return cases, err
}

// EvalResult records a whole-message routing result.
//
//nolint:govet // score rows mirror the report schema.
type EvalResult struct {
	ID                       string
	SourceMailID             string
	Expected                 []router.Destination
	Got                      []router.Destination
	Decision                 router.Decision
	Latency                  time.Duration
	Error                    string
	Correct                  bool
	UnnecessaryClarification bool
}

// EvalScorecard reports exact destination-set and clarification agreement.
type EvalScorecard struct {
	Results                   []EvalResult
	Correct                   int
	ClearCases                int
	UnnecessaryClarifications int
	Errors                    int
	MeanLatency               time.Duration
	P50Latency                time.Duration
	P95Latency                time.Duration
}

// Evaluate scores any Router backend. Errors count as misses; calls are serial.
func Evaluate(ctx context.Context, backend router.Router, cases []EvalCase) EvalScorecard {
	var score EvalScorecard
	var latencies []time.Duration
	var total time.Duration
	for i := range cases {
		c := &cases[i]
		input := c.Text
		if c.Context != "" {
			input += "\nOpen questions:\n" + c.Context
		}
		start := time.Now()
		d, err := backend.Route(ctx, input)
		elapsed := time.Since(start)
		result := EvalResult{ID: c.ID, SourceMailID: c.SourceMailID, Expected: canonicalDestinations(c.Destinations), Decision: d, Latency: elapsed}
		if err != nil {
			// Provider errors can contain request headers or upstream response text.
			result.Error = "routing backend error"
			score.Errors++
		}
		result.Got = decisionDestinations(d)
		result.Correct = err == nil && slices.Equal(result.Expected, result.Got) && d.NeedsClarification == c.NeedsClarification
		if result.Correct {
			score.Correct++
		}
		if !c.NeedsClarification {
			score.ClearCases++
			result.UnnecessaryClarification = err == nil && d.NeedsClarification
			if result.UnnecessaryClarification {
				score.UnnecessaryClarifications++
			}
		}
		score.Results = append(score.Results, result)
		latencies = append(latencies, elapsed)
		total += elapsed
	}
	if len(latencies) > 0 {
		slices.Sort(latencies)
		score.MeanLatency = total / time.Duration(len(latencies))
		score.P50Latency = latencies[(len(latencies)-1)/2]
		score.P95Latency = latencies[(95*len(latencies)+99)/100-1]
	}
	return score
}

func canonicalDestinations(ds []router.Destination) []router.Destination {
	ds = slices.Clone(ds)
	slices.Sort(ds)
	return slices.Compact(ds)
}
func decisionDestinations(d router.Decision) []router.Destination {
	if len(d.Parts) == 0 {
		return []router.Destination{d.Destination}
	}
	var ds []router.Destination
	for i := range d.Parts {
		ds = append(ds, decisionDestinations(d.Parts[i])...)
	}
	return canonicalDestinations(ds)
}

// MeetsTargets requires >=90% accuracy, <10% needless asks, and no errors.
func (s EvalScorecard) MeetsTargets() bool {
	return len(s.Results) > 0 && s.ClearCases > 0 && s.Correct*100 >= 90*len(s.Results) && s.UnnecessaryClarifications*100 < 10*s.ClearCases && s.Errors == 0
}

func (s EvalScorecard) String() string {
	accuracy, unnecessary := 0.0, 0.0
	if len(s.Results) > 0 {
		accuracy = 100 * float64(s.Correct) / float64(len(s.Results))
	}
	if s.ClearCases > 0 {
		unnecessary = 100 * float64(s.UnnecessaryClarifications) / float64(s.ClearCases)
	}
	return fmt.Sprintf("routing accuracy: %d/%d (%.2f%%); unnecessary clarifications: %d/%d (%.2f%%); errors: %d; latency mean=%s p50=%s p95=%s; targets_met=%t", s.Correct, len(s.Results), accuracy, s.UnnecessaryClarifications, s.ClearCases, unnecessary, s.Errors, s.MeanLatency.Round(time.Millisecond), s.P50Latency.Round(time.Millisecond), s.P95Latency.Round(time.Millisecond), s.MeetsTargets())
}
