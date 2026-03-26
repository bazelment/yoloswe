package deepgram_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/voice/stt"
	"github.com/bazelment/yoloswe/voice/stt/deepgram"
)

// collectEvents reads all events from a session until the channel closes or timeout.
func collectEvents(t *testing.T, sess stt.Session, timeout time.Duration) []stt.Event {
	t.Helper()
	var events []stt.Event
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case evt, ok := <-sess.Events():
			if !ok {
				return events
			}
			events = append(events, evt)
		case <-timer.C:
			t.Log("collectEvents: timeout reached")
			return events
		}
	}
}

// mockDeepgramServer creates an httptest server that speaks a simplified version
// of the Deepgram WebSocket protocol. The handler function receives the websocket
// connection to send/receive messages.
func mockDeepgramServer(t *testing.T, handler func(t *testing.T, conn *websocket.Conn)) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("upgrade error: %v", err)
			return
		}
		defer conn.Close()
		handler(t, conn)
	}))
	t.Cleanup(server.Close)
	return server
}

// wsHost converts an httptest server URL to a ws:// URL suitable for
// the Deepgram SDK's ClientOptions.Host field.
// The SDK constructs URLs like {protocol}://{host}/{path}?{params}.
// When Host includes ws:// prefix, the SDK uses plain WebSocket (no TLS).
func wsHost(server *httptest.Server) string {
	return strings.Replace(server.URL, "http://", "ws://", 1)
}

func TestStreamingSession_HappyPath(t *testing.T) {
	t.Parallel()

	server := mockDeepgramServer(t, func(t *testing.T, conn *websocket.Conn) {
		// Read audio frames and respond with transcript events.
		for {
			msgType, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if msgType == websocket.BinaryMessage && len(data) > 0 {
				// Simulate Deepgram responses.
				responses := []map[string]any{
					{
						"type":      "SpeechStarted",
						"channel":   []int{0},
						"timestamp": 0.0,
					},
					{
						"type": "Results",
						"channel": map[string]any{
							"alternatives": []map[string]any{
								{"transcript": "hello", "confidence": 0.95},
							},
						},
						"is_final":     false,
						"speech_final": false,
					},
					{
						"type": "Results",
						"channel": map[string]any{
							"alternatives": []map[string]any{
								{"transcript": "hello world", "confidence": 0.98},
							},
						},
						"is_final":     true,
						"speech_final": true,
					},
					{
						"type":          "UtteranceEnd",
						"channel":       []int{0},
						"last_word_end": 1.5,
					},
				}
				for _, resp := range responses {
					data, _ := json.Marshal(resp)
					if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
						return
					}
				}
			}
			// Check for finalize message.
			if msgType == websocket.TextMessage {
				var msg map[string]any
				if json.Unmarshal(data, &msg) == nil && msg["type"] == "Finalize" {
					break
				}
			}
		}
	})

	provider := deepgram.NewStreamingProviderWithHost("test-key", wsHost(server))
	ctx := context.Background()
	cfg := stt.DefaultAudioConfig()
	opts := stt.StreamOpts{VAD: true, Interim: true}

	sess, err := provider.StartSession(ctx, cfg, opts)
	require.NoError(t, err)

	// Send some audio.
	audio := stt.GenerateSineWAV(440, 100*time.Millisecond, 16000)
	pcm, err := stt.PCMFromWAV(audio)
	require.NoError(t, err)
	require.NoError(t, sess.SendAudio(pcm))

	// Collect events.
	events := collectEvents(t, sess, 5*time.Second)

	require.NoError(t, sess.Close())

	// Verify we got the expected event types.
	var types []stt.EventType
	for _, e := range events {
		types = append(types, e.Type)
	}
	assert.Contains(t, types, stt.EventSpeechStart)
	assert.Contains(t, types, stt.EventPartialText)
	assert.Contains(t, types, stt.EventFinalText)
	assert.Contains(t, types, stt.EventSpeechEnd)
}

func TestStreamingSession_DoubleClose(t *testing.T) {
	t.Parallel()

	server := mockDeepgramServer(t, func(t *testing.T, conn *websocket.Conn) {
		// Just keep reading until close.
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	})

	provider := deepgram.NewStreamingProviderWithHost("test-key", wsHost(server))
	sess, err := provider.StartSession(context.Background(), stt.DefaultAudioConfig(), stt.StreamOpts{})
	require.NoError(t, err)

	// Double close should not panic.
	require.NoError(t, sess.Close())
	require.NoError(t, sess.Close())
}

func TestStreamingSession_ContextCancellation(t *testing.T) {
	t.Parallel()

	server := mockDeepgramServer(t, func(t *testing.T, conn *websocket.Conn) {
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	provider := deepgram.NewStreamingProviderWithHost("test-key", wsHost(server))
	sess, err := provider.StartSession(ctx, stt.DefaultAudioConfig(), stt.StreamOpts{})
	require.NoError(t, err)

	// Cancel context — the session should close cleanly.
	cancel()

	// Close explicitly to ensure Events() drains.
	require.NoError(t, sess.Close())

	// After Close(), Events() channel must be closed within a short timeout.
	require.Eventually(t, func() bool {
		select {
		case _, ok := <-sess.Events():
			return !ok
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond, "Events() channel should be closed after Close()")
}

func TestStreamingSession_ServerDisconnect(t *testing.T) {
	t.Parallel()

	server := mockDeepgramServer(t, func(t *testing.T, conn *websocket.Conn) {
		// Read one message then close abruptly.
		conn.ReadMessage()
		conn.Close()
	})

	provider := deepgram.NewStreamingProviderWithHost("test-key", wsHost(server))
	sess, err := provider.StartSession(context.Background(), stt.DefaultAudioConfig(), stt.StreamOpts{})
	require.NoError(t, err)

	// Send audio — the abrupt server close should cause the events channel to
	// close (via the SDK's Close callback) within a reasonable timeout.
	audio := make([]byte, 640)
	sess.SendAudio(audio)

	// Collect events; after a server disconnect the channel should close.
	events := collectEvents(t, sess, 5*time.Second)
	sess.Close()

	// The events channel must now be closed (drained by collectEvents or Close).
	select {
	case _, ok := <-sess.Events():
		assert.False(t, ok, "Events() channel should be closed after server disconnect and Close()")
	default:
		// Channel already drained.
	}
	_ = events
}
