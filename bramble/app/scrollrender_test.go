package app

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeLines(n int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("  line %d", i)
	}
	return lines
}

func TestRenderScrollableLines_FitsWithoutScroll(t *testing.T) {
	lines := makeLines(5)
	result := renderScrollableLines(lines, 10, 0, NewStyles(DefaultDark))
	// All 5 lines should appear, no indicators
	for _, l := range lines {
		assert.Contains(t, result, l)
	}
	assert.NotContains(t, result, "more lines")
}

func TestRenderScrollableLines_AtBottom_NoIndicators(t *testing.T) {
	lines := makeLines(20)
	result := renderScrollableLines(lines, 10, 0, NewStyles(DefaultDark))
	// Should show last 10 lines, no scroll indicators
	assert.Contains(t, result, "line 19")
	assert.Contains(t, result, "line 10")
	assert.NotContains(t, result, "line 9")
	assert.NotContains(t, result, "\u2191") // no up arrow
	assert.NotContains(t, result, "\u2193") // no down arrow
}

func TestRenderScrollableLines_ScrolledMiddle_BothIndicators(t *testing.T) {
	lines := makeLines(30)
	result := renderScrollableLines(lines, 10, 10, NewStyles(DefaultDark))
	// Should have both up and down indicators
	assert.Contains(t, result, "\u2191")
	assert.Contains(t, result, "\u2193")
	assert.Contains(t, result, "more lines")
}

func TestRenderScrollableLines_ScrolledToTop_OnlyDownIndicator(t *testing.T) {
	lines := makeLines(30)
	// Scroll far enough to reach top
	result := renderScrollableLines(lines, 10, 999, NewStyles(DefaultDark))
	assert.Contains(t, result, "line 0")
	assert.NotContains(t, result, "\u2191") // no up arrow at top
	assert.Contains(t, result, "\u2193")    // down arrow present
}

func TestRenderScrollableLines_EmptyLines(t *testing.T) {
	result := renderScrollableLines(nil, 10, 0, NewStyles(DefaultDark))
	assert.Equal(t, "", result)
}

func TestRenderScrollableLines_ScrollClamped(t *testing.T) {
	lines := makeLines(5)
	// scrollOffset larger than content; should clamp and not panic
	result := renderScrollableLines(lines, 10, 100, NewStyles(DefaultDark))
	require.NotEmpty(t, result)
	// Should show line 0 since we're clamped to top
	assert.Contains(t, result, "line 0")
	// All content fits in viewport, so no "0 more lines" indicator should appear
	assert.NotContains(t, result, "0 more lines")
	assert.NotContains(t, result, "\u2193") // no down arrow when content fits
}

func TestRenderScrollableLines_HeightOne(t *testing.T) {
	lines := makeLines(10)
	// Edge case: only 1 line of display height
	result := renderScrollableLines(lines, 1, 0, NewStyles(DefaultDark))
	require.NotEmpty(t, result)
	// Should show at least 1 line, no panic
	lineCount := strings.Count(result, "\n")
	assert.GreaterOrEqual(t, lineCount, 1)
}
