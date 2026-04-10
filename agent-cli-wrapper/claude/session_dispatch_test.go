package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/internal/ndjson"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"
)

// attachCapturingProcess gives the session a real processManager whose
// writer points at an in-memory buffer. This lets tests inspect bytes that
// the session would have sent to the CLI (e.g. control_response payloads)
// without spawning a subprocess. Returns the buffer for assertions.
func attachCapturingProcess(t *testing.T, s *Session) *bytes.Buffer {
	t.Helper()
	pm := newProcessManager(s.config)
	buf := &bytes.Buffer{}
	pm.writer = ndjson.NewWriter(buf)
	s.process = pm
	return buf
}

// waitForEvent drains s.events until a matching event type (by discriminator
// func) is found or the timeout fires. Returns the matched event.
func waitForEvent(t *testing.T, s *Session, match func(Event) bool, timeout time.Duration) Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-s.events:
			if match(ev) {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for matching event")
			return nil
		}
	}
}

// expectNoEvent asserts that no event is emitted within window.
func expectNoEvent(t *testing.T, s *Session, window time.Duration) {
	t.Helper()
	select {
	case ev := <-s.events:
		t.Fatalf("expected no event, got %T: %+v", ev, ev)
	case <-time.After(window):
	}
}

// injectLine marshals v to JSON and pushes the raw line through handleLine,
// mirroring the real readLoop path.
func injectLine(t *testing.T, s *Session, v interface{}) {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	s.handleLine(data)
}

// mkSystem builds a raw system-message envelope with subtype plus extra
// payload fields merged in. Returns the JSON line.
func mkSystem(t *testing.T, subtype string, extra map[string]interface{}) []byte {
	t.Helper()
	base := map[string]interface{}{
		"type":       "system",
		"subtype":    subtype,
		"session_id": "sess-1",
		"uuid":       "u-" + subtype,
	}
	for k, v := range extra {
		base[k] = v
	}
	data, err := json.Marshal(base)
	require.NoError(t, err)
	return data
}

func TestSessionDispatchNewEvents(t *testing.T) {
	t.Parallel()

	cases := []struct {
		check func(t *testing.T, ev Event)
		name  string
		line  []byte
	}{
		{
			name: "tool_progress",
			line: mustJSON(t, map[string]interface{}{
				"type":                 "tool_progress",
				"session_id":           "s",
				"uuid":                 "u",
				"tool_use_id":          "tool-abc",
				"tool_name":            "Bash",
				"elapsed_time_seconds": 3.5,
			}),
			check: func(t *testing.T, ev Event) {
				e, ok := ev.(ToolExecutionProgressEvent)
				require.True(t, ok, "got %T", ev)
				require.Equal(t, "tool-abc", e.ToolUseID)
				require.Equal(t, "Bash", e.ToolName)
				require.InDelta(t, 3.5, e.ElapsedTimeSeconds, 0.001)
			},
		},
		{
			name: "auth_status",
			line: mustJSON(t, map[string]interface{}{
				"type":             "auth_status",
				"session_id":       "s",
				"uuid":             "u",
				"isAuthenticating": true,
				"output":           []string{"step 1"},
			}),
			check: func(t *testing.T, ev Event) {
				e, ok := ev.(AuthStatusEvent)
				require.True(t, ok, "got %T", ev)
				require.True(t, e.IsAuthenticating)
				require.Equal(t, []string{"step 1"}, e.Output)
			},
		},
		{
			name: "rate_limit_event",
			line: mustJSON(t, map[string]interface{}{
				"type":       "rate_limit_event",
				"session_id": "s",
				"uuid":       "u",
				"rate_limit_info": map[string]interface{}{
					"status":        "allowed_warning",
					"rateLimitType": "primary",
					"utilization":   0.75,
				},
			}),
			check: func(t *testing.T, ev Event) {
				e, ok := ev.(RateLimitEvent)
				require.True(t, ok, "got %T", ev)
				require.Equal(t, "allowed_warning", e.Status)
				require.NotNil(t, e.Utilization)
				require.InDelta(t, 0.75, *e.Utilization, 0.0001)
			},
		},
		{
			name: "compact_boundary",
			line: mkSystem(t, "compact_boundary", map[string]interface{}{
				"compact_metadata": map[string]interface{}{
					"trigger":    "manual",
					"pre_tokens": 12345,
				},
			}),
			check: func(t *testing.T, ev Event) {
				e, ok := ev.(CompactBoundaryEvent)
				require.True(t, ok, "got %T", ev)
				require.Equal(t, "manual", e.Trigger)
				require.Equal(t, 12345, e.PreTokens)
			},
		},
		{
			name: "post_turn_summary",
			line: mkSystem(t, "post_turn_summary", map[string]interface{}{
				"title":       "Fixed bug",
				"description": "Patched the thing",
			}),
			check: func(t *testing.T, ev Event) {
				e, ok := ev.(PostTurnSummaryEvent)
				require.True(t, ok, "got %T", ev)
				require.Equal(t, "Fixed bug", e.Title)
				require.Equal(t, "Patched the thing", e.Description)
			},
		},
		{
			name: "api_retry",
			line: mkSystem(t, "api_retry", map[string]interface{}{
				"attempt":        2,
				"max_retries":    5,
				"retry_delay_ms": 1000,
				"error":          "overloaded",
			}),
			check: func(t *testing.T, ev Event) {
				e, ok := ev.(APIRetryEvent)
				require.True(t, ok, "got %T", ev)
				require.Equal(t, 2, e.Attempt)
				require.Equal(t, 5, e.MaxRetries)
			},
		},
		{
			name: "local_command_output",
			line: mkSystem(t, "local_command_output", map[string]interface{}{
				"content": "hello from /cost",
			}),
			check: func(t *testing.T, ev Event) {
				e, ok := ev.(LocalCommandOutputEvent)
				require.True(t, ok, "got %T", ev)
				require.Equal(t, "hello from /cost", e.Content)
			},
		},
		{
			name: "hook_started",
			line: mkSystem(t, "hook_started", map[string]interface{}{
				"hook_id":    "h1",
				"hook_name":  "pre_tool",
				"hook_event": "PreToolUse",
			}),
			check: func(t *testing.T, ev Event) {
				e, ok := ev.(HookLifecycleEvent)
				require.True(t, ok, "got %T", ev)
				require.Equal(t, HookPhaseStarted, e.Phase)
				require.Equal(t, "h1", e.HookID)
				require.Equal(t, "pre_tool", e.HookName)
			},
		},
		{
			name: "hook_progress",
			line: mkSystem(t, "hook_progress", map[string]interface{}{
				"hook_id":    "h1",
				"hook_name":  "pre_tool",
				"hook_event": "PreToolUse",
				"stdout":     "partial",
			}),
			check: func(t *testing.T, ev Event) {
				e, ok := ev.(HookLifecycleEvent)
				require.True(t, ok, "got %T", ev)
				require.Equal(t, HookPhaseProgress, e.Phase)
				require.Equal(t, "partial", e.Stdout)
			},
		},
		{
			name: "hook_response",
			line: mkSystem(t, "hook_response", map[string]interface{}{
				"hook_id":    "h1",
				"hook_name":  "pre_tool",
				"hook_event": "PreToolUse",
				"exit_code":  0,
				"outcome":    "allow",
				"output":     "ok",
			}),
			check: func(t *testing.T, ev Event) {
				e, ok := ev.(HookLifecycleEvent)
				require.True(t, ok, "got %T", ev)
				require.Equal(t, HookPhaseResponse, e.Phase)
				require.Equal(t, "allow", e.Outcome)
				require.NotNil(t, e.ExitCode)
				require.Equal(t, 0, *e.ExitCode)
			},
		},
		{
			name: "task_started",
			line: mkSystem(t, "task_started", map[string]interface{}{
				"task_id":     "task-1",
				"description": "Searching files",
				"task_type":   "subagent",
			}),
			check: func(t *testing.T, ev Event) {
				e, ok := ev.(TaskStartedEvent)
				require.True(t, ok, "got %T", ev)
				require.Equal(t, "task-1", e.TaskID)
				require.Equal(t, "Searching files", e.Description)
			},
		},
		{
			name: "task_progress",
			line: mkSystem(t, "task_progress", map[string]interface{}{
				"task_id":     "task-1",
				"description": "Searching",
				"summary":     "Found 3 matches",
			}),
			check: func(t *testing.T, ev Event) {
				e, ok := ev.(TaskProgressEvent)
				require.True(t, ok, "got %T", ev)
				require.Equal(t, "task-1", e.TaskID)
				require.Equal(t, "Found 3 matches", e.Summary)
			},
		},
		{
			name: "task_notification",
			line: mkSystem(t, "task_notification", map[string]interface{}{
				"task_id": "task-1",
				"status":  "completed",
				"summary": "done",
			}),
			check: func(t *testing.T, ev Event) {
				e, ok := ev.(TaskNotificationEvent)
				require.True(t, ok, "got %T", ev)
				require.Equal(t, "completed", e.Status)
				require.Equal(t, "task-1", e.TaskID)
			},
		},
		{
			name: "session_state_changed",
			line: mkSystem(t, "session_state_changed", map[string]interface{}{
				"state": "idle",
			}),
			check: func(t *testing.T, ev Event) {
				e, ok := ev.(CLISessionStateChangedEvent)
				require.True(t, ok, "got %T", ev)
				require.Equal(t, "idle", e.State)
			},
		},
		{
			name: "files_persisted",
			line: mkSystem(t, "files_persisted", map[string]interface{}{
				"files": []map[string]interface{}{
					{"filename": "foo.go", "file_id": "f-1"},
				},
				"processed_at": "2025-01-01T00:00:00Z",
			}),
			check: func(t *testing.T, ev Event) {
				e, ok := ev.(FilesPersistedEvent)
				require.True(t, ok, "got %T", ev)
				require.Len(t, e.Files, 1)
				require.Equal(t, "foo.go", e.Files[0].Filename)
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newTestSession(t)
			s.handleLine(tc.line)
			// Grab the first matching event (skip any incidental others).
			ev := waitForEvent(t, s, func(e Event) bool {
				// Skip StateChangeEvent that might fire from init transitions.
				_, isState := e.(StateChangeEvent)
				return !isState
			}, time.Second)
			tc.check(t, ev)
		})
	}
}

// mustJSON marshals v to JSON bytes or fails the test.
func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func TestSessionDispatchKeepAlive(t *testing.T) {
	t.Parallel()
	s := newTestSession(t)
	injectLine(t, s, map[string]interface{}{"type": "keep_alive"})
	expectNoEvent(t, s, 100*time.Millisecond)
}

func TestSessionDispatchUnknownMessage(t *testing.T) {
	t.Parallel()
	s := newTestSession(t)
	// Completely unknown top-level type. ParseMessage returns UnknownMessage;
	// handleLine logs a warning and emits nothing.
	injectLine(t, s, map[string]interface{}{
		"type":       "future_unknown_type_xyz",
		"session_id": "s",
		"uuid":       "u",
	})
	expectNoEvent(t, s, 100*time.Millisecond)
}

// buildControlRequestLine frames an inner control-request payload inside a
// top-level control_request envelope ready to pass to handleLine.
func buildControlRequestLine(t *testing.T, requestID string, inner map[string]interface{}) []byte {
	t.Helper()
	innerBytes, err := json.Marshal(inner)
	require.NoError(t, err)
	env := map[string]interface{}{
		"type":       "control_request",
		"request_id": requestID,
		"request":    json.RawMessage(innerBytes),
	}
	data, err := json.Marshal(env)
	require.NoError(t, err)
	return data
}

// parseControlResponse extracts the (subtype, request_id, response body) from
// the first ndjson line in buf.
func parseControlResponse(t *testing.T, buf *bytes.Buffer) (string, string, map[string]interface{}) {
	t.Helper()
	line := buf.Bytes()
	require.NotEmpty(t, line, "expected control_response to be written")
	var resp struct {
		Response struct {
			Response  map[string]interface{} `json:"response"`
			Subtype   string                 `json:"subtype"`
			RequestID string                 `json:"request_id"`
		} `json:"response"`
		Type string `json:"type"`
	}
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(line), &resp))
	require.Equal(t, "control_response", resp.Type)
	return resp.Response.Subtype, resp.Response.RequestID, resp.Response.Response
}

func TestSessionDispatchControlRequestHookCallback_DefaultHandler(t *testing.T) {
	t.Parallel()
	s := newTestSession(t)
	buf := attachCapturingProcess(t, s)

	line := buildControlRequestLine(t, "req-hc-1", map[string]interface{}{
		"subtype":     "hook_callback",
		"callback_id": "cb-1",
		"input":       map[string]interface{}{"foo": "bar"},
	})
	s.handleLine(line)

	require.Eventually(t, func() bool { return buf.Len() > 0 }, time.Second, 5*time.Millisecond)
	subtype, reqID, body := parseControlResponse(t, buf)
	require.Equal(t, "success", subtype)
	require.Equal(t, "req-hc-1", reqID)
	require.Empty(t, body, "default hook callback handler should produce empty body")
}

func TestSessionDispatchControlRequestHookCallback_CustomHandler(t *testing.T) {
	t.Parallel()
	s := newTestSession(t, WithHookCallbackHandler(
		func(ctx context.Context, req protocol.HookCallbackRequest) (map[string]any, error) {
			return map[string]any{"foo": "bar"}, nil
		},
	))
	buf := attachCapturingProcess(t, s)

	line := buildControlRequestLine(t, "req-hc-2", map[string]interface{}{
		"subtype":     "hook_callback",
		"callback_id": "cb-2",
		"input":       map[string]interface{}{},
	})
	s.handleLine(line)

	require.Eventually(t, func() bool { return buf.Len() > 0 }, time.Second, 5*time.Millisecond)
	subtype, reqID, body := parseControlResponse(t, buf)
	require.Equal(t, "success", subtype)
	require.Equal(t, "req-hc-2", reqID)
	require.Equal(t, "bar", body["foo"])
}

func TestSessionDispatchControlRequestElicitation_DefaultHandler(t *testing.T) {
	t.Parallel()
	s := newTestSession(t)
	buf := attachCapturingProcess(t, s)

	line := buildControlRequestLine(t, "req-el-1", map[string]interface{}{
		"subtype":         "elicitation",
		"mcp_server_name": "srv",
		"message":         "please confirm",
	})
	s.handleLine(line)

	require.Eventually(t, func() bool { return buf.Len() > 0 }, time.Second, 5*time.Millisecond)
	subtype, reqID, body := parseControlResponse(t, buf)
	require.Equal(t, "success", subtype)
	require.Equal(t, "req-el-1", reqID)
	require.Equal(t, "cancel", body["action"])
}

func TestSessionDispatchControlRequestElicitation_CustomHandler(t *testing.T) {
	t.Parallel()
	s := newTestSession(t, WithElicitationHandler(
		func(ctx context.Context, req protocol.ElicitationRequest) (protocol.ElicitationResponse, error) {
			return protocol.ElicitationResponse{
				Action:  "accept",
				Content: map[string]any{"answer": "yes"},
			}, nil
		},
	))
	buf := attachCapturingProcess(t, s)

	line := buildControlRequestLine(t, "req-el-2", map[string]interface{}{
		"subtype":         "elicitation",
		"mcp_server_name": "srv",
		"message":         "please confirm",
	})
	s.handleLine(line)

	require.Eventually(t, func() bool { return buf.Len() > 0 }, time.Second, 5*time.Millisecond)
	subtype, reqID, body := parseControlResponse(t, buf)
	require.Equal(t, "success", subtype)
	require.Equal(t, "req-el-2", reqID)
	require.Equal(t, "accept", body["action"])
	content, ok := body["content"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, "yes", content["answer"])
}

func TestSessionDispatchControlRequestUnknownSubtype(t *testing.T) {
	t.Parallel()
	s := newTestSession(t)
	buf := attachCapturingProcess(t, s)

	line := buildControlRequestLine(t, "req-unk-1", map[string]interface{}{
		"subtype": "future_unknown_subtype",
		"extra":   "payload",
	})
	s.handleLine(line)

	require.Eventually(t, func() bool { return buf.Len() > 0 }, time.Second, 5*time.Millisecond)
	subtype, reqID, body := parseControlResponse(t, buf)
	require.Equal(t, "success", subtype)
	require.Equal(t, "req-unk-1", reqID)
	require.Empty(t, body)
}
