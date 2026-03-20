package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDelegatorRunnerConstruction(t *testing.T) {
	m := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTUI})
	defer m.Close()

	handler := NewDelegatorToolHandler(m, "/tmp/test-worktree", "", nil)
	eventHandler := newSessionEventHandler(m, "test-session")

	runner := &delegatorRunner{
		toolHandler:  handler,
		eventHandler: eventHandler,
		worktreePath: "/tmp/test-worktree",
		model:        "sonnet",
	}

	// Verify initial state
	assert.Equal(t, "sonnet", runner.model)
	assert.Equal(t, "/tmp/test-worktree", runner.worktreePath)
	assert.Nil(t, runner.claudeSession)

	// CLISessionID returns empty before Start
	assert.Equal(t, "", runner.CLISessionID())

	// Stop is safe before Start
	err := runner.Stop()
	require.NoError(t, err)
}

func TestDelegatorRunnerImplementsInterface(t *testing.T) {
	m := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTUI})
	defer m.Close()

	handler := NewDelegatorToolHandler(m, "/tmp/test-worktree", "", nil)
	eventHandler := newSessionEventHandler(m, "test-session")

	var _ sessionRunner = &delegatorRunner{
		toolHandler:  handler,
		eventHandler: eventHandler,
		worktreePath: "/tmp/test-worktree",
		model:        "sonnet",
	}
}

func TestDelegatorSystemPromptContainsTools(t *testing.T) {
	assert.Contains(t, DelegatorSystemPrompt, "start_session")
	assert.Contains(t, DelegatorSystemPrompt, "stop_session")
	assert.Contains(t, DelegatorSystemPrompt, "get_session_progress")
}

func TestDelegatorToolRegistry(t *testing.T) {
	m := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTUI})
	defer m.Close()

	handler := NewDelegatorToolHandler(m, t.TempDir(), "", nil)
	registry := handler.Registry()
	tools := registry.Tools()

	require.Len(t, tools, 3)

	toolNames := make([]string, len(tools))
	for i, tool := range tools {
		toolNames[i] = tool.Name
	}
	assert.Contains(t, toolNames, "start_session")
	assert.Contains(t, toolNames, "stop_session")
	assert.Contains(t, toolNames, "get_session_progress")
}

func TestDelegatorSystemPromptWithModels(t *testing.T) {
	t.Run("empty models returns base prompt", func(t *testing.T) {
		result := delegatorSystemPromptWithModels("")
		assert.Equal(t, DelegatorSystemPrompt, result)
	})

	t.Run("appends model list", func(t *testing.T) {
		models := "- claude: opus, sonnet\n- gemini: gemini-2.5-pro\n"
		result := delegatorSystemPromptWithModels(models)

		assert.Contains(t, result, DelegatorSystemPrompt)
		assert.Contains(t, result, "## Available models")
		assert.Contains(t, result, "claude: opus, sonnet")
		assert.Contains(t, result, "gemini: gemini-2.5-pro")
	})
}
