package lifecycle

import (
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/swarm-queen/state"
)

// The envelope exists because these are the parts that get forgotten. Literal
// paths especially: a child environment does not point back at the run dir.
func TestRenderBriefCarriesLiteralPaths(t *testing.T) {
	t.Parallel()
	got := RenderBrief(BriefContext{
		RunDir: "/runs/deploy-harden",
		Lane:   &state.Lane{ID: "lane-a", Branch: "swarm/lane-a", Worktree: "/wt/lane-a"},
		Phase:  "swe", Round: 1,
		Goal: "make deploys faster", Target: "swarm/deploy-harden",
		Mission: "Fix the retry backoff.",
	})

	for _, want := range []string{
		"/runs/deploy-harden/lane-a.swe.md",
		"/runs/deploy-harden/lane-a.swe.done",
		"swarm/lane-a",
		"/wt/lane-a",
		"Fix the retry backoff.",
		"touch",
		"LAST",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("brief missing %q:\n%s", want, got)
		}
	}
	// No relative paths or env indirection: they reach nothing from a child.
	if strings.Contains(got, "$RUN") || strings.Contains(got, "./lane-a") {
		t.Errorf("brief must not use indirection for report paths:\n%s", got)
	}
}

// A rework round must report to its own paths, or round 2 overwrites round 1.
func TestRenderBriefIsRoundAware(t *testing.T) {
	t.Parallel()
	got := RenderBrief(BriefContext{
		RunDir: "/r", Lane: &state.Lane{ID: "a", Branch: "b"},
		Phase: "swe", Round: 3, Mission: "again",
		Findings: []string{"the fix regressed the timeout path"},
	})
	if !strings.Contains(got, "/r/a.swe3.done") {
		t.Errorf("round-3 brief must use round-3 paths:\n%s", got)
	}
	if !strings.Contains(got, "the fix regressed the timeout path") {
		t.Errorf("rework findings must reach the lane:\n%s", got)
	}
}

// A review phase needs the rejection path, or a bounced lane has no way to say so.
func TestReviewBriefOffersTheNeedsSWESignal(t *testing.T) {
	t.Parallel()
	got := RenderBrief(BriefContext{
		RunDir: "/r", Lane: &state.Lane{ID: "a", Branch: "b"},
		Phase: "local-review", Round: 1, Mission: "review it",
	})
	if !strings.Contains(got, "/r/a.local-review.needs-swe") {
		t.Errorf("review brief must offer the rework signal:\n%s", got)
	}
}

func TestRenderBriefCarriesStandingRulesAndReservations(t *testing.T) {
	t.Parallel()
	got := RenderBrief(BriefContext{
		RunDir: "/r", Lane: &state.Lane{ID: "a", Branch: "b"},
		Phase: "swe", Round: 1, Mission: "do it",
		Standing: []string{"never enable auto-merge"},
		Reserved: []string{"merging", "deploying"},
	})
	for _, want := range []string{"never enable auto-merge", "merging", "deploying"} {
		if !strings.Contains(got, want) {
			t.Errorf("brief missing %q:\n%s", want, got)
		}
	}
}

// The brief must not pad itself with generic advice: padding buries the
// constraints that actually bind.
func TestRenderBriefStaysTerse(t *testing.T) {
	t.Parallel()
	got := RenderBrief(BriefContext{
		RunDir: "/r", Lane: &state.Lane{ID: "a", Branch: "b"},
		Phase: "swe", Round: 1, Mission: "do it",
	})
	if n := strings.Count(got, "\n"); n > 40 {
		t.Errorf("envelope is %d lines; it should stay terse:\n%s", n, got)
	}
}
