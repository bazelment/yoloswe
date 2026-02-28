package sessionmodel

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testdataPath(name string) string {
	// Bazel sets TEST_SRCDIR and TEST_WORKSPACE for runfiles access.
	if srcDir := os.Getenv("TEST_SRCDIR"); srcDir != "" {
		ws := os.Getenv("TEST_WORKSPACE")
		if ws == "" {
			ws = "_main"
		}
		return filepath.Join(srcDir, ws, "bramble", "sessionmodel", "testdata", name)
	}
	// Fallback for go test.
	return filepath.Join("testdata", name)
}

func TestLoadFromRawJSONL_FullSession(t *testing.T) {
	model, err := LoadFromRawJSONL(testdataPath("full_session.jsonl"))
	require.NoError(t, err)

	lines := model.Output()
	require.NotEmpty(t, lines, "should produce output lines")

	// Catalog which OutputLineTypes are present.
	typeCounts := make(map[OutputLineType]int)
	for _, line := range lines {
		typeCounts[line.Type]++
	}

	t.Logf("Total lines: %d", len(lines))
	for typ, count := range typeCounts {
		t.Logf("  %s: %d", typ, count)
	}

	// Verify metadata was extracted from system{init}.
	meta := model.Meta()
	assert.Equal(t, "claude-opus-4-6", meta.Model)
	assert.Equal(t, "/home/user/project", meta.CWD)
	assert.Equal(t, "default", meta.PermissionMode)
	assert.Contains(t, meta.Tools, "Read")

	// Invariant assertions — each maps to a bug fixed in e98f666.
	assert.True(t, meta.Status.IsTerminal(), "status should be terminal after complete parse")
	assert.NotEmpty(t, meta.SessionID, "session ID should be captured from envelope")
	assert.GreaterOrEqual(t, len(lines), 10, "uncapped buffer should preserve all lines")

	// First user message should appear in output (parser captures user text).
	var foundUserPrompt bool
	for _, line := range lines {
		if line.Type == OutputTypeText && line.Content == "Fix the authentication bug in login.go" {
			foundUserPrompt = true
			break
		}
	}
	assert.True(t, foundUserPrompt, "first user message should appear in output lines")

	// Verify we got the expected output line types from the fixture.
	assert.Greater(t, typeCounts[OutputTypeText], 0, "should have text lines")
	assert.Greater(t, typeCounts[OutputTypeThinking], 0, "should have thinking lines")
	assert.Greater(t, typeCounts[OutputTypeToolStart], 0, "should have tool_start lines")
	assert.Greater(t, typeCounts[OutputTypeTurnEnd], 0, "should have turn_end lines")
	assert.Greater(t, typeCounts[OutputTypeError], 0, "should have error lines (api_error)")
	assert.Greater(t, typeCounts[OutputTypeStatus], 0, "should have status lines")

	// Verify specific content from the fixture.
	var foundPR, foundCompact, foundAPIError, foundMCP, foundWaiting bool
	for _, line := range lines {
		switch {
		case line.Content == "PR #42: https://github.com/example/repo/pull/42":
			foundPR = true
		case line.Content == "── Context compacted ──":
			foundCompact = true
		case line.Type == OutputTypeError && line.Content != "":
			foundAPIError = true
		case line.Content == "MCP playwright/browser_navigate: completed":
			foundMCP = true
		case line.Content == "Waiting: Run E2E tests":
			foundWaiting = true
		}
	}

	assert.True(t, foundPR, "should have PR link status line")
	assert.True(t, foundCompact, "should have compact boundary status line")
	assert.True(t, foundAPIError, "should have API error line")
	assert.True(t, foundMCP, "should have MCP progress status line")
	assert.True(t, foundWaiting, "should have waiting_for_task status line")
}

func TestLoadFromRawJSONL_ToolLifecycle(t *testing.T) {
	model, err := LoadFromRawJSONL(testdataPath("full_session.jsonl"))
	require.NoError(t, err)

	lines := model.Output()

	// Find Read tool — should start as running then get completed by tool_result.
	var readTool *OutputLine
	for i := range lines {
		if lines[i].ToolName == "Read" && lines[i].ToolID == "toolu-001" {
			readTool = &lines[i]
			break
		}
	}
	require.NotNil(t, readTool, "should find Read tool line")
	assert.Equal(t, OutputTypeToolStart, readTool.Type)
	assert.Equal(t, ToolStateComplete, readTool.ToolState, "tool_result should mark it complete")

	// Find Edit tool — should also be completed.
	var editTool *OutputLine
	for i := range lines {
		if lines[i].ToolName == "Edit" && lines[i].ToolID == "toolu-002" {
			editTool = &lines[i]
			break
		}
	}
	require.NotNil(t, editTool, "should find Edit tool line")
	assert.Equal(t, ToolStateComplete, editTool.ToolState)
}

func TestLoadFromRawJSONL_Progress(t *testing.T) {
	model, err := LoadFromRawJSONL(testdataPath("full_session.jsonl"))
	require.NoError(t, err)

	progress := model.Progress()
	assert.Equal(t, 1, progress.TurnCount)
	assert.InDelta(t, 0.0235, progress.TotalCostUSD, 0.001)
}

func TestLoadFromRawJSONL_NonexistentFile(t *testing.T) {
	_, err := LoadFromRawJSONL("/nonexistent/file.jsonl")
	require.Error(t, err)
}

func TestFromRawJSONL_AllEnvelopeTypes(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		wantMsg bool // expect a vocabulary message
		wantType string // expected meta.Type
	}{
		{
			name:    "assistant message",
			line:    `{"type":"assistant","sessionId":"s1","uuid":"a1","message":{"role":"assistant","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{}},"timestamp":"2026-02-28T10:00:00.000Z"}`,
			wantMsg: true,
			wantType: "assistant",
		},
		{
			name:    "user message with tool result",
			line:    `{"type":"user","sessionId":"s1","uuid":"u1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok","is_error":false}]},"timestamp":"2026-02-28T10:00:00.000Z"}`,
			wantMsg: true,
			wantType: "user",
		},
		{
			name:    "system init",
			line:    `{"type":"system","subtype":"init","sessionId":"s1","uuid":"s1","model":"claude-opus-4-6","cwd":"/tmp","tools":[],"timestamp":"2026-02-28T10:00:00.000Z"}`,
			wantMsg: true,
			wantType: "system",
		},
		{
			name:    "system api_error",
			line:    `{"type":"system","subtype":"api_error","sessionId":"s1","uuid":"s2","error":{"cause":{"code":"FailedToOpenSocket","path":"https://api.anthropic.com"}},"timestamp":"2026-02-28T10:00:00.000Z"}`,
			wantMsg: false,
			wantType: "system",
		},
		{
			name:    "system turn_duration",
			line:    `{"type":"system","subtype":"turn_duration","sessionId":"s1","uuid":"s3","durationMs":5000,"timestamp":"2026-02-28T10:00:00.000Z"}`,
			wantMsg: false,
			wantType: "system",
		},
		{
			name:    "system compact_boundary",
			line:    `{"type":"system","subtype":"compact_boundary","sessionId":"s1","uuid":"s4","content":"Conversation compacted","timestamp":"2026-02-28T10:00:00.000Z"}`,
			wantMsg: false,
			wantType: "system",
		},
		{
			name:    "progress bash",
			line:    `{"type":"progress","sessionId":"s1","data":{"type":"bash_progress","output":"ok"},"uuid":"p1","timestamp":"2026-02-28T10:00:00.000Z"}`,
			wantMsg: false,
			wantType: "progress",
		},
		{
			name:    "progress mcp",
			line:    `{"type":"progress","sessionId":"s1","data":{"type":"mcp_progress","status":"completed","serverName":"pw","toolName":"nav"},"uuid":"p2","timestamp":"2026-02-28T10:00:00.000Z"}`,
			wantMsg: false,
			wantType: "progress",
		},
		{
			name:    "file-history-snapshot",
			line:    `{"type":"file-history-snapshot","messageId":"m1","snapshot":{},"isSnapshotUpdate":false}`,
			wantMsg: false,
			wantType: "file-history-snapshot",
		},
		{
			name:    "queue-operation",
			line:    `{"type":"queue-operation","operation":"enqueue","sessionId":"s1","timestamp":"2026-02-28T10:00:00.000Z"}`,
			wantMsg: false,
			wantType: "queue-operation",
		},
		{
			name:    "pr-link",
			line:    `{"type":"pr-link","sessionId":"s1","prNumber":7,"prUrl":"https://github.com/x/y/pull/7","prRepository":"x/y","timestamp":"2026-02-28T10:00:00.000Z"}`,
			wantMsg: false,
			wantType: "pr-link",
		},
		{
			name:    "result message",
			line:    `{"type":"result","session_id":"s1","uuid":"r1","num_turns":2,"total_cost_usd":0.05,"duration_ms":8000,"is_error":false,"usage":{"input_tokens":5000,"output_tokens":800},"timestamp":"2026-02-28T10:00:00.000Z"}`,
			wantMsg: true,
			wantType: "result",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg, meta, err := FromRawJSONL([]byte(tc.line))
			require.NoError(t, err)

			if tc.wantMsg {
				assert.NotNil(t, msg, "expected a vocabulary message")
			} else {
				assert.Nil(t, msg, "expected nil message for envelope-only type")
			}

			require.NotNil(t, meta, "meta should always be non-nil")
			assert.Equal(t, tc.wantType, meta.Type)
		})
	}
}
