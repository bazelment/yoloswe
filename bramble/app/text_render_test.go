package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderTextContentSingleLineSkipsMarkdown(t *testing.T) {
	md, err := NewMarkdownRenderer(80, "dark")
	require.NoError(t, err)

	content := "I'll run `pwd`, `ls -1`, and `cat seed.txt` in that exact order now."
	got := renderTextContent(content, md, "  ")

	assert.Equal(t, "  "+content, got)
}

func TestRenderTextContentMultiLineUsesMarkdown(t *testing.T) {
	md, err := NewMarkdownRenderer(80, "dark")
	require.NoError(t, err)

	content := "- one\n- two"
	got := renderTextContent(content, md, "  ")

	assert.Contains(t, got, "one")
	assert.Contains(t, got, "two")
	assert.NotEqual(t, "  "+content, got)
}

func TestShouldRenderMarkdownText(t *testing.T) {
	assert.False(t, shouldRenderMarkdownText("single line"))
	assert.True(t, shouldRenderMarkdownText("line one\nline two"))
}

func TestRenderTextContentTrimsOuterNewlines(t *testing.T) {
	md, err := NewMarkdownRenderer(80, "dark")
	require.NoError(t, err)

	got := renderTextContent("\nhello\n", md, "  ")
	assert.Equal(t, "  hello", got)
}
