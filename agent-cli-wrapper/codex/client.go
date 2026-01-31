package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Client manages the Codex app-server subprocess and provides
// a high-level API for interacting with Codex.
type Client struct {
	pending     map[int64]chan *rpcResult
	process     *processManager
	state       *clientStateManager
	threads     map[string]*Thread
	readyBefore map[string]struct{} // MCP startup received before thread registered
	idGen       *idGenerator
	events      chan Event
	accumulator *streamAccumulator
	info        *ConnectionInfo
	done        chan struct{}
	config      ClientConfig
	mu          sync.RWMutex
	started     bool
	stopping    bool
}

// ConnectionInfo contains connection metadata after initialization.
type ConnectionInfo struct {
	UserAgent string
}

// rpcResult holds the result of a JSON-RPC request.
type rpcResult struct {
	Response *JSONRPCResponse
	Error    error
}

// NewClient creates a new Codex client with options.
func NewClient(opts ...ClientOption) *Client {
	config := defaultClientConfig()
	for _, opt := range opts {
		opt(&config)
	}

	return &Client{
		config:      config,
		state:       newClientStateManager(),
		threads:     make(map[string]*Thread),
		readyBefore: make(map[string]struct{}),
		idGen:       &idGenerator{},
		pending:     make(map[int64]chan *rpcResult),
		events:      make(chan Event, config.EventBufferSize),
		accumulator: newStreamAccumulator(),
		done:        make(chan struct{}),
	}
}

// Start spawns the app-server process and initializes the client.
func (c *Client) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.started {
		return ErrAlreadyStarted
	}

	// Transition state
	if err := c.state.SetStarting(); err != nil {
		return err
	}

	// Create and start process manager
	c.process = newProcessManager(c.config)
	if err := c.process.Start(ctx); err != nil {
		return err
	}

	// Start stderr handler if configured
	if c.config.StderrHandler != nil {
		c.process.startStderrReader(c.config.StderrHandler)
	}

	// Start message reading goroutine
	go c.readLoop(ctx)

	c.started = true

	// Send initialize request
	c.mu.Unlock()
	err := c.initialize(ctx)
	c.mu.Lock()

	if err != nil {
		c.process.Stop()
		return err
	}

	return nil
}

// initialize sends the initialize request.
func (c *Client) initialize(ctx context.Context) error {
	params := InitializeParams{
		ClientInfo: ClientInfo{
			Name:    c.config.ClientName,
			Version: c.config.ClientVersion,
		},
	}

	resp, err := c.sendRequestAndWait(ctx, "initialize", params)
	if err != nil {
		return err
	}

	// Parse response
	var initResp InitializeResponse
	if err := unmarshalRaw(resp.Result, &initResp); err != nil {
		return &ProtocolError{Message: "failed to parse initialize response", Cause: err}
	}

	c.mu.Lock()
	c.info = &ConnectionInfo{
		UserAgent: initResp.UserAgent,
	}
	_ = c.state.SetReady()
	c.mu.Unlock()

	// Emit ready event
	c.emit(ClientReadyEvent{UserAgent: initResp.UserAgent})

	return nil
}

// Stop gracefully shuts down the client.
func (c *Client) Stop() error {
	c.mu.Lock()
	if !c.started || c.stopping {
		c.mu.Unlock()
		return nil
	}
	c.stopping = true
	c.mu.Unlock()

	// Close done channel to signal goroutines
	close(c.done)

	// Stop the process
	if c.process != nil {
		c.process.Stop()
	}

	// Set closed state
	c.state.SetClosed()

	// Close all threads
	c.mu.Lock()
	for _, thread := range c.threads {
		thread.Close()
	}
	c.mu.Unlock()

	// Close event channel
	close(c.events)

	return nil
}

// Events returns a read-only channel for receiving events.
func (c *Client) Events() <-chan Event {
	return c.events
}

// State returns the current client state.
func (c *Client) State() ClientState {
	return c.state.Current()
}

// Info returns client information (available after ClientReadyEvent).
func (c *Client) Info() *ConnectionInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.info
}

// CreateThread creates a new conversation thread.
func (c *Client) CreateThread(ctx context.Context, opts ...ThreadOption) (*Thread, error) {
	c.mu.RLock()
	if !c.started {
		c.mu.RUnlock()
		return nil, ErrNotStarted
	}
	if c.stopping {
		c.mu.RUnlock()
		return nil, ErrStopping
	}
	c.mu.RUnlock()

	// Build thread config
	cfg := defaultThreadConfig()
	for _, opt := range opts {
		opt(&cfg)
	}

	// Build params
	params := ThreadStartParams{
		Model:         cfg.Model,
		ModelProvider: cfg.ModelProvider,
		Profile:       cfg.Profile,
		CWD:           cfg.WorkDir,
		Sandbox:       cfg.Sandbox,
		Config:        cfg.Config,
	}

	if cfg.ApprovalPolicy != "" {
		params.ApprovalPolicy = string(cfg.ApprovalPolicy)
	}

	// Send thread/start request
	resp, err := c.sendRequestAndWait(ctx, "thread/start", params)
	if err != nil {
		return nil, err
	}

	// Parse response
	var threadResp ThreadStartResponse
	if err := unmarshalRaw(resp.Result, &threadResp); err != nil {
		return nil, &ProtocolError{Message: "failed to parse thread/start response", Cause: err}
	}

	// Create thread object
	thread := newThread(c, threadResp.Thread.ID, cfg)
	thread.setInfo(&threadResp.Thread)

	// Register thread and apply any early MCP startup signal
	c.mu.Lock()
	c.threads[thread.id] = thread
	_, wasReadyBefore := c.readyBefore[thread.id]
	delete(c.readyBefore, thread.id)
	c.mu.Unlock()

	if wasReadyBefore {
		thread.setReady()
	}

	// Emit thread started event
	c.emit(ThreadStartedEvent{
		ThreadID:      threadResp.Thread.ID,
		Model:         threadResp.Model,
		ModelProvider: threadResp.ModelProvider,
		WorkDir:       threadResp.CWD,
	})

	return thread, nil
}

// GetThread retrieves an existing thread by ID.
func (c *Client) GetThread(threadID string) (*Thread, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	thread, ok := c.threads[threadID]
	return thread, ok
}

// ListThreads returns all active threads.
func (c *Client) ListThreads() []*Thread {
	c.mu.RLock()
	defer c.mu.RUnlock()

	threads := make([]*Thread, 0, len(c.threads))
	for _, t := range c.threads {
		threads = append(threads, t)
	}
	return threads
}

// CloseThread closes a specific thread.
func (c *Client) CloseThread(threadID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	thread, ok := c.threads[threadID]
	if !ok {
		return ErrThreadNotFound
	}

	thread.Close()
	delete(c.threads, threadID)
	delete(c.readyBefore, threadID)
	c.accumulator.RemoveThread(threadID)
	return nil
}

// Ask is a convenience method that creates a thread, sends a message,
// waits for completion, and returns the response text.
// This is the simplest API for one-shot queries.
func (c *Client) Ask(ctx context.Context, prompt string, opts ...ThreadOption) (string, error) {
	// Create thread
	thread, err := c.CreateThread(ctx, opts...)
	if err != nil {
		return "", err
	}

	// Wait for thread to be ready (MCP startup)
	if err := c.waitForThreadReady(ctx, thread.ID()); err != nil {
		return "", err
	}

	// Send message and wait for response
	result, err := thread.Ask(ctx, prompt)
	if err != nil {
		return "", err
	}

	if !result.Success {
		return "", result.Error
	}

	return result.FullText, nil
}

// AskWithTimeout is a convenience wrapper with timeout context.
func (c *Client) AskWithTimeout(prompt string, timeout time.Duration, opts ...ThreadOption) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.Ask(ctx, prompt, opts...)
}

// waitForThreadReady waits for a thread to receive the MCP startup complete notification.
func (c *Client) waitForThreadReady(ctx context.Context, threadID string) error {
	thread, ok := c.GetThread(threadID)
	if !ok {
		return ErrThreadNotFound
	}

	// If already ready, return immediately
	if thread.State() == ThreadStateReady {
		return nil
	}

	// Wait for ready state by polling events
	// In a production implementation, this would use a proper wait mechanism
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if thread.State() == ThreadStateReady {
				return nil
			}
			if thread.State() == ThreadStateClosed {
				return ErrClientClosed
			}
		}
	}
}

// readLoop reads and processes messages from the app-server.
func (c *Client) readLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		default:
			line, err := c.process.ReadLine()
			if err != nil {
				if err == io.EOF {
					return
				}
				if !c.stopping {
					c.emitError("", "", err, "read_line")
				}
				return
			}

			c.handleMessage(line)
		}
	}
}

// handleMessage processes a single JSON-RPC message.
func (c *Client) handleMessage(line []byte) {
	// Try to determine if this is a response or notification
	var base struct {
		ID     *int64 `json:"id,omitempty"`
		Method string `json:"method,omitempty"`
	}
	if err := json.Unmarshal(line, &base); err != nil {
		c.emitError("", "", &ProtocolError{Message: "failed to parse message", Line: string(line), Cause: err}, "parse_message")
		return
	}

	if base.ID != nil {
		// This is a response
		c.handleResponse(line, *base.ID)
	} else if base.Method != "" {
		// This is a notification
		c.handleNotification(line, base.Method)
	}
}

// handleResponse processes a JSON-RPC response.
func (c *Client) handleResponse(line []byte, id int64) {
	var resp JSONRPCResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		c.emitError("", "", &ProtocolError{Message: "failed to parse response", Line: string(line), Cause: err}, "parse_response")
		return
	}

	c.mu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()

	if ok {
		result := &rpcResult{Response: &resp}
		if resp.Error != nil {
			result.Error = &RPCError{Code: resp.Error.Code, Message: resp.Error.Message}
		}
		select {
		case ch <- result:
		default:
		}
	}
}

// handleNotification processes a JSON-RPC notification.
func (c *Client) handleNotification(line []byte, method string) {
	var notif JSONRPCNotification
	if err := json.Unmarshal(line, &notif); err != nil {
		c.emitError("", "", &ProtocolError{Message: "failed to parse notification", Line: string(line), Cause: err}, "parse_notification")
		return
	}

	switch method {
	case NotifyThreadStarted:
		c.handleThreadStarted(notif.Params)

	case NotifyTurnStarted:
		c.handleTurnStarted(notif.Params)

	case NotifyTurnCompleted:
		c.handleTurnCompleted(notif.Params)

	case NotifyAgentMessageDelta:
		c.handleAgentMessageDelta(notif.Params)

	case NotifyCodexEventMCPStartup:
		c.handleMCPStartupComplete(notif.Params)

	case NotifyCodexEventTokenCount:
		c.handleTokenCount(notif.Params)

	case NotifyCodexEventError:
		c.handleCodexError(notif.Params)

	case NotifyItemStarted:
		c.handleItemStarted(notif.Params)

	case NotifyItemCompleted:
		c.handleItemCompleted(notif.Params)

	case NotifyCodexEventExecBegin:
		c.handleExecCommandBegin(notif.Params)

	case NotifyCodexEventExecEnd:
		c.handleExecCommandEnd(notif.Params)

	case NotifyCodexEventExecOutput:
		c.handleExecCommandOutput(notif.Params)

	case NotifyCodexEventReasoningDelta:
		c.handleReasoningDelta(notif.Params)
	}
}

func (c *Client) handleThreadStarted(params json.RawMessage) {
	var notif ThreadStartedNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}

	c.mu.Lock()
	thread, ok := c.threads[notif.Thread.ID]
	c.mu.Unlock()

	if ok {
		thread.setInfo(&notif.Thread)
	}
}

func (c *Client) handleTurnStarted(params json.RawMessage) {
	var notif TurnStartedNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}

	c.mu.RLock()
	thread, ok := c.threads[notif.ThreadID]
	c.mu.RUnlock()

	if ok {
		thread.handleTurnStarted(notif.Turn.ID)
	}

	c.emit(TurnStartedEvent{
		ThreadID: notif.ThreadID,
		TurnID:   notif.Turn.ID,
	})
}

func (c *Client) handleTurnCompleted(params json.RawMessage) {
	var notif TurnCompletedNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}

	c.mu.RLock()
	thread, ok := c.threads[notif.ThreadID]
	c.mu.RUnlock()

	success := notif.Turn.Status == "completed"
	var errMsg string
	if notif.Turn.Error != nil {
		if s, ok := notif.Turn.Error.(string); ok {
			errMsg = s
		}
	}

	fullText := ""
	if ok {
		thread.handleTurnCompleted(notif.Turn.ID, success, errMsg)
		fullText = thread.GetFullText()
	}

	c.emit(TurnCompletedEvent{
		ThreadID: notif.ThreadID,
		TurnID:   notif.Turn.ID,
		Success:  success,
		FullText: fullText,
	})
}

func (c *Client) handleAgentMessageDelta(params json.RawMessage) {
	var notif AgentMessageDeltaNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}

	c.mu.RLock()
	thread, ok := c.threads[notif.ThreadID]
	c.mu.RUnlock()

	var fullText string
	if ok {
		fullText = thread.handleTextDelta(notif.TurnID, notif.ItemID, notif.Delta)
	} else {
		fullText = c.accumulator.HandleDelta(notif.ThreadID, notif.TurnID, notif.ItemID, notif.Delta)
	}

	c.emit(TextDeltaEvent{
		ThreadID: notif.ThreadID,
		TurnID:   notif.TurnID,
		ItemID:   notif.ItemID,
		Delta:    notif.Delta,
		FullText: fullText,
	})
}

func (c *Client) handleMCPStartupComplete(params json.RawMessage) {
	var notif CodexEventNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}

	threadID := notif.ConversationID

	c.mu.Lock()
	thread, ok := c.threads[threadID]
	if !ok {
		// Thread not registered yet - remember so CreateThread can apply it
		c.readyBefore[threadID] = struct{}{}
	}
	c.mu.Unlock()

	if ok {
		thread.setReady()
	}
	c.emit(ThreadReadyEvent{ThreadID: threadID})
}

func (c *Client) handleTokenCount(params json.RawMessage) {
	var notif CodexEventNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}

	var msg TokenCountMsg
	if err := json.Unmarshal(notif.Msg, &msg); err != nil {
		return
	}

	var totalUsage, lastUsage *TokenUsage
	if msg.Info != nil {
		if msg.Info.TotalTokenUsage != nil {
			totalUsage = &TokenUsage{
				InputTokens:           msg.Info.TotalTokenUsage.InputTokens,
				CachedInputTokens:     msg.Info.TotalTokenUsage.CachedInputTokens,
				OutputTokens:          msg.Info.TotalTokenUsage.OutputTokens,
				ReasoningOutputTokens: msg.Info.TotalTokenUsage.ReasoningOutputTokens,
				TotalTokens:           msg.Info.TotalTokenUsage.TotalTokens,
			}
		}
		if msg.Info.LastTokenUsage != nil {
			lastUsage = &TokenUsage{
				InputTokens:           msg.Info.LastTokenUsage.InputTokens,
				CachedInputTokens:     msg.Info.LastTokenUsage.CachedInputTokens,
				OutputTokens:          msg.Info.LastTokenUsage.OutputTokens,
				ReasoningOutputTokens: msg.Info.LastTokenUsage.ReasoningOutputTokens,
				TotalTokens:           msg.Info.LastTokenUsage.TotalTokens,
			}
		}
	}

	c.emit(TokenUsageEvent{
		ThreadID:   notif.ConversationID,
		TotalUsage: totalUsage,
		LastUsage:  lastUsage,
		RateLimits: msg.RateLimits,
	})
}

func (c *Client) handleCodexError(params json.RawMessage) {
	var notif CodexEventNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}

	threadID := notif.ConversationID
	protocolErr := &ProtocolError{Message: string(notif.Msg)}

	// Set error on thread to wake any waiters
	c.mu.RLock()
	thread, ok := c.threads[threadID]
	c.mu.RUnlock()
	if ok {
		thread.setError(protocolErr)
	}

	c.emitError(threadID, "", protocolErr, "codex_event_error")
}

func (c *Client) handleItemStarted(params json.RawMessage) {
	var notif ItemNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}

	// Parse item to get type
	var item struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(notif.Item, &item); err != nil {
		return
	}

	c.emit(ItemStartedEvent{
		ThreadID: notif.ThreadID,
		TurnID:   notif.TurnID,
		ItemID:   item.ID,
		ItemType: item.Type,
	})
}

func (c *Client) handleItemCompleted(params json.RawMessage) {
	var notif ItemNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}

	// Parse item
	var item struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	}
	if err := json.Unmarshal(notif.Item, &item); err != nil {
		return
	}

	c.emit(ItemCompletedEvent{
		ThreadID: notif.ThreadID,
		TurnID:   notif.TurnID,
		ItemID:   item.ID,
		ItemType: item.Type,
		Text:     item.Text,
	})
}

// sendRequestAndWait sends a request and waits for the response.
func (c *Client) sendRequestAndWait(ctx context.Context, method string, params interface{}) (*JSONRPCResponse, error) {
	id := c.idGen.Next()

	req, err := newRequest(id, method, params)
	if err != nil {
		return nil, err
	}

	// Create response channel
	ch := make(chan *rpcResult, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	// Send request
	if err := c.process.WriteJSON(req); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}

	// Wait for response
	select {
	case result := <-ch:
		if result.Error != nil {
			return nil, result.Error
		}
		return result.Response, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

// emit sends an event to the events channel.
func (c *Client) emit(event Event) {
	select {
	case c.events <- event:
	default:
		// Channel full, drop event
	}
}

// emitError emits an error event.
func (c *Client) emitError(threadID, turnID string, err error, context string) {
	c.emit(ErrorEvent{
		ThreadID:  threadID,
		TurnID:    turnID,
		Error:     err,
		Context:   context,
		Timestamp: time.Now(),
	})
}

func (c *Client) handleExecCommandBegin(params json.RawMessage) {
	var notif CodexEventNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}

	var msg ExecCommandBeginMsg
	if err := json.Unmarshal(notif.Msg, &msg); err != nil {
		return
	}

	// Extract simplified command for display
	parsedCmd := ""
	if len(msg.ParsedCmd) > 0 {
		parsedCmd = msg.ParsedCmd[0].Cmd
	} else if len(msg.Command) >= 3 {
		// Command is typically ["/bin/zsh", "-lc", "actual command"]
		parsedCmd = msg.Command[2]
	}

	c.emit(CommandStartEvent{
		ThreadID:  notif.ConversationID,
		TurnID:    msg.TurnID,
		CallID:    msg.CallID,
		Command:   msg.Command,
		CWD:       msg.CWD,
		ParsedCmd: parsedCmd,
	})
}

func (c *Client) handleExecCommandEnd(params json.RawMessage) {
	var notif CodexEventNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}

	var msg ExecCommandEndMsg
	if err := json.Unmarshal(notif.Msg, &msg); err != nil {
		return
	}

	// Convert duration to milliseconds
	durationMs := msg.Duration.Secs*1000 + msg.Duration.Nanos/1000000

	c.emit(CommandEndEvent{
		ThreadID:   notif.ConversationID,
		TurnID:     msg.TurnID,
		CallID:     msg.CallID,
		ExitCode:   msg.ExitCode,
		Stdout:     msg.Stdout,
		Stderr:     msg.Stderr,
		DurationMs: durationMs,
	})
}

func (c *Client) handleExecCommandOutput(params json.RawMessage) {
	var notif CodexEventNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}

	var msg ExecCommandOutputMsg
	if err := json.Unmarshal(notif.Msg, &msg); err != nil {
		return
	}

	// The Codex protocol always base64-encodes the chunk field
	// (see codex-rs/protocol/src/protocol.rs ExecCommandOutputDeltaEvent)
	chunk := msg.Chunk
	if decoded, err := base64.StdEncoding.DecodeString(msg.Chunk); err == nil {
		chunk = string(decoded)
	}

	c.emit(CommandOutputEvent{
		ThreadID: notif.ConversationID,
		CallID:   msg.CallID,
		Stream:   msg.Stream,
		Chunk:    chunk,
	})
}

func (c *Client) handleReasoningDelta(params json.RawMessage) {
	var notif CodexEventNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}

	var msg ReasoningDeltaMsg
	if err := json.Unmarshal(notif.Msg, &msg); err != nil {
		return
	}

	c.emit(ReasoningDeltaEvent{
		ThreadID: notif.ConversationID,
		Delta:    msg.Delta,
	})
}

// unmarshalRaw is a helper to unmarshal json.RawMessage.
func unmarshalRaw(raw json.RawMessage, v interface{}) error {
	return json.Unmarshal(raw, v)
}
