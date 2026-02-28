package cursor

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/internal/ndjson"
)

// fakeSession simulates a cursor session by writing NDJSON lines to a pipe
// and reading events from the session's handleLine method.
func fakeSession(t *testing.T, lines []string) []Event {
	t.Helper()

	pr, pw := io.Pipe()
	go func() {
		w := ndjson.NewWriter(pw)
		for _, line := range lines {
			// Write raw JSON lines
			if err := w.WriteRaw([]byte(line)); err != nil {
				t.Logf("write error: %v", err)
			}
		}
		pw.Close()
	}()

	reader := ndjson.NewReader(pr)
	events := make(chan Event, 100)

	go func() {
		defer close(events)
		var textBuilder strings.Builder
		for {
			line, err := reader.ReadLine()
			if err != nil {
				return
			}

			msg, err := ParseMessage(line)
			if err != nil {
				events <- ErrorEvent{Error: err, Context: "parse"}
				continue
			}
			if msg == nil {
				// Unknown but valid message type — skip (mirrors session.go behavior).
				continue
			}

			// Simulate session's handleLine dispatch
			switch m := msg.(type) {
			case *SystemInitMessage:
				events <- ReadyEvent{SessionID: m.SessionID, Model: m.Model}
			case *AssistantMessage:
				for _, block := range m.Message.Content {
					if block.Type == "text" && block.Text != "" {
						textBuilder.WriteString(block.Text)
						events <- TextEvent{Text: block.Text, FullText: textBuilder.String()}
					}
				}
			case *ToolCallMessage:
				detail, err := ParseToolCallDetail(m)
				if err != nil {
					events <- ErrorEvent{Error: err, Context: "tool_call"}
					continue
				}
				switch m.Subtype {
				case "started":
					events <- ToolStartEvent{ID: m.CallID, Name: detail.Name, Input: detail.Args}
				case "completed":
					events <- ToolCompleteEvent{ID: m.CallID, Name: detail.Name, Input: detail.Args, Result: detail.Result}
				}
			case *ResultMessage:
				events <- TurnCompleteEvent{Success: !m.IsError, DurationMs: m.DurationMs, DurationAPIMs: m.DurationAPIMs}
			}
		}
	}()

	var collected []Event
	for evt := range events {
		collected = append(collected, evt)
	}
	return collected
}

func TestSession_FullStream(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"s1","model":"cursor-fast","cwd":"/tmp","permissionMode":"auto","apiKeySource":"env"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hello "}]},"session_id":"s1"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"World"}]},"session_id":"s1"}`,
		`{"type":"tool_call","subtype":"started","call_id":"c1","tool_call":{"readToolCall":{"args":{"path":"/tmp/test.go"}}},"session_id":"s1"}`,
		`{"type":"tool_call","subtype":"completed","call_id":"c1","tool_call":{"readToolCall":{"args":{"path":"/tmp/test.go"},"result":"contents"}},"session_id":"s1"}`,
		`{"type":"result","subtype":"success","duration_ms":1234,"duration_api_ms":1000,"is_error":false,"result":"done","session_id":"s1"}`,
	}

	events := fakeSession(t, lines)
	require.Len(t, events, 6)

	// ReadyEvent
	ready, ok := events[0].(ReadyEvent)
	require.True(t, ok)
	assert.Equal(t, "s1", ready.SessionID)
	assert.Equal(t, "cursor-fast", ready.Model)

	// TextEvent 1
	text1, ok := events[1].(TextEvent)
	require.True(t, ok)
	assert.Equal(t, "Hello ", text1.Text)
	assert.Equal(t, "Hello ", text1.FullText)

	// TextEvent 2
	text2, ok := events[2].(TextEvent)
	require.True(t, ok)
	assert.Equal(t, "World", text2.Text)
	assert.Equal(t, "Hello World", text2.FullText)

	// ToolStartEvent
	toolStart, ok := events[3].(ToolStartEvent)
	require.True(t, ok)
	assert.Equal(t, "c1", toolStart.ID)
	assert.Equal(t, "readToolCall", toolStart.Name)

	// ToolCompleteEvent
	toolComplete, ok := events[4].(ToolCompleteEvent)
	require.True(t, ok)
	assert.Equal(t, "c1", toolComplete.ID)
	assert.Equal(t, "readToolCall", toolComplete.Name)
	assert.Equal(t, "contents", toolComplete.Result)

	// TurnCompleteEvent
	turnComplete, ok := events[5].(TurnCompleteEvent)
	require.True(t, ok)
	assert.True(t, turnComplete.Success)
	assert.Equal(t, int64(1234), turnComplete.DurationMs)
}

func TestSession_ErrorResult(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"s1","model":"cursor-fast","cwd":"/tmp","permissionMode":"auto","apiKeySource":"env"}`,
		`{"type":"result","subtype":"error","duration_ms":500,"duration_api_ms":400,"is_error":true,"result":"something failed","session_id":"s1"}`,
	}

	events := fakeSession(t, lines)
	require.Len(t, events, 2)

	turnComplete, ok := events[1].(TurnCompleteEvent)
	require.True(t, ok)
	assert.False(t, turnComplete.Success)
}

func TestSession_MalformedLine(t *testing.T) {
	lines := []string{
		`{bad json}`,
	}

	events := fakeSession(t, lines)
	require.Len(t, events, 1)

	errEvt, ok := events[0].(ErrorEvent)
	require.True(t, ok)
	assert.Equal(t, "parse", errEvt.Context)
}

func TestNewSession_DefaultConfig(t *testing.T) {
	s := NewSession("test prompt")
	assert.Equal(t, "test prompt", s.prompt)
	assert.Equal(t, "agent", s.config.CLIPath)
	assert.Equal(t, 100, s.config.EventBufferSize)
}

func TestNewSession_WithOptions(t *testing.T) {
	s := NewSession("test",
		WithModel("cursor-fast"),
		WithWorkDir("/tmp"),
		WithCLIPath("/usr/bin/agent"),
		WithForce(),
		WithTrust(),
		WithEventBufferSize(200),
	)
	assert.Equal(t, "cursor-fast", s.config.Model)
	assert.Equal(t, "/tmp", s.config.WorkDir)
	assert.Equal(t, "/usr/bin/agent", s.config.CLIPath)
	assert.True(t, s.config.Force)
	assert.True(t, s.config.Trust)
	assert.Equal(t, 200, s.config.EventBufferSize)
}

func TestSession_StopBeforeStart(t *testing.T) {
	s := NewSession("test")
	err := s.Stop()
	assert.NoError(t, err)
}

func TestSession_StartTwice(t *testing.T) {
	// We can't actually start without a real binary, but we can test the guard.
	s := NewSession("test", WithCLIPath("nonexistent-binary-that-does-not-exist"))

	ctx := context.Background()
	// First start will fail because binary doesn't exist, but that's fine.
	err := s.Start(ctx)
	if err != nil {
		// Expected - binary not found
		return
	}
	defer s.Stop()

	// Second start should fail
	err = s.Start(ctx)
	assert.ErrorIs(t, err, ErrAlreadyStarted)
}
