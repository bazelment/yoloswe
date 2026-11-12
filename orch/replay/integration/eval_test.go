//go:build integration

package integration_test

import (
	"cmp"
	"fmt"
	"os"
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

	for i := range rep.MomentResults {
		mr := &rep.MomentResults[i]
		if !mr.Agreed || mr.InvariantViolations > 0 || (mr.DecideDontAsk && mr.AskedOwner) {
			t.Logf("MOMENT MISS %s (run=%s verified=%t): got tools=%v targets=%v asked=%t inv=%d | want tools=%v targets=%v",
				mr.ID, mr.RunID, mr.Grounded, mr.AcceptedTools, mr.AcceptedTargets, mr.AskedOwner, mr.InvariantViolations,
				mr.ExpectedTools, mr.ExpectedTargets)
		}
	}
	for i := range rep.QuestionResults {
		qr := &rep.QuestionResults[i]
		if !qr.Accurate {
			t.Logf("QUESTION MISS %s (run=%s): missing=%v forbidden=%v answer=%q",
				qr.ID, qr.RunID, qr.MissingFacts, qr.ViolatedForbidden, qr.Answer)
		}
	}

	// INCONCLUSIVE (every bar passed but no owner correction is evidenced) is
	// not a pass: the gate must not read green on evidence it does not have.
	if !rep.AllPass() {
		t.Fatalf("Validation Gate 2 verdict %s:\n%s", rep.Verdict(), rep.FormatReport())
	}
}
