package acp

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
)

// extractToolName extracts the tool name from a toolCallId.
// Gemini CLI uses format "tool_name-timestamp" (e.g. "write_file-1770849300776").
func extractToolName(toolCallID string) string {
	if idx := strings.LastIndex(toolCallID, "-"); idx > 0 {
		return toolCallID[:idx]
	}
	return toolCallID
}

// Client manages an ACP-compatible agent subprocess and provides
// a high-level API for interacting with ACP agents (Gemini CLI, etc.).
type Client struct {
	pending   map[int64]chan *rpcResult
	sessions  map[string]*Session
	process   *processManager
	state     *clientStateManager
	idGen     *idGenerator
	agentInfo *InitializeResponse
	events    chan Event
	done      chan struct{}
	config    ClientConfig
	mu        sync.RWMutex
	readWg    sync.WaitGroup // tracks readLoop goroutine
	started   bool
	stopping  bool
}

// rpcResult holds the result of a JSON-RPC request.
type rpcResult struct {
	Response *JSONRPCResponse
	Error    error
}

// NewClient creates a new ACP client with options.
func NewClient(opts ...ClientOption) *Client {
	config := defaultACPClientConfig()
	for _, opt := range opts {
		opt(&config)
	}

	// Apply default handlers if not set
	if config.FsHandler == nil {
		config.FsHandler = &DefaultFsHandler{}
	}
	if config.TerminalHandler == nil {
		config.TerminalHandler = NewDefaultTerminalHandler()
	}
	if config.PermissionHandler == nil {
		config.PermissionHandler = &BypassPermissionHandler{}
	}

	return &Client{
		config:   config,
		state:    newClientStateManager(),
		sessions: make(map[string]*Session),
		idGen:    &idGenerator{},
		pending:  make(map[int64]chan *rpcResult),
		events:   make(chan Event, config.EventBufferSize),
		done:     make(chan struct{}),
	}
}

// Start spawns the agent process and initializes the ACP connection.
func (c *Client) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.started {
		return ErrAlreadyStarted
	}

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
	c.readWg.Add(1)
	go c.readLoop(ctx)

	c.started = true

	// Send initialize request (release lock during blocking call)
	c.mu.Unlock()
	err := c.initialize(ctx)
	c.mu.Lock()

	if err != nil {
		c.started = false
		c.process.Stop()
		return err
	}

	return nil
}

// initialize sends the ACP initialize request.
func (c *Client) initialize(ctx context.Context) error {
	params := InitializeRequest{
		ProtocolVersion: ProtocolVersion,
		ClientInfo: &Implementation{
			Name:    c.config.ClientName,
			Version: c.config.ClientVersion,
		},
		ClientCapabilities: &ClientCapabilities{
			Fs: &FsCapability{
				ReadTextFile:  true,
				WriteTextFile: true,
			},
			Terminal: true,
		},
	}

	resp, err := c.sendRequestAndWait(ctx, MethodInitialize, params)
	if err != nil {
		return err
	}

	var initResp InitializeResponse
	if err := json.Unmarshal(resp.Result, &initResp); err != nil {
		return &ProtocolError{Message: "failed to parse initialize response", Cause: err}
	}

	c.mu.Lock()
	c.agentInfo = &initResp
	_ = c.state.SetReady()
	c.mu.Unlock()

	agentName := ""
	agentVersion := ""
	if initResp.AgentInfo != nil {
		agentName = initResp.AgentInfo.Name
		agentVersion = initResp.AgentInfo.Version
	}
	c.emit(ClientReadyEvent{AgentName: agentName, AgentVersion: agentVersion})

	return nil
}

// NewSession creates a new ACP session.
func (c *Client) NewSession(ctx context.Context, opts ...SessionOption) (*Session, error) {
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

	cfg := defaultACPSessionConfig()
	for _, opt := range opts {
		opt(&cfg)
	}

	params := NewSessionRequest{
		CWD:        cfg.CWD,
		McpServers: cfg.McpServers,
	}
	if params.McpServers == nil {
		params.McpServers = []McpServerConfig{}
	}

	resp, err := c.sendRequestAndWait(ctx, MethodSessionNew, params)
	if err != nil {
		return nil, err
	}

	var sessionResp NewSessionResponse
	if err := json.Unmarshal(resp.Result, &sessionResp); err != nil {
		return nil, &ProtocolError{Message: "failed to parse session/new response", Cause: err}
	}

	session := newSession(c, sessionResp.SessionID)

	c.mu.Lock()
	c.sessions[sessionResp.SessionID] = session
	c.mu.Unlock()

	c.emit(SessionCreatedEvent{SessionID: sessionResp.SessionID})

	return session, nil
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

	close(c.done)

	if c.process != nil {
		c.process.Stop()
	}

	c.state.SetClosed()

	// Close all sessions
	c.mu.Lock()
	for _, session := range c.sessions {
		session.close()
	}
	c.mu.Unlock()

	// Wait for readLoop to exit before closing events channel
	// This prevents panic from sending on closed channel
	c.readWg.Wait()

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

// AgentInfo returns agent information (available after Start).
func (c *Client) AgentInfo() *InitializeResponse {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.agentInfo
}

// readLoop reads and processes messages from the agent subprocess.
func (c *Client) readLoop(ctx context.Context) {
	defer c.readWg.Done()
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
					c.emitError("", err, "read_line")
				}
				return
			}

			c.handleMessage(line)
		}
	}
}

// handleMessage processes a single JSON-RPC message from the agent.
func (c *Client) handleMessage(line []byte) {
	// Peek at the message to determine its type
	var base struct {
		ID     *int64 `json:"id,omitempty"`
		Method string `json:"method,omitempty"`
	}
	if err := json.Unmarshal(line, &base); err != nil {
		c.emitError("", &ProtocolError{Message: "failed to parse message", Line: string(line), Cause: err}, "parse_message")
		return
	}

	if base.Method != "" && base.ID != nil {
		// Agent-to-client request: has both method and id
		c.handleAgentRequest(line, base.Method, *base.ID)
	} else if base.ID != nil {
		// Response to our request: has id but no method
		c.handleResponse(line, *base.ID)
	} else if base.Method != "" {
		// Notification: has method but no id
		c.handleNotification(line, base.Method)
	}
}

// handleResponse routes a JSON-RPC response to its pending waiter.
func (c *Client) handleResponse(line []byte, id int64) {
	var resp JSONRPCResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		c.emitError("", &ProtocolError{Message: "failed to parse response", Line: string(line), Cause: err}, "parse_response")
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

// handleNotification processes a JSON-RPC notification from the agent.
func (c *Client) handleNotification(line []byte, method string) {
	if method == MethodSessionUpdate {
		var notif JSONRPCNotification
		if err := json.Unmarshal(line, &notif); err != nil {
			return
		}
		c.handleSessionUpdate(notif.Params)
	}
}

// handleSessionUpdate processes a session/update notification.
func (c *Client) handleSessionUpdate(params json.RawMessage) {
	var notif SessionNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}

	// Route to the session
	c.mu.RLock()
	session, ok := c.sessions[notif.SessionID]
	c.mu.RUnlock()

	if !ok {
		return
	}

	session.handleUpdate(&notif.Update)

	// Emit events based on the update type
	switch notif.Update.Type {
	case UpdateTypeAgentMessage:
		if notif.Update.Content != nil && notif.Update.Content.Type == "text" {
			session.mu.Lock()
			fullText := session.text.String()
			session.mu.Unlock()

			c.emit(TextDeltaEvent{
				SessionID: notif.SessionID,
				Delta:     notif.Update.Content.Text,
				FullText:  fullText,
			})
		}

	case UpdateTypeAgentThought:
		if notif.Update.Content != nil && notif.Update.Content.Type == "text" {
			session.mu.Lock()
			fullThinking := session.thinking.String()
			session.mu.Unlock()

			c.emit(ThinkingDeltaEvent{
				SessionID: notif.SessionID,
				Delta:     notif.Update.Content.Text,
				FullText:  fullThinking,
			})
		}

	case UpdateTypeToolCall:
		if notif.Update.Status == "running" || notif.Update.Status == "pending" {
			c.emit(ToolCallStartEvent{
				SessionID:  notif.SessionID,
				ToolCallID: notif.Update.ToolCallID,
				ToolName:   notif.Update.ToolName,
				Input:      notif.Update.Input,
			})
		} else {
			c.emit(ToolCallUpdateEvent{
				SessionID:  notif.SessionID,
				ToolCallID: notif.Update.ToolCallID,
				ToolName:   notif.Update.ToolName,
				Status:     notif.Update.Status,
				Input:      notif.Update.Input,
			})
		}

	case UpdateTypeToolCallUpdate:
		// Gemini CLI sends "tool_call_update" for status changes (completed, failed).
		toolName := notif.Update.ToolName
		if toolName == "" {
			toolName = extractToolName(notif.Update.ToolCallID)
		}
		c.emit(ToolCallUpdateEvent{
			SessionID:  notif.SessionID,
			ToolCallID: notif.Update.ToolCallID,
			ToolName:   toolName,
			Status:     notif.Update.Status,
			Input:      notif.Update.Input,
		})

	case UpdateTypePlanUpdate:
		c.emit(PlanUpdateEvent{
			SessionID: notif.SessionID,
			Plan:      notif.Update.Plan,
		})
	}
}

// handleAgentRequest processes a JSON-RPC request from the agent.
// ACP agents can request file operations, terminal commands, and permissions.
func (c *Client) handleAgentRequest(line []byte, method string, id int64) {
	var req JSONRPCRequest
	if err := json.Unmarshal(line, &req); err != nil {
		c.emitError("", &ProtocolError{Message: "failed to parse agent request", Line: string(line), Cause: err}, "parse_agent_request")
		return
	}

	ctx := context.Background()

	switch method {
	case MethodFsReadTextFile:
		c.handleFsReadTextFile(ctx, id, req.Params)
	case MethodFsWriteTextFile:
		c.handleFsWriteTextFile(ctx, id, req.Params)
	case MethodTerminalCreate:
		c.handleTerminalCreate(ctx, id, req.Params)
	case MethodTerminalOutput:
		c.handleTerminalOutput(ctx, id, req.Params)
	case MethodTerminalWaitExit:
		c.handleTerminalWaitExit(ctx, id, req.Params)
	case MethodTerminalKill:
		c.handleTerminalKill(ctx, id, req.Params)
	case MethodTerminalRelease:
		c.handleTerminalRelease(ctx, id, req.Params)
	case MethodRequestPermission:
		c.handleRequestPermission(ctx, id, req.Params)
	default:
		c.sendErrorResponse(id, ErrCodeMethodNotFound, "unknown method: "+method)
	}
}

func (c *Client) handleFsReadTextFile(ctx context.Context, id int64, params json.RawMessage) {
	var req ReadTextFileRequest
	if err := json.Unmarshal(params, &req); err != nil {
		c.sendErrorResponse(id, ErrCodeInvalidParams, err.Error())
		return
	}
	resp, err := c.config.FsHandler.ReadTextFile(ctx, req)
	if err != nil {
		c.sendErrorResponse(id, ErrCodeInternalError, err.Error())
		return
	}
	c.sendResponse(id, resp)
}

func (c *Client) handleFsWriteTextFile(ctx context.Context, id int64, params json.RawMessage) {
	var req WriteTextFileRequest
	if err := json.Unmarshal(params, &req); err != nil {
		c.sendErrorResponse(id, ErrCodeInvalidParams, err.Error())
		return
	}
	resp, err := c.config.FsHandler.WriteTextFile(ctx, req)
	if err != nil {
		c.sendErrorResponse(id, ErrCodeInternalError, err.Error())
		return
	}
	c.sendResponse(id, resp)
}

func (c *Client) handleTerminalCreate(ctx context.Context, id int64, params json.RawMessage) {
	var req CreateTerminalRequest
	if err := json.Unmarshal(params, &req); err != nil {
		c.sendErrorResponse(id, ErrCodeInvalidParams, err.Error())
		return
	}
	resp, err := c.config.TerminalHandler.Create(ctx, req)
	if err != nil {
		c.sendErrorResponse(id, ErrCodeInternalError, err.Error())
		return
	}
	c.sendResponse(id, resp)
}

func (c *Client) handleTerminalOutput(ctx context.Context, id int64, params json.RawMessage) {
	var req TerminalOutputRequest
	if err := json.Unmarshal(params, &req); err != nil {
		c.sendErrorResponse(id, ErrCodeInvalidParams, err.Error())
		return
	}
	resp, err := c.config.TerminalHandler.Output(ctx, req)
	if err != nil {
		c.sendErrorResponse(id, ErrCodeInternalError, err.Error())
		return
	}
	c.sendResponse(id, resp)
}

func (c *Client) handleTerminalWaitExit(ctx context.Context, id int64, params json.RawMessage) {
	var req WaitForTerminalExitRequest
	if err := json.Unmarshal(params, &req); err != nil {
		c.sendErrorResponse(id, ErrCodeInvalidParams, err.Error())
		return
	}
	resp, err := c.config.TerminalHandler.WaitForExit(ctx, req)
	if err != nil {
		c.sendErrorResponse(id, ErrCodeInternalError, err.Error())
		return
	}
	c.sendResponse(id, resp)
}

func (c *Client) handleTerminalKill(ctx context.Context, id int64, params json.RawMessage) {
	var req KillTerminalRequest
	if err := json.Unmarshal(params, &req); err != nil {
		c.sendErrorResponse(id, ErrCodeInvalidParams, err.Error())
		return
	}
	resp, err := c.config.TerminalHandler.Kill(ctx, req)
	if err != nil {
		c.sendErrorResponse(id, ErrCodeInternalError, err.Error())
		return
	}
	c.sendResponse(id, resp)
}

func (c *Client) handleTerminalRelease(ctx context.Context, id int64, params json.RawMessage) {
	var req ReleaseTerminalRequest
	if err := json.Unmarshal(params, &req); err != nil {
		c.sendErrorResponse(id, ErrCodeInvalidParams, err.Error())
		return
	}
	resp, err := c.config.TerminalHandler.Release(ctx, req)
	if err != nil {
		c.sendErrorResponse(id, ErrCodeInternalError, err.Error())
		return
	}
	c.sendResponse(id, resp)
}

func (c *Client) handleRequestPermission(ctx context.Context, id int64, params json.RawMessage) {
	var req RequestPermissionRequest
	if err := json.Unmarshal(params, &req); err != nil {
		c.sendErrorResponse(id, ErrCodeInvalidParams, err.Error())
		return
	}

	// Emit tool start event from the permission request, since Gemini CLI
	// does not send a separate tool_call notification with status "running".
	toolName := req.ToolCall.ToolName
	if toolName == "" {
		// Extract tool name from toolCallId (e.g. "write_file-1770849300776" → "write_file")
		toolName = extractToolName(req.ToolCall.ToolCallID)
	}
	input := req.ToolCall.Input
	if input == nil && len(req.ToolCall.Locations) > 0 {
		input = map[string]interface{}{
			"path": req.ToolCall.Locations[0].Path,
		}
	}
	c.emit(ToolCallStartEvent{
		SessionID:  req.SessionID,
		ToolCallID: req.ToolCall.ToolCallID,
		ToolName:   toolName,
		Input:      input,
	})

	resp, err := c.config.PermissionHandler.RequestPermission(ctx, req)
	if err != nil {
		c.sendErrorResponse(id, ErrCodeInternalError, err.Error())
		return
	}
	c.sendResponse(id, resp)
}

// sendResponse sends a JSON-RPC response to the agent.
func (c *Client) sendResponse(id int64, result interface{}) {
	resp, err := newResponse(id, result)
	if err != nil {
		return
	}
	c.process.WriteJSON(resp)
}

// sendErrorResponse sends a JSON-RPC error response to the agent.
func (c *Client) sendErrorResponse(id int64, code int, message string) {
	resp := newErrorResponse(id, code, message)
	c.process.WriteJSON(resp)
}

// sendRequestAndWait sends a JSON-RPC request and waits for the response.
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

// sendNotification sends a JSON-RPC notification to the agent (no response expected).
func (c *Client) sendNotification(method string, params interface{}) error {
	notif, err := newNotification(method, params)
	if err != nil {
		return err
	}
	return c.process.WriteJSON(notif)
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
func (c *Client) emitError(sessionID string, err error, context string) {
	c.emit(ErrorEvent{
		SessionID: sessionID,
		Error:     err,
		Context:   context,
	})
}
