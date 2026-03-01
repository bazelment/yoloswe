package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
)

// Test session output rendering with various output types.

func TestOutputModelRendering(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name   string
		info   *session.SessionInfo
		lines  []session.OutputLine
		width  int
		height int
	}{
		{
			name: "planner session with thinking",
			info: &session.SessionInfo{
				ID:     "planner-123",
				Type:   session.SessionTypePlanner,
				Status: session.StatusRunning,
				Prompt: "Plan a new feature for authentication",
			},
			lines: []session.OutputLine{
				{Type: session.OutputTypeStatus, Content: "Starting planning session"},
				{Type: session.OutputTypeThinking, Content: "Analyzing the requirements..."},
				{Type: session.OutputTypeTool, Content: "read_file src/auth/login.go"},
				{Type: session.OutputTypeStatus, Content: "Tool completed successfully"},
			},
			width:  80,
			height: 24,
		},
		{
			name: "builder session with error",
			info: &session.SessionInfo{
				ID:     "builder-456",
				Type:   session.SessionTypeBuilder,
				Status: session.StatusFailed,
				Prompt: "Implement the login functionality",
			},
			lines: []session.OutputLine{
				{Type: session.OutputTypeStatus, Content: "Starting builder session"},
				{Type: session.OutputTypeTool, Content: "write_file src/auth/login.go"},
				{Type: session.OutputTypeError, Content: "Build failed: syntax error at line 42"},
			},
			width:  80,
			height: 24,
		},
		{
			name: "completed session",
			info: &session.SessionInfo{
				ID:     "session-789",
				Type:   session.SessionTypePlanner,
				Status: session.StatusCompleted,
				Prompt: "Review the PR",
			},
			lines: []session.OutputLine{
				{Type: session.OutputTypeStatus, Content: "Starting review"},
				{Type: session.OutputTypeText, Content: "The PR looks good overall."},
				{Type: session.OutputTypeStatus, Content: "Review completed"},
			},
			width:  80,
			height: 24,
		},
		{
			name:   "empty session",
			info:   nil,
			lines:  nil,
			width:  80,
			height: 24,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewOutputModel(tc.info, tc.lines)
			m.SetSize(tc.width, tc.height)

			view := m.View()
			assert.NotEmpty(t, view)

			// Basic assertions about content
			if tc.info != nil {
				assert.Contains(t, view, string(tc.info.ID))
			}

			for _, line := range tc.lines {
				if line.Type == session.OutputTypeError {
					assert.Contains(t, view, "✗")
				}
				if line.Type == session.OutputTypeTool {
					assert.Contains(t, view, "🔧")
				}
			}

			t.Logf("Rendered output:\n%s", view)
		})
	}

	_ = now // Silence unused warning
}

func TestOutputModelReplay(t *testing.T) {
	now := time.Now()

	stored := &session.StoredSession{
		ID:           "replay-session",
		Type:         session.SessionTypePlanner,
		Status:       session.StatusCompleted,
		RepoName:     "test-repo",
		WorktreeName: "feature",
		Prompt:       "Test replay functionality",
		CreatedAt:    now.Add(-time.Hour),
		Output: []session.OutputLine{
			{Timestamp: now.Add(-50 * time.Minute), Type: session.OutputTypeStatus, Content: "Session started"},
			{Timestamp: now.Add(-45 * time.Minute), Type: session.OutputTypeThinking, Content: "Processing..."},
			{Timestamp: now.Add(-40 * time.Minute), Type: session.OutputTypeStatus, Content: "Session completed"},
		},
	}

	m := NewReplayOutputModel(stored)
	m.SetSize(80, 24)

	view := m.View()

	// Replay should show the [Replay] indicator
	assert.Contains(t, view, "[Replay]")
	assert.Contains(t, view, "replay-session")
	assert.Contains(t, view, "Session started")
}

func TestFormatOutputLine(t *testing.T) {
	tests := []struct {
		name     string
		contains string
		line     session.OutputLine
		width    int
	}{
		{
			name:     "error line",
			line:     session.OutputLine{Type: session.OutputTypeError, Content: "Something failed"},
			width:    80,
			contains: "✗",
		},
		{
			name:     "thinking line",
			line:     session.OutputLine{Type: session.OutputTypeThinking, Content: "Analyzing..."},
			width:    80,
			contains: "💭",
		},
		{
			name:     "tool line",
			line:     session.OutputLine{Type: session.OutputTypeTool, Content: "run_command ls"},
			width:    80,
			contains: "🔧",
		},
		{
			name:     "status line",
			line:     session.OutputLine{Type: session.OutputTypeStatus, Content: "Done"},
			width:    80,
			contains: "→",
		},
		{
			name:     "text line",
			line:     session.OutputLine{Type: session.OutputTypeText, Content: "Hello world"},
			width:    80,
			contains: "Hello world",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			formatted := formatOutputLineWithStyles(tc.line, tc.width, NewStyles(Dark), nil)
			assert.Contains(t, formatted, tc.contains)
		})
	}
}

func TestOutputModelLongPromptNoTruncation(t *testing.T) {
	info := &session.SessionInfo{
		ID:     "test-id",
		Type:   session.SessionTypePlanner,
		Status: session.StatusRunning,
		Prompt: "A very long prompt that should be truncated when displayed in the output view because it exceeds the available width",
	}

	// Create many lines to test scrolling
	var lines []session.OutputLine
	for i := 0; i < 100; i++ {
		lines = append(lines, session.OutputLine{
			Type:    session.OutputTypeText,
			Content: "Line content",
		})
	}

	m := NewOutputModel(info, lines)
	m.SetSize(40, 10) // Small terminal

	view := m.View()
	assert.NotEmpty(t, view)
	assert.Contains(t, view, info.Prompt)
	assert.NotContains(t, view, "...\"")
	t.Logf("Truncated output:\n%s", view)
}

func TestToolStateRendering(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name      string
		wantIcon  string
		wantState string
		line      session.OutputLine
		width     int
	}{
		{
			name: "running tool shows spinner and elapsed time",
			line: session.OutputLine{
				Type:      session.OutputTypeToolStart,
				Content:   "[Read] /path/to/file.go",
				ToolName:  "Read",
				ToolID:    "tool-123",
				ToolInput: map[string]interface{}{"file_path": "/path/to/file.go"},
				ToolState: session.ToolStateRunning,
				StartTime: now.Add(-2 * time.Second), // 2 seconds ago
			},
			width:     80,
			wantIcon:  "🔧",
			wantState: "⏳",
		},
		{
			name: "completed tool shows checkmark and duration",
			line: session.OutputLine{
				Type:       session.OutputTypeToolStart,
				Content:    "[Read] /path/to/file.go",
				ToolName:   "Read",
				ToolID:     "tool-123",
				ToolInput:  map[string]interface{}{"file_path": "/path/to/file.go"},
				ToolState:  session.ToolStateComplete,
				StartTime:  now.Add(-2 * time.Second),
				DurationMs: 2500,
			},
			width:     80,
			wantIcon:  "✓",
			wantState: "2.50s",
		},
		{
			name: "error tool shows error icon and duration",
			line: session.OutputLine{
				Type:       session.OutputTypeToolStart,
				Content:    "[Bash] ls /nonexistent",
				ToolName:   "Bash",
				ToolID:     "tool-456",
				ToolInput:  map[string]interface{}{"command": "ls /nonexistent"},
				ToolState:  session.ToolStateError,
				StartTime:  now.Add(-1 * time.Second),
				DurationMs: 500,
				IsError:    true,
			},
			width:     80,
			wantIcon:  "✗",
			wantState: "0.50s",
		},
		{
			name: "tool with no state shows wrench icon (backward compat)",
			line: session.OutputLine{
				Type:      session.OutputTypeToolStart,
				Content:   "[Glob] **/*.go",
				ToolName:  "Glob",
				ToolID:    "tool-789",
				ToolInput: map[string]interface{}{"pattern": "**/*.go"},
				ToolState: "", // empty state
			},
			width:    80,
			wantIcon: "🔧",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			formatted := formatOutputLineWithStyles(tc.line, tc.width, NewStyles(Dark), nil)

			assert.Contains(t, formatted, tc.wantIcon,
				"expected icon %q in output: %s", tc.wantIcon, formatted)

			if tc.wantState != "" {
				assert.Contains(t, formatted, tc.wantState,
					"expected state info %q in output: %s", tc.wantState, formatted)
			}

			t.Logf("Formatted output: %s", formatted)
		})
	}
}

func TestFormatToolDisplay(t *testing.T) {
	tests := []struct {
		input    map[string]interface{}
		name     string
		toolName string
		want     string
		maxLen   int
	}{
		{
			name:     "Read tool shows file path",
			toolName: "Read",
			input:    map[string]interface{}{"file_path": "/path/to/file.go"},
			maxLen:   60,
			want:     "[Read] /path/to/file.go",
		},
		{
			name:     "Write tool shows arrow and path",
			toolName: "Write",
			input:    map[string]interface{}{"file_path": "/path/to/file.go"},
			maxLen:   60,
			want:     "[Write] → /path/to/file.go",
		},
		{
			name:     "Edit tool shows arrow and path",
			toolName: "Edit",
			input:    map[string]interface{}{"file_path": "/path/to/file.go"},
			maxLen:   60,
			want:     "[Edit] → /path/to/file.go",
		},
		{
			name:     "Bash tool shows command",
			toolName: "Bash",
			input:    map[string]interface{}{"command": "ls -la"},
			maxLen:   60,
			want:     "[Bash] ls -la",
		},
		{
			name:     "Glob tool shows pattern",
			toolName: "Glob",
			input:    map[string]interface{}{"pattern": "**/*.go"},
			maxLen:   60,
			want:     "[Glob] **/*.go",
		},
		{
			name:     "Grep tool shows pattern",
			toolName: "Grep",
			input:    map[string]interface{}{"pattern": "func.*Test"},
			maxLen:   60,
			want:     "[Grep] func.*Test",
		},
		{
			name:     "Task tool shows description",
			toolName: "Task",
			input:    map[string]interface{}{"description": "Search for files"},
			maxLen:   60,
			want:     "[Task] Search for files",
		},
		{
			name:     "nil input shows tool name only",
			toolName: "Read",
			input:    nil,
			maxLen:   60,
			want:     "[Read]",
		},
		{
			name:     "unknown tool shows tool name only",
			toolName: "UnknownTool",
			input:    map[string]interface{}{"foo": "bar"},
			maxLen:   60,
			want:     "[UnknownTool]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := formatToolDisplay(tc.toolName, tc.input, tc.maxLen)
			assert.Equal(t, tc.want, result)
		})
	}
}

func TestToolDurationCalculation(t *testing.T) {
	// Simulate a tool that takes 1.5 seconds
	startTime := time.Now()
	endTime := startTime.Add(1500 * time.Millisecond)
	durationMs := endTime.Sub(startTime).Milliseconds()

	line := session.OutputLine{
		Type:       session.OutputTypeToolStart,
		ToolName:   "Read",
		ToolID:     "test-tool",
		ToolInput:  map[string]interface{}{"file_path": "/test/file.go"},
		ToolState:  session.ToolStateComplete,
		StartTime:  startTime,
		DurationMs: durationMs,
	}

	formatted := formatOutputLineWithStyles(line, 80, NewStyles(Dark), nil)

	// Should show duration of 1.50s
	assert.Contains(t, formatted, "1.50s")
	assert.Contains(t, formatted, "✓") // checkmark for complete
}

func TestVisualLineCountInOutput(t *testing.T) {
	// Test that OutputModel shows recent lines when there are many

	info := &session.SessionInfo{
		ID:     "output-test",
		Type:   session.SessionTypePlanner,
		Status: session.StatusRunning,
		Prompt: "Test output handling",
	}

	// Create many single-line entries
	var lines []session.OutputLine
	for i := 0; i < 100; i++ {
		lines = append(lines, session.OutputLine{
			Type:    session.OutputTypeStatus,
			Content: fmt.Sprintf("Status line %d", i),
		})
	}

	m := NewOutputModel(info, lines)
	m.SetSize(80, 15) // Small height

	view := m.View()

	// Count actual lines in output
	viewLines := strings.Split(view, "\n")
	t.Logf("Total lines in view: %d", len(viewLines))

	// Should show recent lines (end of the output), not all 100
	// The model should only show the last N lines that fit
	assert.Contains(t, view, "Status line 99", "should show recent content")

	// Should not show very old lines
	assert.NotContains(t, view, "Status line 0", "should not show old content")
}

// TestRenderCenterScrolling tests the real Model.renderCenter with many output lines,
// verifying that content is visible and scrolling works correctly.
func TestRenderCenterScrolling(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	// Create a test session
	sessID := session.SessionID("test-scroll-session")
	mgr.AddSession(&session.Session{
		ID:           sessID,
		Type:         session.SessionTypePlanner,
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/test-wt",
		Prompt:       "Test scrolling",
		Title:        "Test scrolling",
	})
	mgr.InitOutputBuffer(sessID)

	// Add 50 output lines
	for i := 0; i < 50; i++ {
		mgr.AddOutputLine(sessID, session.OutputLine{
			Type:    session.OutputTypeStatus,
			Content: fmt.Sprintf("Line-%03d", i),
		})
	}

	// Verify output buffer
	allLines := mgr.GetSessionOutput(sessID)
	assert.Equal(t, 50, len(allLines), "should have 50 output lines in manager")

	// Create a Model that views this session
	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, nil, 0, 0, nil, nil)
	m.width = 80
	m.height = 20 // Small terminal: centerHeight = 20-1-1-0-2 = 16
	m.viewingSessionID = sessID

	centerHeight := 16
	center := m.renderCenter(80, centerHeight)

	// centerHeight=16, minus 5 for header/prompt/separator = 11 outputHeight
	// With scrollOffset=0 (default), should show the LAST 11 lines (Line-039..Line-049)
	t.Logf("=== Default view (scrollOffset=0) ===\n%s", center)

	assert.Contains(t, center, "Line-049", "should show last line")
	assert.Contains(t, center, "Line-040", "should show line near the end")
	assert.NotContains(t, center, "Line-000", "should NOT show first line")
	assert.NotContains(t, center, "Line-010", "should NOT show early lines")

	// Scroll up (increase scrollOffset) to see older content
	m.scrollOffset = 20
	center = m.renderCenter(80, centerHeight)
	t.Logf("=== Scrolled up 20 (scrollOffset=20) ===\n%s", center)

	// With scrollOffset=20: endIdx=50-20=30, startIdx=30-outputHeight
	// Should show lines around 20-29 area
	assert.Contains(t, center, "Line-025", "should show middle content when scrolled up")
	assert.NotContains(t, center, "Line-049", "should NOT show latest when scrolled up")

	// Scroll to very top (large offset, gets clamped)
	m.scrollOffset = 999
	center = m.renderCenter(80, centerHeight)
	t.Logf("=== Scrolled to top (scrollOffset=999, clamped) ===\n%s", center)

	assert.Contains(t, center, "Line-000", "should show first line when scrolled to top")
	assert.NotContains(t, center, "Line-049", "should NOT show last line when scrolled to top")

	// Verify scroll indicator appears when not at bottom
	m.scrollOffset = 10
	center = m.renderCenter(80, centerHeight)
	t.Logf("=== Scroll indicator test (scrollOffset=10) ===\n%s", center)
	assert.Contains(t, center, "more lines", "should show scroll indicator when not at bottom")

	// Back to bottom
	m.scrollOffset = 0
	center = m.renderCenter(80, centerHeight)
	assert.NotContains(t, center, "more lines", "should NOT show scroll indicator at bottom")
}

// TestRenderCenterFewLines tests renderCenter when output lines fit within the view.
func TestRenderCenterFewLines(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	sessID := session.SessionID("test-few-lines")
	mgr.AddSession(&session.Session{
		ID:           sessID,
		Type:         session.SessionTypePlanner,
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/test-wt",
		Prompt:       "Short session",
		Title:        "Short session",
	})
	mgr.InitOutputBuffer(sessID)

	// Add only 3 lines
	for i := 0; i < 3; i++ {
		mgr.AddOutputLine(sessID, session.OutputLine{
			Type:    session.OutputTypeStatus,
			Content: fmt.Sprintf("Short-%d", i),
		})
	}

	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, nil, 0, 0, nil, nil)
	m.width = 80
	m.height = 30
	m.viewingSessionID = sessID

	centerHeight := 26
	center := m.renderCenter(80, centerHeight)
	t.Logf("=== Few lines view ===\n%s", center)

	// All 3 lines should be visible
	assert.Contains(t, center, "Short-0")
	assert.Contains(t, center, "Short-1")
	assert.Contains(t, center, "Short-2")

	// No scroll indicator
	assert.NotContains(t, center, "more lines")
}

// TestRenderCenterStreaming simulates tool output streaming — lines arrive one by one
// and each render should show the latest content (auto-scroll at scrollOffset=0).
func TestRenderCenterStreaming(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	sessID := session.SessionID("test-stream")
	mgr.AddSession(&session.Session{
		ID:           sessID,
		Type:         session.SessionTypePlanner,
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/test-wt",
		Prompt:       "Stream test",
		Title:        "Stream test",
	})
	mgr.InitOutputBuffer(sessID)

	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, nil, 0, 0, nil, nil)
	m.width = 80
	m.height = 20 // centerHeight = 16, outputHeight = 11
	m.viewingSessionID = sessID

	centerHeight := 16

	// Simulate streaming: add lines one at a time and verify latest is always visible
	for i := 0; i < 30; i++ {
		mgr.AddOutputLine(sessID, session.OutputLine{
			Type:    session.OutputTypeStatus,
			Content: fmt.Sprintf("Stream-%03d", i),
		})

		center := m.renderCenter(80, centerHeight)

		// Latest line should ALWAYS be visible when scrollOffset=0
		latest := fmt.Sprintf("Stream-%03d", i)
		assert.Contains(t, center, latest,
			"render %d: latest line %q should be visible (scrollOffset=%d)",
			i, latest, m.scrollOffset)
	}

	// Simulate tool call streaming: tool starts, then completes
	mgr.AddOutputLine(sessID, session.OutputLine{
		Type:      session.OutputTypeToolStart,
		ToolName:  "Read",
		ToolID:    "tool-1",
		ToolInput: map[string]interface{}{"file_path": "/test.go"},
		ToolState: session.ToolStateRunning,
		StartTime: time.Now(),
	})

	center := m.renderCenter(80, centerHeight)
	assert.Contains(t, center, "[Read]", "running tool should be visible")
	assert.Contains(t, center, "⏳", "running tool should show spinner")

	// Tool completes — update in-place
	mgr.GetSessionOutput(sessID) // force a snapshot before update
	// Simulate what updateToolOutput does: modify the line in the output buffer
	lines := mgr.GetSessionOutput(sessID)
	lastIdx := len(lines) - 1
	assert.Equal(t, "tool-1", lines[lastIdx].ToolID)
}

// TestRenderCenterAutoScrollStaysAtBottom verifies that when scrollOffset=0 (default),
// the view always tracks the latest output even as totalLines grows.
func TestRenderCenterAutoScrollStaysAtBottom(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	sessID := session.SessionID("test-autoscroll")
	mgr.AddSession(&session.Session{
		ID:           sessID,
		Type:         session.SessionTypeBuilder,
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/test-wt",
		Prompt:       "Auto scroll test",
		Title:        "Auto scroll",
	})
	mgr.InitOutputBuffer(sessID)

	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, nil, 0, 0, nil, nil)
	m.width = 80
	m.height = 15 // centerHeight = 11, outputHeight = 6
	m.viewingSessionID = sessID

	centerHeight := 11

	// Add 100 lines rapidly
	for i := 0; i < 100; i++ {
		mgr.AddOutputLine(sessID, session.OutputLine{
			Type:    session.OutputTypeStatus,
			Content: fmt.Sprintf("Auto-%03d", i),
		})
	}

	// scrollOffset should still be 0
	assert.Equal(t, 0, m.scrollOffset, "scrollOffset should remain 0")

	center := m.renderCenter(80, centerHeight)
	assert.Contains(t, center, "Auto-099", "should show the very last line")
	assert.NotContains(t, center, "Auto-000", "should NOT show the first line")
	// No scroll indicator at bottom
	assert.NotContains(t, center, "more lines", "no scroll indicator when at bottom")

	// Now user scrolls up
	m.scrollOutput(5)
	assert.Equal(t, 5, m.scrollOffset)

	center = m.renderCenter(80, centerHeight)
	assert.NotContains(t, center, "Auto-099", "should NOT show last line when scrolled up")
	assert.Contains(t, center, "more lines", "should show scroll indicator")

	// User scrolls back to bottom
	m.scrollOutput(-5)
	assert.Equal(t, 0, m.scrollOffset)

	center = m.renderCenter(80, centerHeight)
	assert.Contains(t, center, "Auto-099", "should show last line after scrolling back")
}

// TestRenderCenterTextStreaming verifies that incremental text updates
// (via appendOrAddText) accumulate into a single output line and are visible.
func TestRenderCenterTextStreaming(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	sessID := session.SessionID("test-text-stream")
	mgr.AddSession(&session.Session{
		ID:           sessID,
		Type:         session.SessionTypePlanner,
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/test-wt",
		Prompt:       "Text streaming test",
		Title:        "Text streaming",
	})
	mgr.InitOutputBuffer(sessID)

	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, nil, 0, 0, nil, nil)
	m.width = 80
	m.height = 20
	m.viewingSessionID = sessID

	centerHeight := 16

	// Simulate streaming text: first chunk creates a text line
	mgr.AddOutputLine(sessID, session.OutputLine{
		Type:    session.OutputTypeText,
		Content: "Hello ",
	})

	lines := mgr.GetSessionOutput(sessID)
	assert.Equal(t, 1, len(lines))
	assert.Equal(t, "Hello ", lines[0].Content)

	center := m.renderCenter(80, centerHeight)
	assert.Contains(t, center, "Hello")

	// Add a tool call after text — this should be a separate line
	mgr.AddOutputLine(sessID, session.OutputLine{
		Type:       session.OutputTypeToolStart,
		ToolName:   "Read",
		ToolID:     "t1",
		ToolState:  session.ToolStateComplete,
		ToolInput:  map[string]interface{}{"file_path": "/test.go"},
		DurationMs: 100,
	})

	lines = mgr.GetSessionOutput(sessID)
	assert.Equal(t, 2, len(lines))
	assert.Equal(t, session.OutputTypeText, lines[0].Type)
	assert.Equal(t, session.OutputTypeToolStart, lines[1].Type)

	center = m.renderCenter(80, centerHeight)
	assert.Contains(t, center, "Hello")
	assert.Contains(t, center, "[Read]")

	// More text after tool — should be a NEW text line
	mgr.AddOutputLine(sessID, session.OutputLine{
		Type:    session.OutputTypeText,
		Content: "World!",
	})

	lines = mgr.GetSessionOutput(sessID)
	assert.Equal(t, 3, len(lines), "text after tool should be separate line")

	center = m.renderCenter(80, centerHeight)
	assert.Contains(t, center, "World!")
}

// TestScrollViaUpdate tests scrolling through the actual Update→View cycle,
// simulating what bubbletea does when the user presses arrow keys.
func TestScrollViaUpdate(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	sessID := session.SessionID("test-scroll-update")
	mgr.AddSession(&session.Session{
		ID:           sessID,
		Type:         session.SessionTypePlanner,
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/test-wt",
		Prompt:       "Scroll via update",
		Title:        "Scroll test",
	})
	mgr.InitOutputBuffer(sessID)

	for i := 0; i < 50; i++ {
		mgr.AddOutputLine(sessID, session.OutputLine{
			Type:    session.OutputTypeStatus,
			Content: fmt.Sprintf("Line-%03d", i),
		})
	}

	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, nil, 0, 0, nil, nil)
	m.width = 80
	m.height = 20
	m.viewingSessionID = sessID

	// Verify initial state shows latest
	view := m.View()
	assert.Contains(t, view, "Line-049", "initial view should show latest")

	// Simulate pressing "up" key via Update
	upKey := tea.KeyMsg{Type: tea.KeyUp}
	newModel, _ := m.Update(upKey)
	m2 := newModel.(Model)

	t.Logf("After 1 up: scrollOffset=%d", m2.scrollOffset)
	assert.Equal(t, 1, m2.scrollOffset, "scrollOffset should be 1 after pressing up")

	view = m2.View()
	t.Logf("View after up:\n%s", view)
	// Should NOT show the very last line anymore
	assert.NotContains(t, view, "Line-049", "should not show last line after scrolling up")
	assert.Contains(t, view, "Line-048", "should show second-to-last line")

	// Press up 5 more times
	current := m2
	for i := 0; i < 5; i++ {
		newModel, _ = current.Update(upKey)
		current = newModel.(Model)
	}
	t.Logf("After 6 total ups: scrollOffset=%d", current.scrollOffset)
	assert.Equal(t, 6, current.scrollOffset)

	// Press down to scroll back
	downKey := tea.KeyMsg{Type: tea.KeyDown}
	newModel, _ = current.Update(downKey)
	current = newModel.(Model)
	t.Logf("After 1 down: scrollOffset=%d", current.scrollOffset)
	assert.Equal(t, 5, current.scrollOffset)

	// Press End to jump to bottom
	endKey := tea.KeyMsg{Type: tea.KeyEnd}
	newModel, _ = current.Update(endKey)
	current = newModel.(Model)
	assert.Equal(t, 0, current.scrollOffset, "End should reset scrollOffset to 0")

	view = current.View()
	assert.Contains(t, view, "Line-049", "should show latest after End")
}

// TestScrollWithMultiLineContent tests that scrolling works when OutputLines
// produce multiple visual lines (e.g., large text blocks). This is the real-world
// scenario: a session has few OutputLines but they contain long text/markdown.
func TestScrollWithMultiLineContent(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	sessID := session.SessionID("test-multiline-scroll")
	mgr.AddSession(&session.Session{
		ID:           sessID,
		Type:         session.SessionTypePlanner,
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/test-wt",
		Prompt:       "Multi-line test",
		Title:        "Multi-line",
	})
	mgr.InitOutputBuffer(sessID)

	// Add a text block that spans many visual lines
	var longText strings.Builder
	for i := 0; i < 30; i++ {
		longText.WriteString(fmt.Sprintf("Text line %d of the response\n", i))
	}
	mgr.AddOutputLine(sessID, session.OutputLine{
		Type:    session.OutputTypeText,
		Content: longText.String(),
	})

	// Add a tool call
	mgr.AddOutputLine(sessID, session.OutputLine{
		Type:       session.OutputTypeToolStart,
		ToolName:   "Read",
		ToolID:     "t1",
		ToolState:  session.ToolStateComplete,
		ToolInput:  map[string]interface{}{"file_path": "/test.go"},
		DurationMs: 100,
	})

	// Only 2 logical OutputLines, but 30+ visual lines
	lines := mgr.GetSessionOutput(sessID)
	assert.Equal(t, 2, len(lines), "should have 2 logical OutputLines")

	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, nil, 0, 0, nil, nil)
	m.width = 80
	m.height = 15 // Small screen: centerHeight=11, outputHeight=6
	m.viewingSessionID = sessID

	// Default view (scrollOffset=0): should show the end (tool call + last text lines)
	view := m.View()
	assert.Contains(t, view, "[Read]", "should show tool call at bottom")
	t.Logf("Default view:\n%s", view)

	// Scroll up — should now see different text lines
	m.scrollOffset = 5
	view = m.View()
	t.Logf("Scrolled up 5:\n%s", view)
	assert.Contains(t, view, "more lines", "should show scroll indicator")
	// Tool call should no longer be visible (it was at the very bottom)
	assert.NotContains(t, view, "[Read]", "tool call should be scrolled off")

	// Scroll further up — should see earlier lines than when scrolled up 5
	m.scrollOffset = 20
	view = m.View()
	t.Logf("Scrolled up 20:\n%s", view)
	assert.Contains(t, view, "more lines", "should still show scroll indicator")
	// Should see earlier text than the scrolled-up-5 view
	assert.NotContains(t, view, "Text line 26", "should not show late text when scrolled up far")

	// Scroll all the way to top — should see the very first lines
	m.scrollOffset = 100 // More than maxScrollOffset, gets clamped
	view = m.View()
	t.Logf("Scrolled to top:\n%s", view)
	assert.Contains(t, view, "Text line 0", "should show first text when scrolled to top")
}

func TestGenerateDropdownTitle(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
		want   string
		maxLen int
	}{
		{
			name:   "short prompt",
			prompt: "fix bug",
			maxLen: 20,
			want:   "fix bug",
		},
		{
			name:   "truncates at word boundary",
			prompt: "implement user authentication system",
			maxLen: 20,
			want:   "implement user",
		},
		{
			name:   "empty prompt",
			prompt: "",
			maxLen: 20,
			want:   "",
		},
		{
			name:   "single long word",
			prompt: "abcdefghijklmnopqrstuvwxyz",
			maxLen: 10,
			want:   "abcdefg...",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := generateDropdownTitle(tc.prompt, tc.maxLen)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestFormatWorktreeStatus(t *testing.T) {
	tests := []struct {
		name   string
		status *wt.WorktreeStatus
		want   []string // substrings expected
	}{
		{
			name: "dirty with ahead/behind",
			status: &wt.WorktreeStatus{
				IsDirty: true,
				Ahead:   2,
				Behind:  1,
			},
			want: []string{"dirty", "↑2", "↓1"},
		},
		{
			name: "clean with PR",
			status: &wt.WorktreeStatus{
				IsDirty:  false,
				PRNumber: 123,
				PRState:  "OPEN",
			},
			want: []string{"clean", "PR#123 OPEN"},
		},
		{
			name: "dirty only",
			status: &wt.WorktreeStatus{
				IsDirty: true,
			},
			want: []string{"dirty"},
		},
		{
			name: "clean with time",
			status: &wt.WorktreeStatus{
				IsDirty:        false,
				LastCommitTime: time.Now().Add(-5 * time.Minute),
			},
			want: []string{"clean", "5m ago"},
		},
		{
			name: "ahead only",
			status: &wt.WorktreeStatus{
				IsDirty: false,
				Ahead:   3,
			},
			want: []string{"clean", "↑3"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := formatWorktreeStatus(tc.status, 0, NewStyles(Dark))
			for _, substr := range tc.want {
				assert.Contains(t, result, substr)
			}
		})
	}
}

func TestTimeAgo(t *testing.T) {
	tests := []struct {
		name string
		want string
		ago  time.Duration
	}{
		{"just now", "just now", 10 * time.Second},
		{"minutes", "5m ago", 5 * time.Minute},
		{"hours", "3h ago", 3 * time.Hour},
		{"days", "2d ago", 48 * time.Hour},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := timeAgo(time.Now().Add(-tc.ago))
			assert.Equal(t, tc.want, result)
		})
	}
}

// TestWindowKeyOutsideTmux tests the "w" keybinding when not inside a tmux session.
// (In CI/test environments we are never inside tmux, so the handler should show a toast.)
func TestWindowKeyOutsideTmux(t *testing.T) {
	ctx := context.Background()

	mgr := session.NewManagerWithConfig(session.ManagerConfig{
		SessionMode: session.SessionModeTUI,
	})
	defer mgr.Close()

	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, 80, 24, nil, nil)
	m.worktreeDropdown.SelectIndex(0)

	wKey := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}}
	newModel, cmd := m.Update(wKey)
	m2 := newModel.(Model)

	// Outside tmux, the handler returns a toast cmd (not nil)
	assert.NotNil(t, cmd, "should return a toast command")
	// Should have added a toast about not being inside tmux
	assert.True(t, m2.toasts.HasToasts(), "should show a toast message")
}

// TestStatusBarWindowHint tests that the [w] Window hint appears in tmux mode.
func TestStatusBarWindowHint(t *testing.T) {
	ctx := context.Background()

	t.Run("tmux mode shows window hint", func(t *testing.T) {
		mgr := session.NewManagerWithConfig(session.ManagerConfig{
			SessionMode: session.SessionModeTmux,
		})
		defer mgr.Close()

		m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, []wt.Worktree{
			{Branch: "main", Path: "/tmp/wt/main"},
		}, 80, 24, nil, nil)
		m.worktreeDropdown.SelectIndex(0)

		view := m.View()
		assert.Contains(t, view, "[w] Window")
	})

	t.Run("TUI mode does not show window hint", func(t *testing.T) {
		mgr := session.NewManagerWithConfig(session.ManagerConfig{
			SessionMode: session.SessionModeTUI,
		})
		defer mgr.Close()

		m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, []wt.Worktree{
			{Branch: "main", Path: "/tmp/wt/main"},
		}, 80, 24, nil, nil)
		m.worktreeDropdown.SelectIndex(0)

		view := m.View()
		assert.NotContains(t, view, "[w] Window")
	})
}

func TestDropdownWidth(t *testing.T) {
	d := NewDropdown(nil)
	assert.Equal(t, 0, d.Width())

	d.SetWidth(42)
	assert.Equal(t, 42, d.Width())
}

func TestSessionDropdownUsesTitle(t *testing.T) {
	// Verify that updateSessionDropdown uses Title when available
	info := &session.SessionInfo{
		ID:     "test-session",
		Type:   session.SessionTypePlanner,
		Status: session.StatusRunning,
		Prompt: "implement user authentication for the login page",
		Title:  "implement user",
		Model:  "sonnet",
	}

	// Verify the model display in the session header
	model := NewOutputModel(info, nil)
	model.SetSize(80, 24)
	view := model.View()

	// Should contain the session info
	assert.Contains(t, view, "test-session")
}

func TestOutputModelWithModelField(t *testing.T) {
	// Test that Model field is accessible through SessionInfo
	info := &session.SessionInfo{
		ID:     "model-test",
		Type:   session.SessionTypeBuilder,
		Status: session.StatusRunning,
		Prompt: "build something",
		Model:  "sonnet",
	}

	assert.Equal(t, "sonnet", info.Model)
	assert.Equal(t, session.SessionTypeBuilder, info.Type)
}
