package session

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/bramble/sessionmodel"
)

// DelegatorToolHandler provides SDK tools for the delegator agent to manage
// child sessions via the session Manager.
type DelegatorToolHandler struct { //nolint:govet // fieldalignment: keep related fields grouped
	registry     *claude.TypedToolRegistry
	manager      *Manager
	worktreePath string
	childIDs     map[SessionID]struct{}
	mu           sync.Mutex
}

// startSessionParams are the parameters for the start_session tool.
type startSessionParams struct {
	Type   string `json:"type" jsonschema:"required,description=Session type to start: planner (read-only analysis) or builder (code modification),enum=planner,enum=builder"`
	Prompt string `json:"prompt" jsonschema:"required,description=The task prompt for the child session"`
	Model  string `json:"model,omitempty" jsonschema:"description=Model to use (e.g. opus or sonnet). Defaults to opus for planner and sonnet for builder."`
}

// stopSessionParams are the parameters for the stop_session tool.
type stopSessionParams struct {
	SessionID string `json:"session_id" jsonschema:"required,description=The session ID to stop"`
}

// getSessionProgressParams are the parameters for the get_session_progress tool.
type getSessionProgressParams struct {
	SessionID string `json:"session_id" jsonschema:"required,description=The session ID to check progress for"`
}

// NewDelegatorToolHandler creates a new DelegatorToolHandler that manages
// child sessions on the given worktree path.
func NewDelegatorToolHandler(manager *Manager, worktreePath string) *DelegatorToolHandler {
	h := &DelegatorToolHandler{
		manager:      manager,
		worktreePath: worktreePath,
		childIDs:     make(map[SessionID]struct{}),
	}

	registry := claude.NewTypedToolRegistry()
	claude.AddTool(registry, "start_session",
		"Start a new child session (planner or builder) on the worktree. Returns the session ID.",
		h.handleStartSession)
	claude.AddTool(registry, "stop_session",
		"Stop a running child session.",
		h.handleStopSession)
	claude.AddTool(registry, "get_session_progress",
		"Get the current progress and recent output of a child session.",
		h.handleGetSessionProgress)

	h.registry = registry
	return h
}

// Registry returns the underlying TypedToolRegistry for use with WithSDKTools.
func (h *DelegatorToolHandler) Registry() *claude.TypedToolRegistry {
	return h.registry
}

// ChildIDs returns a snapshot of the tracked child session IDs.
func (h *DelegatorToolHandler) ChildIDs() []SessionID {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := make([]SessionID, 0, len(h.childIDs))
	for id := range h.childIDs {
		ids = append(ids, id)
	}
	return ids
}

func (h *DelegatorToolHandler) handleStartSession(_ context.Context, params startSessionParams) (string, error) {
	var sessionType SessionType
	switch params.Type {
	case "planner":
		sessionType = SessionTypePlanner
	case "builder":
		sessionType = SessionTypeBuilder
	default:
		return "", fmt.Errorf("invalid session type %q: must be planner or builder", params.Type)
	}

	id, err := h.manager.StartSession(sessionType, h.worktreePath, params.Prompt, params.Model)
	if err != nil {
		return "", fmt.Errorf("failed to start session: %w", err)
	}

	h.mu.Lock()
	h.childIDs[id] = struct{}{}
	h.mu.Unlock()

	return fmt.Sprintf("Started %s session: %s", params.Type, id), nil
}

func (h *DelegatorToolHandler) handleStopSession(_ context.Context, params stopSessionParams) (string, error) {
	id := SessionID(params.SessionID)

	h.mu.Lock()
	_, isChild := h.childIDs[id]
	h.mu.Unlock()
	if !isChild {
		return "", fmt.Errorf("session %s is not owned by this delegator", id)
	}

	if err := h.manager.StopSession(id); err != nil {
		return "", fmt.Errorf("failed to stop session: %w", err)
	}
	return fmt.Sprintf("Stopped session: %s", id), nil
}

func (h *DelegatorToolHandler) handleGetSessionProgress(_ context.Context, params getSessionProgressParams) (string, error) {
	id := SessionID(params.SessionID)
	info, ok := h.manager.GetSessionInfo(id)
	if !ok {
		return "", fmt.Errorf("session not found: %s", id)
	}

	recentLines := h.manager.RecentOutputLines(id, sessionmodel.RecentOutputDisplayLines)

	var b strings.Builder
	fmt.Fprintf(&b, "Session: %s\n", info.ID)
	fmt.Fprintf(&b, "Type: %s\n", info.Type)
	fmt.Fprintf(&b, "Status: %s\n", info.Status)
	fmt.Fprintf(&b, "Model: %s\n", info.Model)
	fmt.Fprintf(&b, "Turns: %d\n", info.Progress.TurnCount)
	fmt.Fprintf(&b, "Cost: $%.4f\n", info.Progress.TotalCostUSD)
	fmt.Fprintf(&b, "Tokens: %d in / %d out\n", info.Progress.InputTokens, info.Progress.OutputTokens)

	if info.ErrorMsg != "" {
		fmt.Fprintf(&b, "Error: %s\n", info.ErrorMsg)
	}

	if len(recentLines) > 0 {
		fmt.Fprintf(&b, "\nRecent output:\n")
		for _, line := range recentLines {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}

	return b.String(), nil
}
