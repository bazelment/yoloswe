package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestSessionEventHandler(t *testing.T) (*Manager, *sessionEventHandler, SessionID) {
	t.Helper()

	manager := NewManager()
	t.Cleanup(manager.Close)

	sessionID := SessionID("test-session")
	manager.AddSession(&Session{
		ID:       sessionID,
		Status:   StatusRunning,
		Progress: &SessionProgress{},
	})
	manager.InitOutputBuffer(sessionID)

	return manager, newSessionEventHandler(manager, sessionID), sessionID
}

func TestSessionEventHandler_OnToolResultUpdatesCompletedToolLine(t *testing.T) {
	manager, handler, sessionID := newTestSessionEventHandler(t)

	handler.OnToolStart("Read", "tool-1", nil)
	handler.OnToolComplete("Read", "tool-1", map[string]interface{}{"file_path": "x"}, nil, false)
	handler.OnToolResult("file contents", false)

	output := manager.GetSessionOutput(sessionID)
	require.Len(t, output, 1)
	line := output[0]
	assert.Equal(t, "tool-1", line.ToolID)
	assert.Equal(t, map[string]interface{}{"file_path": "x"}, line.ToolInput)
	assert.Equal(t, "file contents", line.ToolResult)
	assert.Equal(t, ToolStateComplete, line.ToolState)
	assert.False(t, line.IsError)
}

func TestSessionEventHandler_OnToolResultMarksError(t *testing.T) {
	manager, handler, sessionID := newTestSessionEventHandler(t)

	handler.OnToolStart("Read", "tool-1", nil)
	handler.OnToolComplete("Read", "tool-1", map[string]interface{}{"file_path": "x"}, nil, false)
	handler.OnToolResult("failed", true)

	output := manager.GetSessionOutput(sessionID)
	require.Len(t, output, 1)
	line := output[0]
	assert.Equal(t, "tool-1", line.ToolID)
	assert.Equal(t, "failed", line.ToolResult)
	assert.Equal(t, ToolStateError, line.ToolState)
	assert.True(t, line.IsError)
}

func TestSessionEventHandler_OnToolResultUsesLastCompletedTool(t *testing.T) {
	manager, handler, sessionID := newTestSessionEventHandler(t)

	handler.OnToolStart("Read", "tool-1", nil)
	handler.OnToolComplete("Read", "tool-1", map[string]interface{}{"file_path": "x"}, nil, false)
	handler.OnToolStart("Bash", "tool-2", nil)
	handler.OnToolResult("file contents", false)

	output := manager.GetSessionOutput(sessionID)
	require.Len(t, output, 2)
	assert.Equal(t, "file contents", output[0].ToolResult)
	assert.Equal(t, ToolStateComplete, output[0].ToolState)
	assert.Nil(t, output[1].ToolResult)
	assert.Equal(t, ToolStateRunning, output[1].ToolState)
}
