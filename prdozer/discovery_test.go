package prdozer

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/wt"
)

// concurrentGH returns a distinct PR-view response per PR number, so a goroutine
// that captures the wrong loop index would produce a visibly wrong result.
type concurrentGH struct {
	mu sync.Mutex
}

func (c *concurrentGH) Run(_ context.Context, args []string, _ string) (*wt.CmdResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	joined := strings.Join(args, " ")
	for _, n := range []int{10, 20, 30} {
		if strings.Contains(joined, fmt.Sprintf("pr view %d", n)) {
			return &wt.CmdResult{Stdout: fmt.Sprintf(
				`{"number":%d,"headRefName":"feat-%d","baseRefName":"main","url":"https://github.com/o/r/pull/%d","isDraft":false,"labels":[]}`,
				n, n, n,
			)}, nil
		}
	}
	return &wt.CmdResult{Stdout: "[]"}, nil
}

// labelListGH stubs `gh pr list` with a hard-coded response regardless of args,
// so tests can assert which PRs the Go filter keeps.
type labelListGH struct {
	resp string
}

func (l *labelListGH) Run(_ context.Context, args []string, _ string) (*wt.CmdResult, error) {
	if len(args) > 1 && args[0] == "pr" && args[1] == "list" {
		return &wt.CmdResult{Stdout: l.resp}, nil
	}
	return &wt.CmdResult{Stdout: "[]"}, nil
}

func TestDiscoverPRs_AllMode_LabelsAreANY(t *testing.T) {
	t.Parallel()
	// Three PRs: #1 has label-a, #2 has label-b, #3 has neither.
	resp := `[
		{"number":1,"headRefName":"f1","baseRefName":"main","url":"https://github.com/o/r/pull/1","isDraft":false,"labels":[{"name":"label-a"}]},
		{"number":2,"headRefName":"f2","baseRefName":"main","url":"https://github.com/o/r/pull/2","isDraft":false,"labels":[{"name":"label-b"}]},
		{"number":3,"headRefName":"f3","baseRefName":"main","url":"https://github.com/o/r/pull/3","isDraft":false,"labels":[{"name":"label-c"}]}
	]`
	gh := &labelListGH{resp: resp}
	out, err := DiscoverPRs(context.Background(), gh, ".", SourceConfig{
		Mode:   SourceModeAll,
		Filter: SourceFilter{Labels: []string{"label-a", "label-b"}},
	})
	require.NoError(t, err)
	// ANY semantics: #1 and #2 must both appear; #3 is filtered out.
	require.Len(t, out, 2)
	got := []int{out[0].Number, out[1].Number}
	assert.ElementsMatch(t, []int{1, 2}, got)
}

func TestDiscoverPRs_AllMode_ExcludeStillApplies(t *testing.T) {
	t.Parallel()
	resp := `[
		{"number":1,"headRefName":"f1","baseRefName":"main","url":"https://github.com/o/r/pull/1","isDraft":false,"labels":[{"name":"wip"}]},
		{"number":2,"headRefName":"f2","baseRefName":"main","url":"https://github.com/o/r/pull/2","isDraft":false,"labels":[]}
	]`
	gh := &labelListGH{resp: resp}
	out, err := DiscoverPRs(context.Background(), gh, ".", SourceConfig{
		Mode:   SourceModeAll,
		Filter: SourceFilter{ExcludeLabels: []string{"wip"}},
	})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, 2, out[0].Number)
}

func TestDiscoverPRs_ListModeFanOutPreservesOrderAndDistinctResults(t *testing.T) {
	t.Parallel()
	gh := &concurrentGH{}
	out, err := DiscoverPRs(context.Background(), gh, ".", SourceConfig{
		Mode: SourceModeList,
		PRs:  []int{10, 20, 30},
	})
	require.NoError(t, err)
	require.Len(t, out, 3)
	// Each index must match its input number (guards against goroutine loop-capture bug).
	assert.Equal(t, 10, out[0].Number)
	assert.Equal(t, 20, out[1].Number)
	assert.Equal(t, 30, out[2].Number)
	assert.Equal(t, "feat-10", out[0].HeadRefName)
	assert.Equal(t, "feat-20", out[1].HeadRefName)
	assert.Equal(t, "feat-30", out[2].HeadRefName)
}
