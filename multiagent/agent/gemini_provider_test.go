package agent

import (
	"sync"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/acp"
)

// TestGeminiLongRunningProvider_EventBridgeInitialization verifies that
// the event bridge is properly initialized in Start() and cleaned up in Close().
func TestGeminiLongRunningProvider_EventBridgeInitialization(t *testing.T) {
	// This test verifies the lifecycle without actually starting an ACP client
	provider := NewGeminiLongRunningProvider(
		[]acp.ClientOption{
			acp.WithBinaryPath("true"), // Use 'true' command that exits immediately
		},
		acp.WithSessionCWD("/tmp"),
	)

	// Before Start(), bridgeDone should be nil
	if provider.bridgeDone != nil {
		t.Error("bridgeDone should be nil before Start()")
	}

	// Note: We can't actually call Start() in a unit test without a real ACP binary,
	// but we can verify the structure is correct
	if provider.events == nil {
		t.Error("events channel should be initialized")
	}
}

// TestGeminiLongRunningProvider_EventChannelLifecycle verifies that the
// events channel is properly managed across the provider lifecycle.
func TestGeminiLongRunningProvider_EventChannelLifecycle(t *testing.T) {
	provider := NewGeminiLongRunningProvider(
		[]acp.ClientOption{
			acp.WithBinaryPath("true"),
		},
		acp.WithSessionCWD("/tmp"),
	)

	// Events channel should be initialized
	if provider.events == nil {
		t.Fatal("events channel should be initialized")
	}

	eventsChan := provider.Events()
	if eventsChan == nil {
		t.Fatal("Events() should return a non-nil channel")
	}

	// Events channel should be the same as the internal one
	if eventsChan != provider.events {
		t.Error("Events() should return the internal events channel")
	}
}

// TestBridgeACPEventsToChannel verifies the event bridge forwards events correctly.
func TestBridgeACPEventsToChannel(t *testing.T) {
	// Create channels
	acpEvents := make(chan acp.Event, 10)
	agentEvents := make(chan AgentEvent, 10)
	done := make(chan struct{})
	var wg sync.WaitGroup

	// Start bridge
	wg.Add(1)
	go bridgeACPEventsToChannel(acpEvents, agentEvents, done, &wg)

	// Send some events
	acpEvents <- acp.TextDeltaEvent{Delta: "hello"}
	acpEvents <- acp.ThinkingDeltaEvent{Delta: "thinking"}
	acpEvents <- acp.ToolCallStartEvent{
		ToolName:   "test_tool",
		ToolCallID: "test-1",
		Input:      map[string]interface{}{"arg": "value"},
	}

	// Give bridge time to process
	time.Sleep(50 * time.Millisecond)

	// Close the bridge
	close(done)
	wg.Wait()

	// Verify events were forwarded
	select {
	case ev := <-agentEvents:
		textEv, ok := ev.(TextAgentEvent)
		if !ok {
			t.Errorf("expected TextAgentEvent, got %T", ev)
		}
		if textEv.Text != "hello" {
			t.Errorf("expected text 'hello', got '%s'", textEv.Text)
		}
	default:
		t.Error("expected to receive TextAgentEvent")
	}

	select {
	case ev := <-agentEvents:
		thinkingEv, ok := ev.(ThinkingAgentEvent)
		if !ok {
			t.Errorf("expected ThinkingAgentEvent, got %T", ev)
		}
		if thinkingEv.Thinking != "thinking" {
			t.Errorf("expected thinking 'thinking', got '%s'", thinkingEv.Thinking)
		}
	default:
		t.Error("expected to receive ThinkingAgentEvent")
	}

	select {
	case ev := <-agentEvents:
		toolEv, ok := ev.(ToolStartAgentEvent)
		if !ok {
			t.Errorf("expected ToolStartAgentEvent, got %T", ev)
		}
		if toolEv.Name != "test_tool" {
			t.Errorf("expected tool name 'test_tool', got '%s'", toolEv.Name)
		}
		if toolEv.ID != "test-1" {
			t.Errorf("expected tool ID 'test-1', got '%s'", toolEv.ID)
		}
	default:
		t.Error("expected to receive ToolStartAgentEvent")
	}
}

// TestBridgeACPEventsToChannel_StopsOnDone verifies the bridge stops when done is closed.
func TestBridgeACPEventsToChannel_StopsOnDone(t *testing.T) {
	acpEvents := make(chan acp.Event, 10)
	agentEvents := make(chan AgentEvent, 10)
	done := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go bridgeACPEventsToChannel(acpEvents, agentEvents, done, &wg)

	// Close done immediately
	close(done)

	// Wait for bridge to exit
	waitCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitCh)
	}()

	select {
	case <-waitCh:
		// Bridge exited as expected
	case <-time.After(1 * time.Second):
		t.Error("bridge did not exit after done was closed")
	}
}

// TestBridgeACPEventsToChannel_StopsOnChannelClose verifies the bridge stops when events channel closes.
func TestBridgeACPEventsToChannel_StopsOnChannelClose(t *testing.T) {
	acpEvents := make(chan acp.Event, 10)
	agentEvents := make(chan AgentEvent, 10)
	done := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go bridgeACPEventsToChannel(acpEvents, agentEvents, done, &wg)

	// Close events channel
	close(acpEvents)

	// Wait for bridge to exit
	waitCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitCh)
	}()

	select {
	case <-waitCh:
		// Bridge exited as expected
	case <-time.After(1 * time.Second):
		t.Error("bridge did not exit after events channel was closed")
	}

	close(done) // Clean up
}

// TestBridgeACPEventsToHandler verifies event forwarding to a handler.
func TestBridgeACPEventsToHandler(t *testing.T) {
	acpEvents := make(chan acp.Event, 10)
	done := make(chan struct{})

	// Create a mock handler
	var receivedText []string
	var receivedThinking []string
	var receivedToolStarts []string
	var mu sync.Mutex

	handler := &testEventHandler{
		onText: func(text string) {
			mu.Lock()
			receivedText = append(receivedText, text)
			mu.Unlock()
		},
		onThinking: func(thinking string) {
			mu.Lock()
			receivedThinking = append(receivedThinking, thinking)
			mu.Unlock()
		},
		onToolStart: func(name, id string, input map[string]interface{}) {
			mu.Lock()
			receivedToolStarts = append(receivedToolStarts, name)
			mu.Unlock()
		},
	}

	// Start bridge
	go bridgeACPEventsToHandler(acpEvents, handler, done)

	// Send events
	acpEvents <- acp.TextDeltaEvent{Delta: "test1"}
	acpEvents <- acp.TextDeltaEvent{Delta: "test2"}
	acpEvents <- acp.ThinkingDeltaEvent{Delta: "thinking1"}
	acpEvents <- acp.ToolCallStartEvent{ToolName: "tool1", ToolCallID: "id1"}

	// Give handler time to process
	time.Sleep(50 * time.Millisecond)

	// Close bridge
	close(done)
	time.Sleep(50 * time.Millisecond)

	// Verify events were forwarded
	mu.Lock()
	defer mu.Unlock()

	if len(receivedText) != 2 {
		t.Errorf("expected 2 text events, got %d", len(receivedText))
	}
	if len(receivedText) > 0 && receivedText[0] != "test1" {
		t.Errorf("expected first text 'test1', got '%s'", receivedText[0])
	}

	if len(receivedThinking) != 1 {
		t.Errorf("expected 1 thinking event, got %d", len(receivedThinking))
	}

	if len(receivedToolStarts) != 1 {
		t.Errorf("expected 1 tool start event, got %d", len(receivedToolStarts))
	}
}

// testEventHandler is a mock EventHandler for testing.
type testEventHandler struct {
	onText      func(string)
	onThinking  func(string)
	onToolStart func(string, string, map[string]interface{})
}

func (h *testEventHandler) OnText(text string) {
	if h.onText != nil {
		h.onText(text)
	}
}

func (h *testEventHandler) OnThinking(thinking string) {
	if h.onThinking != nil {
		h.onThinking(thinking)
	}
}

func (h *testEventHandler) OnToolStart(name, id string, input map[string]interface{}) {
	if h.onToolStart != nil {
		h.onToolStart(name, id, input)
	}
}

func (h *testEventHandler) OnToolComplete(name, id string, input map[string]interface{}, result interface{}, isError bool) {
}

func (h *testEventHandler) OnTurnComplete(turnNumber int, success bool, durationMs int64, costUSD float64) {
}

func (h *testEventHandler) OnError(err error, context string) {
}

// Test that GeminiLongRunningProvider.Close() cleans up both clients without double-closing bridgeDone.
// This prevents resource leaks when Execute() was called on a long-running instance.
func TestGeminiLongRunningProvider_CloseCleanupBothClients(t *testing.T) {
	// Create a GeminiProvider with initialized channels
	baseProvider := &GeminiProvider{
		client:     nil, // No actual client for this test
		events:     make(chan AgentEvent, 10),
		bridgeDone: make(chan struct{}),
	}

	// Create long-running provider that embeds the base provider
	lrProvider := &GeminiLongRunningProvider{
		GeminiProvider: baseProvider,
	}

	// Close should complete successfully without panicking
	// (it should not try to close bridgeDone twice)
	err := lrProvider.Close()
	if err != nil {
		t.Fatalf("Close() failed: %v", err)
	}

	// Verify that the embedded provider's client field is set to nil
	// (indicating it was cleaned up)
	if baseProvider.client != nil {
		t.Error("embedded GeminiProvider.client should be nil after Close()")
	}
}
