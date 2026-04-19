package prdozer

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// concurrencyTrackingPolish records the maximum number of concurrent Run calls.
type concurrencyTrackingPolish struct {
	mu      sync.Mutex
	current int32
	peak    int32
	calls   int32
}

func (c *concurrencyTrackingPolish) Run(ctx context.Context, _ PolishRequest) (PolishResult, error) {
	atomic.AddInt32(&c.calls, 1)
	cur := atomic.AddInt32(&c.current, 1)
	defer atomic.AddInt32(&c.current, -1)
	c.mu.Lock()
	if cur > c.peak {
		c.peak = cur
	}
	c.mu.Unlock()
	// Simulate non-trivial work so concurrency actually overlaps.
	select {
	case <-time.After(20 * time.Millisecond):
	case <-ctx.Done():
	}
	return PolishResult{SessionID: "stub"}, nil
}

func setupGHForOrch(t *testing.T) *fakeGH {
	t.Helper()
	gh := newFakeGH()
	// Discovery: pr list returns 3 PRs.
	gh.addPrefix("pr list --state open --json number,headRefName,baseRefName,url,isDraft,labels --limit 200 --author @me", `[
        {"number":10,"headRefName":"feat-a","baseRefName":"main","url":"https://github.com/o/r/pull/10","isDraft":false,"labels":[]},
        {"number":20,"headRefName":"feat-b","baseRefName":"main","url":"https://github.com/o/r/pull/20","isDraft":false,"labels":[]},
        {"number":30,"headRefName":"feat-c","baseRefName":"main","url":"https://github.com/o/r/pull/30","isDraft":false,"labels":[{"name":"wip"}]}
    ]`)
	// Per-PR snapshot. Use a FAILURE rollup so polish would be invoked.
	for _, n := range []int{10, 20, 30} {
		nStr := numToStr(n)
		gh.addPrefix("pr view "+nStr+" --json number,url,headRefName,baseRefName,headRefOid,state,isDraft,reviewDecision,mergeable", `{
            "number":`+nStr+`,
            "url":"https://github.com/o/r/pull/`+nStr+`",
            "headRefName":"feat","baseRefName":"main","headRefOid":"head1",
            "state":"OPEN","isDraft":false,"reviewDecision":"REVIEW_REQUIRED","mergeable":"MERGEABLE"
        }`)
		gh.addPrefix("pr view "+nStr+" --json statusCheckRollup", failureRollupJSON)
		gh.addPrefix("pr view "+nStr+" --json number,headRefName,baseRefName,url,isDraft,labels", `{
            "number":`+nStr+`,"headRefName":"feat","baseRefName":"main","url":"https://github.com/o/r/pull/`+nStr+`","isDraft":false,"labels":[]
        }`)
	}
	gh.addPrefix("run list --branch feat --status failure", "[]")
	gh.addPrefix("api repos/{owner}/{repo}/git/refs/heads/main", "base1")
	gh.addPrefix("api --paginate repos/o/r/pulls/", "[]")
	gh.addPrefix("api --paginate repos/o/r/issues/", "[]")
	return gh
}

func numToStr(n int) string {
	switch n {
	case 10:
		return "10"
	case 20:
		return "20"
	case 30:
		return "30"
	}
	return ""
}

func TestOrchestrator_RunOnce_RespectsExcludeLabels(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gh := setupGHForOrch(t)
	polish := &concurrencyTrackingPolish{}
	cfg := DefaultConfig()
	cfg.Source.MaxConcurrent = 5
	o := NewOrchestrator(cfg, gh, polish, ".", "r", nil)

	results, err := o.RunOnce(context.Background())
	require.NoError(t, err)
	// PR 30 is excluded by the "wip" label.
	assert.Len(t, results, 2)
	assert.Contains(t, results, 10)
	assert.Contains(t, results, 20)
	assert.NotContains(t, results, 30)
}

func TestOrchestrator_RunOnce_ConcurrencyLimited(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gh := setupGHForOrch(t)
	polish := &concurrencyTrackingPolish{}
	cfg := DefaultConfig()
	cfg.Source.MaxConcurrent = 1
	cfg.Source.Filter.ExcludeLabels = nil // include the wip PR so we have 3 to run
	o := NewOrchestrator(cfg, gh, polish, ".", "r", nil)

	// Pre-seed all 3 PRs with state so they aren't first-run (which is idle for FAILURE...
	// actually FAILURE is actionable on first run too. But we want polish to fire for all 3.)
	// First run with FAILURE rollup is actionable per ComputeChangeset.
	_, err := o.RunOnce(context.Background())
	require.NoError(t, err)

	assert.LessOrEqual(t, polish.peak, int32(1), "should not exceed MaxConcurrent")
	assert.Equal(t, int32(3), polish.calls, "all three PRs should have been polished")
}

func TestDiscoverPRs_List(t *testing.T) {
	t.Parallel()
	gh := newFakeGH()
	gh.addPrefix("pr view 5 --json number,headRefName,baseRefName,url,isDraft,labels", `{
        "number":5,"headRefName":"x","baseRefName":"main","url":"u","isDraft":false,"labels":[{"name":"a"}]
    }`)
	prs, err := DiscoverPRs(context.Background(), gh, ".", SourceConfig{
		Mode: SourceModeList,
		PRs:  []int{5},
	})
	require.NoError(t, err)
	require.Len(t, prs, 1)
	assert.Equal(t, 5, prs[0].Number)
	assert.Equal(t, []string{"a"}, prs[0].Labels)
}

func TestDiscoverPRs_AllPassesAuthorFilter(t *testing.T) {
	t.Parallel()
	gh := newFakeGH()
	gh.addPrefix("pr list --state open --json number,headRefName,baseRefName,url,isDraft,labels --limit 200 --author someone", "[]")
	prs, err := DiscoverPRs(context.Background(), gh, ".", SourceConfig{
		Mode:   SourceModeAll,
		Filter: SourceFilter{Author: "someone"},
	})
	require.NoError(t, err)
	assert.Empty(t, prs)
	require.NotEmpty(t, gh.calls)
	last := gh.calls[len(gh.calls)-1]
	joined := strings.Join(last, " ")
	assert.Contains(t, joined, "--author someone")
}
