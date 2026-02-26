package cursor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseMessage_SystemInit(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"init","session_id":"sess-123","model":"cursor-fast","cwd":"/tmp","permissionMode":"auto","apiKeySource":"env"}`)

	msg, err := ParseMessage(line)
	require.NoError(t, err)

	sysMsg, ok := msg.(*SystemInitMessage)
	require.True(t, ok)
	assert.Equal(t, "system", sysMsg.Type)
	assert.Equal(t, "init", sysMsg.Subtype)
	assert.Equal(t, "sess-123", sysMsg.SessionID)
	assert.Equal(t, "cursor-fast", sysMsg.Model)
	assert.Equal(t, "/tmp", sysMsg.CWD)
	assert.Equal(t, "auto", sysMsg.PermissionMode)
	assert.Equal(t, "env", sysMsg.APIKeySource)
}

func TestParseMessage_Assistant(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hello world"}]},"session_id":"sess-123"}`)

	msg, err := ParseMessage(line)
	require.NoError(t, err)

	asstMsg, ok := msg.(*AssistantMessage)
	require.True(t, ok)
	assert.Equal(t, "assistant", asstMsg.Type)
	assert.Equal(t, "assistant", asstMsg.Message.Role)
	require.Len(t, asstMsg.Message.Content, 1)
	assert.Equal(t, "text", asstMsg.Message.Content[0].Type)
	assert.Equal(t, "Hello world", asstMsg.Message.Content[0].Text)
	assert.Equal(t, "sess-123", asstMsg.SessionID)
}

func TestParseMessage_ToolCallStarted(t *testing.T) {
	line := []byte(`{"type":"tool_call","subtype":"started","call_id":"call-1","tool_call":{"Read":{"args":{"file_path":"/tmp/test.go"}}},"session_id":"sess-123"}`)

	msg, err := ParseMessage(line)
	require.NoError(t, err)

	tcMsg, ok := msg.(*ToolCallMessage)
	require.True(t, ok)
	assert.Equal(t, "started", tcMsg.Subtype)
	assert.Equal(t, "call-1", tcMsg.CallID)

	detail, err := ParseToolCallDetail(tcMsg)
	require.NoError(t, err)
	assert.Equal(t, "Read", detail.Name)
	assert.Equal(t, "/tmp/test.go", detail.Args["file_path"])
	assert.Nil(t, detail.Result)
}

func TestParseMessage_ToolCallCompleted(t *testing.T) {
	line := []byte(`{"type":"tool_call","subtype":"completed","call_id":"call-1","tool_call":{"Read":{"args":{"file_path":"/tmp/test.go"},"result":"file contents here"}},"session_id":"sess-123"}`)

	msg, err := ParseMessage(line)
	require.NoError(t, err)

	tcMsg, ok := msg.(*ToolCallMessage)
	require.True(t, ok)
	assert.Equal(t, "completed", tcMsg.Subtype)

	detail, err := ParseToolCallDetail(tcMsg)
	require.NoError(t, err)
	assert.Equal(t, "Read", detail.Name)
	assert.Equal(t, "file contents here", detail.Result)
}

func TestParseMessage_ResultSuccess(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"success","duration_ms":1234,"duration_api_ms":1000,"is_error":false,"result":"All done","session_id":"sess-123"}`)

	msg, err := ParseMessage(line)
	require.NoError(t, err)

	resMsg, ok := msg.(*ResultMessage)
	require.True(t, ok)
	assert.Equal(t, "success", resMsg.Subtype)
	assert.Equal(t, int64(1234), resMsg.DurationMs)
	assert.Equal(t, int64(1000), resMsg.DurationAPIMs)
	assert.False(t, resMsg.IsError)
	assert.Equal(t, "All done", resMsg.Result)
}

func TestParseMessage_ResultError(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"error","duration_ms":500,"duration_api_ms":400,"is_error":true,"result":"something went wrong","session_id":"sess-123"}`)

	msg, err := ParseMessage(line)
	require.NoError(t, err)

	resMsg, ok := msg.(*ResultMessage)
	require.True(t, ok)
	assert.Equal(t, "error", resMsg.Subtype)
	assert.True(t, resMsg.IsError)
	assert.Equal(t, "something went wrong", resMsg.Result)
}

func TestParseMessage_MalformedJSON(t *testing.T) {
	line := []byte(`{not valid json}`)

	_, err := ParseMessage(line)
	require.Error(t, err)
}

func TestParseMessage_UnknownType(t *testing.T) {
	line := []byte(`{"type":"unknown_type"}`)

	msg, err := ParseMessage(line)
	require.NoError(t, err)
	assert.Nil(t, msg, "unknown types should return nil message")
}

func TestParseMessage_UserType(t *testing.T) {
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hello"}]},"session_id":"sess-123"}`)

	msg, err := ParseMessage(line)
	require.NoError(t, err)
	assert.Nil(t, msg, "user messages should be silently skipped")
}

func TestParseMessage_ThinkingType(t *testing.T) {
	line := []byte(`{"type":"thinking","subtype":"delta","text":"let me think","session_id":"sess-123"}`)

	msg, err := ParseMessage(line)
	require.NoError(t, err)
	assert.Nil(t, msg, "thinking messages should be silently skipped")
}

func TestParseMessage_UnknownSystemSubtype(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"unknown"}`)

	_, err := ParseMessage(line)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown system subtype")
}

func TestParseToolCallDetail_EmptyToolCall(t *testing.T) {
	msg := &ToolCallMessage{ToolCall: nil}
	_, err := ParseToolCallDetail(msg)
	require.Error(t, err)
}

func TestParseToolCallDetail_NilMessage(t *testing.T) {
	_, err := ParseToolCallDetail(nil)
	require.Error(t, err)
}
