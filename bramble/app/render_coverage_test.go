package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/bazelment/yoloswe/bramble/session"
)

// TestRenderCoverage_AllOutputTypes verifies that formatOutputLineWithStyles
// produces non-empty, non-panicking output for every OutputLineType.
// This ensures no type falls through to the default case silently.
func TestRenderCoverage_AllOutputTypes(t *testing.T) {
	now := time.Now()
	styles := NewStyles(Dark)
	width := 100

	tests := []struct {
		name     string
		line     session.OutputLine
		contains string // expected substring in output
	}{
		{
			name:     "text",
			line:     session.OutputLine{Type: session.OutputTypeText, Content: "Hello world"},
			contains: "Hello world",
		},
		{
			name:     "thinking",
			line:     session.OutputLine{Type: session.OutputTypeThinking, Content: "Let me analyze this..."},
			contains: "💭",
		},
		{
			name:     "tool_start running",
			line:     session.OutputLine{Type: session.OutputTypeToolStart, ToolName: "Read", ToolID: "t1", ToolInput: map[string]interface{}{"file_path": "/foo.go"}, ToolState: session.ToolStateRunning, StartTime: now},
			contains: "🔧",
		},
		{
			name:     "tool_start complete",
			line:     session.OutputLine{Type: session.OutputTypeToolStart, ToolName: "Bash", ToolID: "t2", ToolInput: map[string]interface{}{"command": "ls"}, ToolState: session.ToolStateComplete, DurationMs: 1500, StartTime: now.Add(-2 * time.Second)},
			contains: "✓",
		},
		{
			name:     "tool_start error",
			line:     session.OutputLine{Type: session.OutputTypeToolStart, ToolName: "Write", ToolID: "t3", ToolInput: map[string]interface{}{"file_path": "/bad"}, ToolState: session.ToolStateError, DurationMs: 200, IsError: true},
			contains: "✗",
		},
		{
			name:     "tool (legacy)",
			line:     session.OutputLine{Type: session.OutputTypeTool, Content: "read_file /foo"},
			contains: "🔧",
		},
		{
			name:     "error",
			line:     session.OutputLine{Type: session.OutputTypeError, Content: "API error: FailedToOpenSocket", IsError: true},
			contains: "✗",
		},
		{
			name:     "status",
			line:     session.OutputLine{Type: session.OutputTypeStatus, Content: "── Context compacted ──"},
			contains: "→",
		},
		{
			name:     "turn_end",
			line:     session.OutputLine{Type: session.OutputTypeTurnEnd, TurnNumber: 3, CostUSD: 0.0235, DurationMs: 11000},
			contains: "Turn 3",
		},
		{
			name:     "turn_end with duration",
			line:     session.OutputLine{Type: session.OutputTypeTurnEnd, TurnNumber: 1, CostUSD: 0.01, DurationMs: 5000},
			contains: "5.0s",
		},
		{
			name:     "plan_ready",
			line:     session.OutputLine{Type: session.OutputTypePlanReady, Content: "# My Plan\n\n- Step 1\n- Step 2"},
			contains: "Plan Ready",
		},
		{
			name:     "plan_ready empty",
			line:     session.OutputLine{Type: session.OutputTypePlanReady, Content: ""},
			contains: "Plan Ready",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			formatted := formatOutputLineWithStyles(tc.line, width, styles)
			assert.NotEmpty(t, formatted, "output should not be empty for type %s", tc.line.Type)
			assert.Contains(t, formatted, tc.contains, "output should contain %q", tc.contains)
			t.Logf("  %s → %s", tc.line.Type, truncateForLog(formatted, 120))
		})
	}
}

// TestRenderCoverage_FullSessionFromFixture loads the full test fixture through
// the OutputModel and verifies it renders without panics.
func TestRenderCoverage_FullSessionFromFixture(t *testing.T) {
	// Build a representative set of output lines covering all types.
	now := time.Now()
	lines := []session.OutputLine{
		{Type: session.OutputTypeStatus, Content: "Session initialized", Timestamp: now},
		{Type: session.OutputTypeThinking, Content: "Analyzing the codebase...", Timestamp: now},
		{Type: session.OutputTypeText, Content: "I'll fix the authentication bug.", Timestamp: now},
		{Type: session.OutputTypeToolStart, ToolName: "Read", ToolID: "t1", ToolInput: map[string]interface{}{"file_path": "/login.go"}, ToolState: session.ToolStateComplete, DurationMs: 500, StartTime: now, Timestamp: now},
		{Type: session.OutputTypeToolStart, ToolName: "Edit", ToolID: "t2", ToolInput: map[string]interface{}{"file_path": "/login.go"}, ToolState: session.ToolStateComplete, DurationMs: 200, StartTime: now, Timestamp: now},
		{Type: session.OutputTypeText, Content: "Fixed the bug by adding password validation.", Timestamp: now},
		{Type: session.OutputTypeTurnEnd, TurnNumber: 1, CostUSD: 0.0235, DurationMs: 11000, Timestamp: now},
		{Type: session.OutputTypeError, Content: "API error: FailedToOpenSocket", IsError: true, Timestamp: now},
		{Type: session.OutputTypeStatus, Content: "PR #42: https://github.com/example/repo/pull/42", Timestamp: now},
		{Type: session.OutputTypeStatus, Content: "── Context compacted ──", Timestamp: now},
		{Type: session.OutputTypePlanReady, Content: "# Plan\n- Fix login\n- Add tests", Timestamp: now},
	}

	info := &session.SessionInfo{
		ID:     "test-coverage",
		Type:   session.SessionTypeBuilder,
		Status: session.StatusCompleted,
		Prompt: "Fix the authentication bug",
	}

	model := NewOutputModel(info, lines)
	model.SetSize(120, 40)

	view := model.View()
	assert.NotEmpty(t, view)
	assert.Contains(t, view, "test-coverage")
	assert.Contains(t, view, "Fix the authentication bug")

	// Verify key content is rendered.
	assert.Contains(t, view, "💭", "should render thinking icon")
	assert.Contains(t, view, "✓", "should render completed tool icon")
	assert.Contains(t, view, "Turn 1", "should render turn end")
	assert.Contains(t, view, "✗", "should render error icon")
	assert.Contains(t, view, "Plan Ready", "should render plan ready")
	assert.Contains(t, view, "PR #42", "should render PR link")

	t.Logf("Full rendered view:\n%s", view)
}

// TestRenderCoverage_AllOutputTypes_ViewPath exercises the view.go render path
// (Model.formatOutputLine) with the same set of output types as the output.go
// test above. Both paths must handle every OutputLineType — this catches drift.
func TestRenderCoverage_AllOutputTypes_ViewPath(t *testing.T) {
	now := time.Now()
	styles := NewStyles(Dark)
	m := Model{styles: styles}
	width := 100

	tests := []struct {
		name     string
		line     session.OutputLine
		contains string
	}{
		{
			name:     "text",
			line:     session.OutputLine{Type: session.OutputTypeText, Content: "Hello world"},
			contains: "Hello world",
		},
		{
			name:     "thinking",
			line:     session.OutputLine{Type: session.OutputTypeThinking, Content: "Let me analyze this..."},
			contains: "💭",
		},
		{
			name:     "tool_start running",
			line:     session.OutputLine{Type: session.OutputTypeToolStart, ToolName: "Read", ToolID: "t1", ToolInput: map[string]interface{}{"file_path": "/foo.go"}, ToolState: session.ToolStateRunning, StartTime: now},
			contains: "🔧",
		},
		{
			name:     "tool_start complete",
			line:     session.OutputLine{Type: session.OutputTypeToolStart, ToolName: "Bash", ToolID: "t2", ToolInput: map[string]interface{}{"command": "ls"}, ToolState: session.ToolStateComplete, DurationMs: 1500, StartTime: now.Add(-2 * time.Second)},
			contains: "✓",
		},
		{
			name:     "tool_start error",
			line:     session.OutputLine{Type: session.OutputTypeToolStart, ToolName: "Write", ToolID: "t3", ToolInput: map[string]interface{}{"file_path": "/bad"}, ToolState: session.ToolStateError, DurationMs: 200, IsError: true},
			contains: "✗",
		},
		{
			name:     "tool (legacy)",
			line:     session.OutputLine{Type: session.OutputTypeTool, Content: "read_file /foo"},
			contains: "🔧",
		},
		{
			name:     "error",
			line:     session.OutputLine{Type: session.OutputTypeError, Content: "API error: FailedToOpenSocket", IsError: true},
			contains: "✗",
		},
		{
			name:     "status",
			line:     session.OutputLine{Type: session.OutputTypeStatus, Content: "── Context compacted ──"},
			contains: "→",
		},
		{
			name:     "turn_end",
			line:     session.OutputLine{Type: session.OutputTypeTurnEnd, TurnNumber: 3, CostUSD: 0.0235, DurationMs: 11000},
			contains: "Turn 3",
		},
		{
			name:     "plan_ready",
			line:     session.OutputLine{Type: session.OutputTypePlanReady, Content: "# My Plan\n\n- Step 1\n- Step 2"},
			contains: "Plan Ready",
		},
		{
			name:     "plan_ready empty",
			line:     session.OutputLine{Type: session.OutputTypePlanReady, Content: ""},
			contains: "Plan Ready",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			formatted := m.formatOutputLine(tc.line, width)
			assert.NotEmpty(t, formatted, "view-path output should not be empty for type %s", tc.line.Type)
			assert.Contains(t, formatted, tc.contains, "view-path output should contain %q", tc.contains)
			t.Logf("  %s → %s", tc.line.Type, truncateForLog(formatted, 120))
		})
	}
}

func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
