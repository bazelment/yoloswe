package app

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/assert"
)

func TestStripAnsiWidth_Emoji(t *testing.T) {
	// Emoji is 2 display columns wide
	s := "📋 planner"
	stripped := stripAnsi(s)
	width := runewidth.StringWidth(stripped)
	// 📋 is 2 columns, space is 1, "planner" is 7 = 10 total
	assert.Equal(t, 10, width)
	// Byte length is different from display width
	assert.NotEqual(t, len(stripped), width)
}

func TestStripAnsiWidth_CJK(t *testing.T) {
	// CJK characters are 2 display columns wide
	s := "Hello 世界"
	width := runewidth.StringWidth(s)
	// "Hello " is 6, "世" is 2, "界" is 2 = 10 total
	assert.Equal(t, 10, width)
}

func TestTruncateVisual_Emoji(t *testing.T) {
	s := "📋 planner session"
	result := truncateVisual(s, 10)
	// Should truncate to ~7 visual columns + "..."
	visualWidth := runewidth.StringWidth(stripAnsi(result))
	assert.LessOrEqual(t, visualWidth, 10)
	assert.True(t, strings.HasSuffix(result, "..."))
}

func TestTruncateVisual_ANSI(t *testing.T) {
	// ANSI-styled string: the escape codes should not count toward width
	s := "\x1b[32mGreen text\x1b[0m"
	result := truncateVisual(s, 8)
	// Visual width of result (excluding ANSI) should be <= 8
	visualWidth := runewidth.StringWidth(stripAnsi(result))
	assert.LessOrEqual(t, visualWidth, 8)
}

func TestPadToSize_WideChars(t *testing.T) {
	content := "📋 plan"
	result := padToSize(content, 20, 1)
	lines := strings.Split(result, "\n")
	// Each line should have visual width of exactly 20
	stripped := stripAnsi(lines[0])
	width := runewidth.StringWidth(stripped)
	assert.Equal(t, 20, width)
}

func TestTopBarPadding_Emoji(t *testing.T) {
	// Integration test: ensure top bar doesn't overflow with emoji
	// This is a basic sanity check - full test would need a running Model
	left := "📋 Test Session"
	right := "[Alt-W]"

	width := 80
	leftWidth := runewidth.StringWidth(stripAnsi(left))
	rightWidth := runewidth.StringWidth(stripAnsi(right))
	padding := width - leftWidth - rightWidth - 4

	// Padding should be reasonable (not negative or huge)
	assert.Greater(t, padding, 0)
	assert.Less(t, padding, width)
}
