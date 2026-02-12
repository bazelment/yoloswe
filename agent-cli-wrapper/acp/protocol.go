package acp

import "encoding/json"

// ACP protocol version supported by this SDK.
const ProtocolVersion = 1

// --- Initialize ---

// InitializeRequest is sent by the client to establish the connection.
type InitializeRequest struct {
	ClientCapabilities *ClientCapabilities `json:"clientCapabilities,omitempty"`
	ClientInfo         *Implementation     `json:"clientInfo,omitempty"`
	ProtocolVersion    int                 `json:"protocolVersion"`
}

// InitializeResponse is returned by the agent with its capabilities.
type InitializeResponse struct {
	AgentCapabilities *AgentCapabilities `json:"agentCapabilities,omitempty"`
	AgentInfo         *Implementation    `json:"agentInfo,omitempty"`
	AuthMethods       []AuthMethod       `json:"authMethods,omitempty"`
	ProtocolVersion   int                `json:"protocolVersion"`
}

// Implementation identifies a client or agent.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// ClientCapabilities advertises what the client supports.
type ClientCapabilities struct {
	Fs       *FsCapability `json:"fs,omitempty"`
	Terminal bool          `json:"terminal,omitempty"`
}

// FsCapability describes file system capabilities.
type FsCapability struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

// AgentCapabilities advertises what the agent supports.
type AgentCapabilities struct {
	McpCapabilities *McpCapabilities `json:"mcpCapabilities,omitempty"`
	LoadSession     bool             `json:"loadSession,omitempty"`
}

// McpCapabilities describes supported MCP transports.
type McpCapabilities struct {
	Stdio bool `json:"stdio,omitempty"`
	HTTP  bool `json:"http,omitempty"`
	SSE   bool `json:"sse,omitempty"`
}

// AuthMethod describes an authentication method.
type AuthMethod struct {
	Type string `json:"type"`
	URL  string `json:"url,omitempty"`
}

// --- Session ---

// NewSessionRequest creates a new conversation session.
type NewSessionRequest struct {
	CWD        string            `json:"cwd"`
	McpServers []McpServerConfig `json:"mcpServers"`
}

// NewSessionResponse returns the created session info.
type NewSessionResponse struct {
	SessionID     string                `json:"sessionId"`
	Modes         []SessionModeState    `json:"modes,omitempty"`
	ConfigOptions []SessionConfigOption `json:"configOptions,omitempty"`
}

// SessionModeState describes an available session mode.
type SessionModeState struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName,omitempty"`
	IsCurrent   bool   `json:"isCurrent,omitempty"`
}

// SessionConfigOption describes a configurable session option.
type SessionConfigOption struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName,omitempty"`
	Value       string `json:"value,omitempty"`
}

// McpServerConfig configures an MCP server for the session.
type McpServerConfig struct {
	Name    string      `json:"name"`
	Type    string      `json:"type"`
	Command string      `json:"command,omitempty"`
	URL     string      `json:"url,omitempty"`
	Headers []McpHeader `json:"headers,omitempty"`
	Env     []EnvVar    `json:"env,omitempty"`
	Args    []string    `json:"args,omitempty"`
}

// McpHeader is a name-value pair for HTTP headers.
type McpHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// EnvVar is a name-value pair for environment variables.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// --- Prompt ---

// PromptRequest sends a user prompt to the agent.
type PromptRequest struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// PromptResponse indicates the prompt turn has completed.
type PromptResponse struct {
	StopReason string `json:"stopReason"` // "endTurn", "cancelled", "error", "maxTokens"
}

// --- Content Blocks ---

// ContentBlock represents typed content in prompts and messages.
// Discriminated by the Type field.
type ContentBlock struct {
	// Common
	Type string `json:"type"` // "text", "image", "audio", "resource_link", "resource"

	// TextContent
	Text string `json:"text,omitempty"`

	// ImageContent / AudioContent
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"` // base64-encoded
	URI      string `json:"uri,omitempty"`

	// ResourceLink
	Name string `json:"name,omitempty"`
}

// NewTextContent creates a text content block.
func NewTextContent(text string) ContentBlock {
	return ContentBlock{Type: "text", Text: text}
}

// --- Session Update (notification from agent) ---

// SessionNotification is the params for a session/update notification.
type SessionNotification struct {
	SessionID string        `json:"sessionId"`
	Update    SessionUpdate `json:"update"`
}

// SessionUpdate is a discriminated union of update types.
// The Type field determines which other fields are populated.
// Note: Gemini CLI uses "sessionUpdate" as the JSON discriminator field name.
type SessionUpdate struct {
	// Discriminator
	Type string `json:"sessionUpdate"` // "agent_message_chunk", "agent_thought_chunk", "tool_call", etc.

	// agent_message_chunk / agent_thought_chunk fields
	Content *ContentBlock `json:"content,omitempty"`

	// tool_call fields
	ToolCallID string                 `json:"toolCallId,omitempty"`
	ToolName   string                 `json:"toolName,omitempty"`
	Status     string                 `json:"status,omitempty"` // "running", "completed", "errored"
	Input      map[string]interface{} `json:"input,omitempty"`

	// tool_call_result fields
	Result []ContentBlock `json:"result,omitempty"`

	// plan_update fields
	Plan *Plan `json:"plan,omitempty"`

	// available_commands_update fields
	AvailableCommands []AvailableCommand `json:"availableCommands,omitempty"`

	// current_mode_update fields
	CurrentModeID string `json:"currentModeId,omitempty"`

	// Metadata
	Meta json.RawMessage `json:"_meta,omitempty"`
}

// Plan represents an agent's execution plan.
type Plan struct {
	Entries []PlanEntry `json:"entries"`
}

// PlanEntry is a single step in a plan.
type PlanEntry struct {
	Title    string `json:"title"`
	Status   string `json:"status,omitempty"`   // "pending", "in_progress", "completed"
	Priority string `json:"priority,omitempty"` // "high", "medium", "low"
}

// AvailableCommand describes a command the agent can execute.
type AvailableCommand struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

// --- Cancel ---

// CancelNotification is sent by the client to cancel a prompt.
type CancelNotification struct {
	SessionID string `json:"sessionId"`
}

// --- Agent-to-Client Requests ---

// ReadTextFileRequest is sent by the agent to read a file.
type ReadTextFileRequest struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Line      int    `json:"line,omitempty"`  // 1-based, optional
	Limit     int    `json:"limit,omitempty"` // optional
}

// ReadTextFileResponse returns the file content.
type ReadTextFileResponse struct {
	Content string `json:"content"`
}

// WriteTextFileRequest is sent by the agent to write a file.
type WriteTextFileRequest struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Content   string `json:"content"`
}

// WriteTextFileResponse is empty on success.
type WriteTextFileResponse struct{}

// CreateTerminalRequest is sent by the agent to create a terminal.
type CreateTerminalRequest struct {
	SessionID       string   `json:"sessionId"`
	Command         string   `json:"command"`
	CWD             string   `json:"cwd,omitempty"`
	Env             []EnvVar `json:"env,omitempty"`
	Args            []string `json:"args,omitempty"`
	OutputByteLimit int      `json:"outputByteLimit,omitempty"`
}

// CreateTerminalResponse returns the terminal ID.
type CreateTerminalResponse struct {
	TerminalID string `json:"terminalId"`
}

// TerminalOutputRequest reads output from a terminal.
type TerminalOutputRequest struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}

// TerminalOutputResponse returns the terminal output.
type TerminalOutputResponse struct {
	ExitStatus *int   `json:"exitStatus,omitempty"`
	Output     string `json:"output"`
}

// WaitForTerminalExitRequest waits for a terminal to exit.
type WaitForTerminalExitRequest struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}

// WaitForTerminalExitResponse returns the exit status.
type WaitForTerminalExitResponse struct {
	ExitStatus int `json:"exitStatus"`
}

// KillTerminalRequest kills a terminal process.
type KillTerminalRequest struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}

// KillTerminalResponse is empty on success.
type KillTerminalResponse struct{}

// ReleaseTerminalRequest releases terminal resources.
type ReleaseTerminalRequest struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}

// ReleaseTerminalResponse is empty on success.
type ReleaseTerminalResponse struct{}

// RequestPermissionRequest is sent by the agent to request tool permission.
type RequestPermissionRequest struct {
	ToolCall  ToolCallInfo       `json:"toolCall"`
	SessionID string             `json:"sessionId"`
	Options   []PermissionOption `json:"options"`
}

// ToolCallInfo describes the tool call requiring permission.
type ToolCallInfo struct {
	Input      map[string]interface{} `json:"input"`
	ToolCallID string                 `json:"toolCallId"`
	ToolName   string                 `json:"toolName,omitempty"`
	Status     string                 `json:"status"`
	// Gemini-specific fields (not in generic ACP spec)
	Title     string         `json:"title,omitempty"`
	Kind      string         `json:"kind,omitempty"` // "edit", "shell", etc.
	Locations []ToolLocation `json:"locations,omitempty"`
}

// ToolLocation describes a file location associated with a tool call.
type ToolLocation struct {
	Path string `json:"path"`
}

// PermissionOption describes a permission choice.
type PermissionOption struct {
	ID   string `json:"optionId"`
	Name string `json:"name"`
	Kind string `json:"kind"` // "allow_once", "allow_always", "reject_once", "reject_always"
}

// RequestPermissionResponse returns the user's permission choice.
type RequestPermissionResponse struct {
	Outcome PermissionOutcome `json:"outcome"`
}

// PermissionOutcome is the result of a permission request.
// Discriminated by the Type field.
type PermissionOutcome struct {
	Type     string `json:"type"` // "cancelled", "selected"
	OptionID string `json:"optionId,omitempty"`
}
