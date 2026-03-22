package session

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// MockSessionState represents scripted state for a mock child session.
type MockSessionState struct { //nolint:govet // fieldalignment: keep related fields grouped
	ID           string
	Type         string
	Status       string
	ErrorMsg     string
	Model        string
	Question     string // non-empty when Status is "waiting_for_input"
	TurnCount    int
	TotalCostUSD float64
	InputTokens  int
	OutputTokens int
	RecentOutput []string
}

// MockSessionBehavior defines state progression for a session type.
// States are advanced one at a time by AdvanceAll() between delegator turns,
// NOT by get_session_progress calls (which are read-only).
// When exhausted, the last state persists.
type MockSessionBehavior struct {
	States []MockSessionState
}

// MockToolCall records a tool invocation for assertion.
type MockToolCall struct { //nolint:govet // fieldalignment: keep related fields grouped
	Tool   string
	Params map[string]any
	Time   time.Time
}

// mockSessionEntry tracks a single mock session's state.
type mockSessionEntry struct { //nolint:govet // fieldalignment: keep related fields grouped
	behavior *MockSessionBehavior
	stateIdx int
	current  MockSessionState
}

// MockDelegatorToolHandler is a drop-in replacement for DelegatorToolHandler
// that returns scripted responses and records all calls.
type MockDelegatorToolHandler struct { //nolint:govet // fieldalignment: keep related fields grouped
	registry  *claude.TypedToolRegistry
	mu        sync.Mutex
	calls     []MockToolCall
	sessions  map[string]*mockSessionEntry
	behaviors map[string][]*MockSessionBehavior // type → queue of behaviors
	notified  map[string]string                 // id → status last notified
	nextID    int
}

// NewMockDelegatorToolHandler creates a new mock handler with the given
// behavior scripts. The behaviors map keys are session types (e.g. "planner",
// "builder"). Each start_session call for a type pops the next behavior from
// the queue.
func NewMockDelegatorToolHandler(behaviors map[string][]*MockSessionBehavior) *MockDelegatorToolHandler {
	h := &MockDelegatorToolHandler{
		sessions:  make(map[string]*mockSessionEntry),
		behaviors: behaviors,
		notified:  make(map[string]string),
	}
	if h.behaviors == nil {
		h.behaviors = make(map[string][]*MockSessionBehavior)
	}

	registry := claude.NewTypedToolRegistry()
	claude.AddTool(registry, "start_session",
		"Start a new child session (planner, builder, or codetalk) on the worktree. Returns the session ID.",
		h.handleStartSession)
	claude.AddTool(registry, "stop_session",
		"Stop a running child session.",
		h.handleStopSession)
	claude.AddTool(registry, "get_session_progress",
		"Get the current progress and recent output of a child session.",
		h.handleGetSessionProgress)
	claude.AddTool(registry, "send_followup",
		"Send a follow-up message to an idle child session.",
		h.handleSendFollowUp)

	h.registry = registry
	return h
}

// Registry returns the underlying TypedToolRegistry for use with WithSDKTools.
func (h *MockDelegatorToolHandler) Registry() *claude.TypedToolRegistry {
	return h.registry
}

// Calls returns all recorded tool calls.
func (h *MockDelegatorToolHandler) Calls() []MockToolCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]MockToolCall, len(h.calls))
	copy(out, h.calls)
	return out
}

// CallsFor returns recorded calls filtered by tool name.
func (h *MockDelegatorToolHandler) CallsFor(tool string) []MockToolCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []MockToolCall
	for _, c := range h.calls {
		if c.Tool == tool {
			out = append(out, c)
		}
	}
	return out
}

// SessionIDs returns all created mock session IDs.
func (h *MockDelegatorToolHandler) SessionIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := make([]string, 0, len(h.sessions))
	for id := range h.sessions {
		ids = append(ids, id)
	}
	return ids
}

// SetSessionState manually overrides the current state of a mock session.
func (h *MockDelegatorToolHandler) SetSessionState(id string, state MockSessionState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if entry, ok := h.sessions[id]; ok {
		state.ID = id
		state.Type = entry.current.Type
		entry.current = state
	}
}

// AdvanceAll advances each session by one step in its scripted behavior.
// This simulates time passing between delegator turns — in the real system,
// child sessions make progress asynchronously while the delegator is idle.
// Call this between turns, not within a turn.
func (h *MockDelegatorToolHandler) AdvanceAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, entry := range h.sessions {
		if entry.behavior == nil {
			continue
		}
		if entry.stateIdx+1 >= len(entry.behavior.States) {
			continue
		}
		entry.stateIdx++
		s := entry.behavior.States[entry.stateIdx]
		s.ID = entry.current.ID
		s.Type = entry.current.Type
		if s.Model == "" {
			s.Model = entry.current.Model
		}
		entry.current = s
	}
}

// DrainNotifications returns a notification message for sessions that have
// reached a notable state (completed, failed, stopped, waiting_for_input)
// since the last call. Returns empty string if there are no new notifications.
func (h *MockDelegatorToolHandler) DrainNotifications() string {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Sort session IDs for deterministic notification ordering.
	ids := make([]string, 0, len(h.sessions))
	for id := range h.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var parts []string
	for _, id := range ids {
		entry := h.sessions[id]
		st := entry.current.Status
		if !isNotifiableStatus(st) {
			continue
		}
		if h.notified[id] == st {
			continue // already notified for this status
		}
		h.notified[id] = st

		switch st {
		case "waiting_for_input":
			q := entry.current.Question
			if q == "" {
				q = "Session needs input"
			}
			parts = append(parts, fmt.Sprintf(
				"Child session %s is waiting for input: %s", id, q))
		default:
			parts = append(parts, fmt.Sprintf(
				"Child session %s status changed to %s. Use get_session_progress to check details.", id, st))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n")
}

// AdvanceUntilNotification steps sessions forward one at a time until a
// notifiable state is reached or no more progress can be made.
// This simulates multiple child turns happening between delegator turns.
func (h *MockDelegatorToolHandler) AdvanceUntilNotification() string {
	const maxSteps = 20
	for i := 0; i < maxSteps; i++ {
		h.AdvanceAll()
		notification := h.DrainNotifications()
		if notification != "" {
			return notification
		}
	}
	return ""
}

func isNotifiableStatus(s string) bool {
	return s == "completed" || s == "failed" || s == "stopped" || s == "waiting_for_input"
}

func (h *MockDelegatorToolHandler) record(tool string, params map[string]any) {
	h.calls = append(h.calls, MockToolCall{
		Tool:   tool,
		Params: params,
		Time:   time.Now(),
	})
}

func (h *MockDelegatorToolHandler) handleStartSession(_ context.Context, params startSessionParams) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.record("start_session", map[string]any{
		"type":   params.Type,
		"prompt": params.Prompt,
		"model":  params.Model,
	})

	if params.Type != "planner" && params.Type != "builder" && params.Type != "codetalk" {
		return "", fmt.Errorf("invalid session type %q: must be planner, builder, or codetalk", params.Type)
	}

	h.nextID++
	id := fmt.Sprintf("mock-%s-%d", params.Type, h.nextID)

	model := params.Model
	if model == "" {
		switch params.Type {
		case "planner", "codetalk":
			model = "opus"
		default:
			model = "sonnet"
		}
	}

	entry := &mockSessionEntry{
		current: MockSessionState{
			ID:     id,
			Type:   params.Type,
			Status: "running",
			Model:  model,
		},
	}

	// Pop the next behavior for this type if available.
	if queue, ok := h.behaviors[params.Type]; ok && len(queue) > 0 {
		entry.behavior = queue[0]
		h.behaviors[params.Type] = queue[1:]
		// Apply initial state from behavior if present.
		if len(entry.behavior.States) > 0 {
			s := entry.behavior.States[0]
			s.ID = id
			s.Type = params.Type
			if s.Model == "" {
				s.Model = model
			}
			entry.current = s
			entry.stateIdx = 0
		}
	}

	h.sessions[id] = entry
	return fmt.Sprintf("Started %s session: %s", params.Type, id), nil
}

func (h *MockDelegatorToolHandler) handleStopSession(_ context.Context, params stopSessionParams) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.record("stop_session", map[string]any{
		"session_id": params.SessionID,
	})

	entry, ok := h.sessions[params.SessionID]
	if !ok {
		return "", fmt.Errorf("session not found: %s", params.SessionID)
	}
	entry.current.Status = "stopped"
	return fmt.Sprintf("Stopped session: %s", params.SessionID), nil
}

func (h *MockDelegatorToolHandler) handleGetSessionProgress(_ context.Context, params getSessionProgressParams) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.record("get_session_progress", map[string]any{
		"session_id": params.SessionID,
	})

	entry, ok := h.sessions[params.SessionID]
	if !ok {
		return "", fmt.Errorf("session not found: %s", params.SessionID)
	}

	// Read-only: return current state without advancing.
	// State only advances via AdvanceAll() between turns.
	st := entry.current
	var b strings.Builder
	fmt.Fprintf(&b, "Session: %s\n", st.ID)
	fmt.Fprintf(&b, "Type: %s\n", st.Type)
	fmt.Fprintf(&b, "Status: %s\n", st.Status)
	fmt.Fprintf(&b, "Model: %s\n", st.Model)
	fmt.Fprintf(&b, "Turns: %d\n", st.TurnCount)
	fmt.Fprintf(&b, "Cost: $%.4f\n", st.TotalCostUSD)
	fmt.Fprintf(&b, "Tokens: %d in / %d out\n", st.InputTokens, st.OutputTokens)

	if st.ErrorMsg != "" {
		fmt.Fprintf(&b, "Error: %s\n", st.ErrorMsg)
	}

	if st.Question != "" {
		fmt.Fprintf(&b, "Question: %s\n", st.Question)
	}

	if len(st.RecentOutput) > 0 {
		fmt.Fprintf(&b, "\nRecent output:\n")
		for _, line := range st.RecentOutput {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}

	return b.String(), nil
}

func (h *MockDelegatorToolHandler) handleSendFollowUp(_ context.Context, params sendFollowUpParams) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.record("send_followup", map[string]any{
		"session_id": params.SessionID,
		"prompt":     params.Prompt,
	})

	entry, ok := h.sessions[params.SessionID]
	if !ok {
		return "", fmt.Errorf("session not found: %s", params.SessionID)
	}
	if entry.current.Status != "idle" {
		return "", fmt.Errorf("session %s is not idle (status: %s)", params.SessionID, entry.current.Status)
	}
	entry.current.Status = "running"
	return fmt.Sprintf("Follow-up sent to session %s. It will resume and you will be notified when it completes.", params.SessionID), nil
}
