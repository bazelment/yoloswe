package agy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseResultPayload_Success(t *testing.T) {
	t.Parallel()

	raw := `{"conversation_id":"7f3ec0ba-9c3a-458c-ada3-15e87074608c","status":"SUCCESS",` +
		`"response":"JSONOK\n","duration_seconds":0.83,"num_turns":1,` +
		`"usage":{"input_tokens":13894,"output_tokens":2,"thinking_tokens":0,` +
		`"cache_read_tokens":0,"total_tokens":13896}}`

	payload, err := parseResultPayload([]byte(raw))
	require.NoError(t, err)
	assert.Equal(t, "7f3ec0ba-9c3a-458c-ada3-15e87074608c", payload.ConversationID)
	assert.Equal(t, "SUCCESS", payload.Status)
	assert.Equal(t, "JSONOK\n", payload.Response)
	assert.Equal(t, Usage{
		InputTokens:  13894,
		OutputTokens: 2,
		TotalTokens:  13896,
	}, payload.Usage)
}

func TestParseResultPayload_Error(t *testing.T) {
	t.Parallel()

	raw := `{"conversation_id":"","status":"ERROR","response":"",` +
		`"error":"Error: empty prompt. Usage: agy --print \"your prompt here\"",` +
		`"duration_seconds":0.000006911,"num_turns":0,` +
		`"usage":{"input_tokens":0,"output_tokens":0,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":0}}`

	payload, err := parseResultPayload([]byte(raw))
	require.NoError(t, err)
	assert.Equal(t, "ERROR", payload.Status)
	assert.Contains(t, payload.Error, "empty prompt")
}

func TestParseResultPayload_Empty(t *testing.T) {
	t.Parallel()

	payload, err := parseResultPayload([]byte("   \n"))
	require.NoError(t, err)
	assert.Equal(t, resultPayload{}, payload)
}

func TestParseResultPayload_Malformed(t *testing.T) {
	t.Parallel()

	_, err := parseResultPayload([]byte("not json"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse")
}

// writeFakeAgy writes a JSON-printing fake agy binary.
func writeFakeAgy(t *testing.T, resultJSON string, expectedArgs ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agy")
	script := "#!/bin/sh\n" +
		"case \" $* \" in\n" +
		"  *' --output-format json '*) ;;\n" +
		"  *) echo 'fake agy: missing --output-format json' >&2; exit 2 ;;\n" +
		"esac\n"
	if len(expectedArgs) > 0 {
		script += "case \" $* \" in\n" +
			"  *' " + strings.Join(expectedArgs, " ") + " '*) ;;\n" +
			"  *) echo 'fake agy: missing expected arguments' >&2; exit 2 ;;\n" +
			"esac\n"
	}
	script += "cat <<'EOF'\n" + resultJSON + "\nEOF\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

func TestSession_RoundTrip_CapturesConversationIDAndUsage(t *testing.T) {
	// Keep fake-CLI tests serial: concurrent fork/exec can race ETXTBSY under
	// Bazel load (the documented jiradozer/orchestrator_test.go precedent).

	resultJSON := `{"conversation_id":"conv-abc-123","status":"SUCCESS","response":"PLATYPUS",` +
		`"duration_seconds":1.1,"num_turns":1,` +
		`"usage":{"input_tokens":100,"output_tokens":5,"thinking_tokens":1,"cache_read_tokens":2,"total_tokens":108}}`
	cliPath := writeFakeAgy(t, resultJSON)

	result, err := Query(context.Background(), "remember PLATYPUS", WithCLIPath(cliPath))
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, "PLATYPUS", result.Text)
	assert.Equal(t, "conv-abc-123", result.ConversationID)
	assert.True(t, result.Success)
	assert.Equal(t, 100, result.Usage.InputTokens)
	assert.Equal(t, 5, result.Usage.OutputTokens)
	assert.Equal(t, 2, result.Usage.CacheReadTokens)
	assert.Equal(t, 108, result.Usage.TotalTokens)
}

// TestSession_RoundTrip_ConversationIDFailsWithoutParsing guards the ID
// round-trip from agy's JSON result to QueryResult.
func TestSession_RoundTrip_ConversationIDFailsWithoutParsing(t *testing.T) {
	resultJSON := `{"conversation_id":"conv-xyz-789","status":"SUCCESS","response":"ok",` +
		`"duration_seconds":0.5,"num_turns":1,` +
		`"usage":{"input_tokens":10,"output_tokens":1,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":11}}`
	cliPath := writeFakeAgy(t, resultJSON)

	result, err := Query(context.Background(), "hi", WithCLIPath(cliPath))
	require.NoError(t, err)
	require.NotEmpty(t, result.ConversationID, "conversation id must round-trip from agy's JSON result to QueryResult")
	assert.Equal(t, "conv-xyz-789", result.ConversationID)
}

func TestSession_RoundTrip_ErrorStatusReportsAgyError(t *testing.T) {
	resultJSON := `{"conversation_id":"","status":"ERROR","response":"",` +
		`"error":"boom","duration_seconds":0.01,"num_turns":0,` +
		`"usage":{"input_tokens":0,"output_tokens":0,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":0}}`
	cliPath := writeFakeAgy(t, resultJSON)

	result, err := Query(context.Background(), "hi", WithCLIPath(cliPath))
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "boom")
}

func TestSession_RoundTrip_ResumesWithConversationFlag(t *testing.T) {
	resultJSON := `{"conversation_id":"conv-resume-1","status":"SUCCESS","response":"PLATYPUS",
 "duration_seconds":0.2,"num_turns":2,
 "usage":{"input_tokens":50,"output_tokens":3,"thinking_tokens":0,"cache_read_tokens":10,"total_tokens":63}}`
	cliPath := writeFakeAgy(t, resultJSON, "--conversation", "conv-resume-1")

	result, err := Query(context.Background(), "what was the word?",
		WithCLIPath(cliPath), WithConversation("conv-resume-1"))
	require.NoError(t, err)
	assert.Equal(t, "PLATYPUS", result.Text)
	assert.Equal(t, "conv-resume-1", result.ConversationID)
}
