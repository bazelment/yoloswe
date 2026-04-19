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
