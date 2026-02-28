package sessionmodel

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"
)

// --- Live NDJSON (from claude.Session via stdio) ----------------------------

// FromLiveNDJSON strips a bare NDJSON line.  The line IS already a vocabulary
// message, so this is a thin wrapper around protocol.ParseMessage.
func FromLiveNDJSON(line []byte) (protocol.Message, error) {
	return protocol.ParseMessage(line)
}

// --- SDK recorder ({timestamp, direction, message} envelope) ----------------

type sdkRecorderEnvelope struct {
	Timestamp string          `json:"timestamp"`
	Direction string          `json:"direction"`
	Message   json.RawMessage `json:"message"`
}

// FromSDKRecorder strips the {timestamp, direction, message} envelope used
// by the SDK session recorder (agent-cli-wrapper/claude/recorder.go).
// Returns the vocabulary message, timestamp, direction, and any error.
func FromSDKRecorder(line []byte) (protocol.Message, time.Time, string, error) {
	var env sdkRecorderEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, time.Time{}, "", fmt.Errorf("unmarshal SDK recorder envelope: %w", err)
	}

	ts, _ := time.Parse(time.RFC3339Nano, env.Timestamp)

	if len(env.Message) == 0 {
		return nil, ts, env.Direction, nil
	}

	msg, err := protocol.ParseMessage(env.Message)
	if err != nil {
		return nil, ts, env.Direction, fmt.Errorf("parse SDK recorder message: %w", err)
	}
	return msg, ts, env.Direction, nil
}

// --- Raw JSONL (~/.claude/projects/ native format) --------------------------

type rawJSONLEnvelope struct {
	Timestamp     string          `json:"timestamp"`
	Type          string          `json:"type"`
	Subtype       string          `json:"subtype,omitempty"`
	ParentUUID    string          `json:"parentUuid,omitempty"`
	UUID          string          `json:"uuid,omitempty"`
	GitBranch     string          `json:"gitBranch,omitempty"`
	Version       string          `json:"version,omitempty"`
	SessionID     string          `json:"sessionId,omitempty"`
	Content       string          `json:"content,omitempty"`
	Operation     string          `json:"operation,omitempty"`
	PRNumber      int             `json:"prNumber,omitempty"`
	PRURL         string          `json:"prUrl,omitempty"`
	PRRepository  string          `json:"prRepository,omitempty"`
	IsSidechain   bool            `json:"isSidechain,omitempty"`
	DurationMs    int64           `json:"durationMs,omitempty"`
	Message       json.RawMessage `json:"message,omitempty"`
	Data          json.RawMessage `json:"data,omitempty"`
	ToolUseResult json.RawMessage `json:"toolUseResult,omitempty"`
	Error         json.RawMessage `json:"error,omitempty"`
}

// FromRawJSONL strips the native ~/.claude/projects/ envelope and returns the
// vocabulary message plus envelope metadata.
//
// The raw JSONL format wraps the inner "message" (which has the same
// {content, role, ...} structure as SDK messages) with an outer "type" field
// plus envelope metadata (parentUuid, isSidechain, gitBranch, etc.).
//
// For raw JSONL-only types (file-history-snapshot, queue-operation, progress)
// a nil message is returned with metadata only.
func FromRawJSONL(line []byte) (protocol.Message, *RawEnvelopeMeta, error) {
	var env rawJSONLEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, nil, fmt.Errorf("unmarshal raw JSONL envelope: %w", err)
	}

	ts, _ := time.Parse(time.RFC3339Nano, env.Timestamp)
	meta := &RawEnvelopeMeta{
		ParentUUID:    env.ParentUUID,
		IsSidechain:   env.IsSidechain,
		GitBranch:     env.GitBranch,
		Version:       env.Version,
		UUID:          env.UUID,
		SessionID:     env.SessionID,
		Timestamp:     ts,
		ToolUseResult: env.ToolUseResult,
	}

	meta.Type = env.Type
	meta.Subtype = env.Subtype

	// Raw JSONL-only envelope types — produce synthetic OutputLines via
	// the RawEnvelopeMeta so the caller (LoadFromRawJSONL) can handle them.
	switch env.Type {
	case "file-history-snapshot":
		return nil, meta, nil

	case "queue-operation":
		meta.Operation = env.Operation
		meta.Content = env.Content
		return nil, meta, nil

	case "pr-link":
		meta.PRNumber = env.PRNumber
		meta.PRURL = env.PRURL
		meta.PRRepository = env.PRRepository
		return nil, meta, nil

	case "progress":
		meta.Data = env.Data
		return nil, meta, nil

	case "system":
		// system subtypes that don't carry an inner "message": turn_duration,
		// api_error, compact_boundary, local_command.
		if env.Subtype != "" && env.Subtype != "init" {
			meta.Content = env.Content
			meta.DurationMs = env.DurationMs
			meta.ErrorJSON = env.Error
			return nil, meta, nil
		}
	}

	// For vocabulary types (system, assistant, user, result, stream_event,
	// control_request, control_response), the inner "message" field contains
	// the message content.  We reconstruct a top-level message by injecting
	// the "type" field into the inner message JSON so protocol.ParseMessage
	// can dispatch on it.
	if len(env.Message) == 0 {
		// Some entries (e.g. bare "system" init) are already top-level.
		msg, err := protocol.ParseMessage(line)
		if err != nil {
			return nil, meta, nil // not a vocabulary message
		}
		return msg, meta, nil
	}

	// Build a composite JSON object for protocol.ParseMessage.
	// The protocol parser expects: { type: "assistant", message: { role, content } }
	// but the raw JSONL inner message IS the message content: { role, content }.
	// For "user" and "assistant" types, wrap in the expected structure.
	// For "result" and "system" (init), the inner fields are at the top level.
	composite, err := wrapForProtocol(env.Message, env.Type)
	if err != nil {
		return nil, meta, fmt.Errorf("wrap raw JSONL message: %w", err)
	}

	msg, err := protocol.ParseMessage(composite)
	if err != nil {
		return nil, meta, fmt.Errorf("parse raw JSONL inner message: %w", err)
	}
	return msg, meta, nil
}

// wrapForProtocol constructs a JSON object that protocol.ParseMessage expects.
//
// For assistant/user messages, the protocol expects:
//
//	{ "type": "assistant", "message": { "role": "...", "content": [...] } }
//
// But in raw JSONL, the inner `message` field IS already the message content
// (role, content, usage, etc.). So we wrap it:
//
//	{ "type": "assistant", "message": <inner> }
//
// For result/system types, the inner fields can be merged directly.
func wrapForProtocol(inner json.RawMessage, envType string) ([]byte, error) {
	switch envType {
	case "assistant", "user":
		// Wrap: { "type": envType, "message": <inner> }
		wrapper := map[string]json.RawMessage{
			"message": inner,
		}
		typeBytes, _ := json.Marshal(envType)
		wrapper["type"] = typeBytes
		return json.Marshal(wrapper)

	default:
		// For result, system, stream_event, etc.: inject "type" into the inner object.
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(inner, &obj); err != nil {
			return nil, err
		}
		typeBytes, _ := json.Marshal(envType)
		obj["type"] = typeBytes
		return json.Marshal(obj)
	}
}
