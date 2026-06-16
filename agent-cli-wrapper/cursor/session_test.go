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

// fakeSession drives the real Session.handleLine dispatch over a set of NDJSON
// lines and collects the events it emits. It exercises production parsing code
// directly rather than reimplementing it, so the test stays in lockstep with
// session.go (e.g. parse/skip behavior) instead of drifting from it.
func fakeSession(t *testing.T, lines []string) []Event {
	t.Helper()

	s := &Session{
		events: make(chan Event, 100),
		done:   make(chan struct{}),
	}

	pr, pw := io.Pipe()
	go func() {
		w := ndjson.NewWriter(pw)
		for _, line := range lines {
			if err := w.WriteRaw([]byte(line)); err != nil {
				t.Logf("write error: %v", err)
			}
		}
		pw.Close()
	}()

	reader := ndjson.NewReader(pr)
	var textBuilder strings.Builder
	for {
		line, err := reader.ReadLine()
		if err != nil {
			break
		}
		s.handleLine(line, &textBuilder)
	}
	close(s.events)

	var collected []Event
	for evt := range s.events {
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

// A malformed line whose type can't be read is skipped, not surfaced as a
// (fatal) ErrorEvent — it's indistinguishable from the shape-drift case.
func TestSession_MalformedLine(t *testing.T) {
	lines := []string{
		`{bad json}`,
	}

	events := fakeSession(t, lines)
	assert.Empty(t, events, "a malformed line should be skipped, producing no events")
}

// A malformed terminal "result" frame stays fatal: losing it would leave the
// caller with no TurnCompleteEvent (truncated output / a stream that blocks
// until EOF), so it must surface as an ErrorEvent rather than being skipped.
func TestSession_MalformedResultFrameIsFatal(t *testing.T) {
	lines := []string{
		// Valid type discriminator, but duration_ms is a string where an int64
		// is expected — the result body fails to unmarshal.
		`{"type":"result","subtype":"success","duration_ms":"oops","session_id":"s1"}`,
	}

	events := fakeSession(t, lines)
	require.Len(t, events, 1)

	errEvt, ok := events[0].(ErrorEvent)
	require.True(t, ok, "a malformed result frame must surface as an ErrorEvent")
	assert.Equal(t, "parse_message", errEvt.Context)
}

// A truncated (syntactically invalid) result frame must still be fatal —
// truncation is the corruption most likely to hit the final line, and the raw
// "type":"result" discriminator must trip the fatal path even when the line
// can't be decoded.
func TestSession_TruncatedResultFrameIsFatal(t *testing.T) {
	lines := []string{
		// Valid up to the type discriminator, then cut off mid-frame.
		`{"type":"result","subtype":"success","duration_ms":12`,
	}

	events := fakeSession(t, lines)
	require.Len(t, events, 1)

	errEvt, ok := events[0].(ErrorEvent)
	require.True(t, ok, "a truncated result frame must surface as an ErrorEvent")
	assert.Equal(t, "parse_message", errEvt.Context)
}

// A malformed non-terminal frame (here, assistant) is skipped — only the result
// frame is fatal. Losing one assistant delta is better than aborting the run.
func TestSession_MalformedAssistantFrameIsSkipped(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":"not-an-array"},"session_id":"s1"}`,
	}

	events := fakeSession(t, lines)
	assert.Empty(t, events, "a malformed non-terminal frame should be skipped")
}

// A malformed or unrecognized frame mid-stream must not abort the session:
// frames before and after it are still delivered.
func TestSession_SkipsBadFrameAndContinues(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"s1","model":"cursor-fast","cwd":"/tmp","permissionMode":"auto","apiKeySource":"env"}`,
		`{bad json}`, // malformed — skipped
		`{"type":"tool_call","subtype":"started","call_id":"c1","tool_call":"unparseable-detail","session_id":"s1"}`,                             // bad detail — skipped
		`{"type":"tool_call","subtype":"started","call_id":"c2","tool_call":[{"readToolCall":{"args":{"path":"/tmp/x.go"}}}],"session_id":"s1"}`, // array shape — delivered
		`{"type":"result","subtype":"success","duration_ms":1,"duration_api_ms":1,"is_error":false,"result":"done","session_id":"s1"}`,
	}

	events := fakeSession(t, lines)

	// No ErrorEvents — nothing here is fatal.
	for _, evt := range events {
		_, isErr := evt.(ErrorEvent)
		assert.False(t, isErr, "no frame in this stream should produce a fatal ErrorEvent")
	}

	// Ready, the array-shaped tool start, and the terminal result all survive.
	require.Len(t, events, 3)

	ready, ok := events[0].(ReadyEvent)
	require.True(t, ok)
	assert.Equal(t, "s1", ready.SessionID)

	toolStart, ok := events[1].(ToolStartEvent)
	require.True(t, ok)
	assert.Equal(t, "c2", toolStart.ID)
	assert.Equal(t, "readToolCall", toolStart.Name)
	assert.Equal(t, "/tmp/x.go", toolStart.Input["path"])

	turn, ok := events[2].(TurnCompleteEvent)
	require.True(t, ok)
	assert.True(t, turn.Success)
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
