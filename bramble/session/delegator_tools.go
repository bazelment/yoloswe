package session

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/bramble/sessionmodel"
	"github.com/bazelment/yoloswe/multiagent/agent"
)

// DelegatorToolHandler provides SDK tools for the delegator agent to manage
// child sessions via the session Manager.
type DelegatorToolHandler struct { //nolint:govet // fieldalignment: keep related fields grouped
	registry      *claude.TypedToolRegistry
	manager       *Manager
	worktreePath  string
	model         string // delegator's model, used as default for child sessions
	modelRegistry *agent.ModelRegistry
	childIDs      map[SessionID]struct{}
	mu            sync.Mutex
}

// startSessionParams are the parameters for the start_session tool.
type startSessionParams struct {
	Type   string `json:"type" jsonschema:"required,description=Session type to start: planner (read-only analysis) or builder (code modification),enum=planner,enum=builder"`
	Prompt string `json:"prompt" jsonschema:"required,description=The task prompt for the child session"`
	Model  string `json:"model,omitempty" jsonschema:"description=Model to use for the child session. See the system prompt for a list of available models. Defaults to the delegator's own model if not specified."`
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
// child sessions on the given worktree path. If registry is non-nil, model
// names supplied to start_session are validated against it.
func NewDelegatorToolHandler(manager *Manager, worktreePath string, model string, modelRegistry *agent.ModelRegistry) *DelegatorToolHandler {
	h := &DelegatorToolHandler{
		manager:       manager,
		worktreePath:  worktreePath,
		model:         model,
		modelRegistry: modelRegistry,
		childIDs:      make(map[SessionID]struct{}),
	}

	toolRegistry := claude.NewTypedToolRegistry()
	claude.AddTool(toolRegistry, "start_session",
		"Start a new child session (planner or builder) on the worktree. Returns the session ID.",
		h.handleStartSession)
	claude.AddTool(toolRegistry, "stop_session",
		"Stop a running child session.",
		h.handleStopSession)
	claude.AddTool(toolRegistry, "get_session_progress",
		"Get the current progress and recent output of a child session.",
		h.handleGetSessionProgress)

	h.registry = toolRegistry
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

// IsChild reports whether the given session ID is a child of this delegator.
// This is an O(1) map lookup, suitable for use in hot-path event handlers.
func (h *DelegatorToolHandler) IsChild(id SessionID) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.childIDs[id]
	return ok
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

	// Use the delegator's model as default for child sessions, unless the
	// LLM explicitly requested a different model.
	model := params.Model
	if model == "" {
		model = h.model
	}

	// Validate the model against the registry if available.
	if model != "" && h.modelRegistry != nil {
		if _, ok := h.modelRegistry.ModelByID(model); !ok {
			available := make([]string, 0)
			for _, m := range h.modelRegistry.Models() {
				available = append(available, m.ID)
			}
			return "", fmt.Errorf("unknown model %q; available models: %s", model, strings.Join(available, ", "))
		}
	}

	// Pre-register the child ID *before* spawning the session goroutine.
	// generateSessionID + startSessionWithID are the two halves of StartSession;
	// registering first ensures watchChildSessionChanges never misses a state
	// transition that fires before the caller returns from this function.
	worktreeName := filepath.Base(h.worktreePath)
	id := generateSessionID(worktreeName, sessionType)

	h.mu.Lock()
	h.childIDs[id] = struct{}{}
	h.mu.Unlock()

	_, err := h.manager.startSessionWithID(id, sessionType, h.worktreePath, worktreeName, params.Prompt, model)
	if err != nil {
		// Clean up the pre-registration on failure.
		h.mu.Lock()
		delete(h.childIDs, id)
		h.mu.Unlock()
		return "", fmt.Errorf("failed to start session: %w", err)
	}

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

// AvailableModelsDescription returns a human-readable description of available
// models, suitable for injection into the system prompt. Each model is listed
// on its own line with the provider in parentheses, so the model ID is
// unambiguously the first token (avoids "provider: model" being parsed as a
// single string).
// Returns an empty string if no registry is set.
func (h *DelegatorToolHandler) AvailableModelsDescription() string {
	if h.modelRegistry == nil {
		return ""
	}
	models := h.modelRegistry.Models()
	if len(models) == 0 {
		return ""
	}

	// Sort by provider then ID for stable output.
	sort.Slice(models, func(i, j int) bool {
		if models[i].Provider != models[j].Provider {
			return models[i].Provider < models[j].Provider
		}
		return models[i].ID < models[j].ID
	})

	var b strings.Builder
	for _, m := range models {
		fmt.Fprintf(&b, "- %s (%s)\n", m.ID, m.Provider)
	}
	return b.String()
}

func (h *DelegatorToolHandler) handleGetSessionProgress(_ context.Context, params getSessionProgressParams) (string, error) {
	id := SessionID(params.SessionID)

	h.mu.Lock()
	_, isChild := h.childIDs[id]
	h.mu.Unlock()
	if !isChild {
		return "", fmt.Errorf("session %s is not owned by this delegator", id)
	}

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

	if info.PlanFilePath != "" {
		fmt.Fprintf(&b, "Plan file: %s\n", info.PlanFilePath)
	}

	if len(recentLines) > 0 {
		fmt.Fprintf(&b, "\nRecent output:\n")
		for _, line := range recentLines {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}

	return b.String(), nil
}
