//go:build integration

package integration_test

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orch/dispatcher"
	"github.com/bazelment/yoloswe/orch/model"
	"github.com/bazelment/yoloswe/orch/replay"
)

func liveClientFromEnv(t *testing.T) *model.HTTPClient {
	t.Helper()
	if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) == "" &&
		strings.TrimSpace(os.Getenv("ANTHROPIC_AUTH_TOKEN")) == "" &&
		strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")) == "" {
		t.Skip("skipping live Gate 2 replay eval: none of ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, or ANTHROPIC_BASE_URL is set")
	}
	return model.NewHTTPClient(
		model.WithMaxRetries(4),
		model.WithRetryBackoff(500*time.Millisecond, 5*time.Second),
	)
}

func TestValidationGate2ReplayEval(t *testing.T) {
	client := liveClientFromEnv(t)

	var (
		ds  *replay.Dataset
		err error
	)
	if os.Getenv("ORCH_REPLAY_INNER_LOOP_ONLY") == "1" {
		ds, err = replay.LoadInnerLoopDataset()
	} else {
		ds, err = replay.LoadDefaultDataset()
	}
	if err != nil {
		t.Fatalf("load dataset: %v", err)
	}

	// ORCH_REPLAY_SPLIT=tune|holdout (see replay.SplitMomentIDs) or
	// ORCH_REPLAY_MOMENTS=<ids> runs a subset: the bars are reported, but the
	// verdict is asserted only on the full set (Concierge questions included).
	subset := false
	if split := strings.TrimSpace(os.Getenv("ORCH_REPLAY_SPLIT")); split != "" {
		if ds, err = ds.Subset(replay.Split(split)); err != nil {
			t.Fatalf("split dataset: %v", err)
		}
		subset = true
	}
	if raw := strings.TrimSpace(os.Getenv("ORCH_REPLAY_MOMENTS")); raw != "" {
		if ds, err = ds.SelectMoments(strings.Split(raw, ",")); err != nil {
			t.Fatalf("select moments: %v", err)
		}
		subset = true
	}

	concurrency := 6
	if raw := strings.TrimSpace(os.Getenv("ORCH_REPLAY_CONCURRENCY")); raw != "" {
		if n, pErr := strconv.Atoi(raw); pErr == nil && n > 0 {
			concurrency = n
		}
	}

	modelName := cmp.Or(strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL")), replay.DefaultEvalModel)
	concierge := replay.NewStoreConcierge(client)
	concierge.Model = modelName

	rep, err := replay.Evaluate(t.Context(), ds, replay.EvalConfig{
		Client:    client,
		Concierge: concierge,
		DispatcherConfig: dispatcher.Config{
			Model:          modelName,
			HardBatchModel: modelName,
			MaxTurns:       4,
			MaxTokens:      1024,
		},
		Concurrency: concurrency,
	})
	if err != nil {
		t.Fatalf("replay.Evaluate: %v", err)
	}

	reportText := rep.FormatReport()
	fmt.Println(reportText)
	t.Log("\n" + reportText)

	if !subset {
		printSplitReports(rep)
	}

	for i := range rep.MomentResults {
		mr := &rep.MomentResults[i]
		if !mr.Agreed || mr.InvariantViolations > 0 || (mr.ForbidOwnerAsk && mr.AskedOwner) {
			fmt.Printf("MOMENT MISS %s (run=%s verified=%t): got tools=%v targets=%v asked=%t inv=%d rejected=%q | want tools=%v targets=%v | summary=%q\n",
				mr.ID, mr.RunID, mr.Grounded, mr.AcceptedTools, mr.AcceptedTargets, mr.AskedOwner, mr.InvariantViolations,
				mr.RejectedReasons, mr.ExpectedTools, mr.ExpectedTargets, mr.Summary)
		}
	}
	for i := range rep.QuestionResults {
		qr := &rep.QuestionResults[i]
		if !qr.Accurate {
			t.Logf("QUESTION MISS %s (run=%s): missing=%v forbidden=%v answer=%q",
				qr.ID, qr.RunID, qr.MissingFacts, qr.ViolatedForbidden, qr.Answer)
		}
	}

	if subset {
		t.Logf("subset run (%d moments): verdict not asserted; compare the per-bar rows above", len(rep.MomentResults))
		return
	}

	// INCONCLUSIVE (every bar passed but no owner correction is evidenced) is
	// not a pass: the gate must not read green on evidence it does not have.
	if !rep.AllPass() {
		t.Fatalf("Validation Gate 2 verdict %s:\n%s", rep.Verdict(), rep.FormatReport())
	}
}

// printSplitReports scores a full run once per split (see replay.SplitMomentIDs),
// so tune and holdout numbers come from the same single pass.
func printSplitReports(rep *replay.EvalReport) {
	ids := make([]string, len(rep.MomentResults))
	for i := range rep.MomentResults {
		ids[i] = rep.MomentResults[i].ID
	}
	tune, holdout := replay.SplitMomentIDs(ids)
	for _, half := range []struct {
		name string
		ids  []string
	}{{string(replay.SplitTune), tune}, {string(replay.SplitHoldout), holdout}} {
		var moments []replay.MomentResult
		for i := range rep.MomentResults {
			if slices.Contains(half.ids, rep.MomentResults[i].ID) {
				moments = append(moments, rep.MomentResults[i])
			}
		}
		sub := replay.ComputeReport(moments, nil)
		fmt.Printf("\n--- %s split (%d moments; Concierge questions are scored on the full set only) ---\n%s", half.name, len(moments), sub.FormatReport())
	}
}
