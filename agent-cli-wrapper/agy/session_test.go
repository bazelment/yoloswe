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

func TestIsToolDeniedEmptyResult(t *testing.T) {
	t.Parallel()

	deniedStderr := []byte(`jetski: no output produced — a tool required the "read_file" permission that headless mode cannot prompt for, so it was auto-denied.`)

	tests := []struct {
		name     string
		response string
		stderr   []byte
		want     bool
	}{
		{"empty response with denial marker", "", deniedStderr, true},
		{"empty response, generic no-output prefix without a denial clause", "",
			[]byte("jetski: no output produced \u2014 the model stopped early."), false},
		{"empty response, no stderr at all", "", nil, false},
		{"empty response, unrelated stderr", "", []byte("some warning: deprecated flag used"), false},
		{"non-empty response with denial marker present", "SECRETTOKEN42", deniedStderr, false},
		{"non-empty response, no stderr", "hi", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isToolDeniedEmptyResult(tt.response, tt.stderr))
		})
	}
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

// writeFakeAgyWithStderr writes a fake CLI that produces both result JSON and
// stderr, including the headless tool-denial shape.
func writeFakeAgyWithStderr(t *testing.T, resultJSON, stderrText string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agy")
	script := "#!/bin/sh\n" +
		"cat >&2 <<'EOF'\n" + stderrText + "\nEOF\n" +
		"cat <<'EOF'\n" + resultJSON + "\nEOF\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

// TestSession_ToolDeniedEmptyResult_ReportsAsFailure keeps denied empty turns
// from being reported as successful queries.
func TestSession_ToolDeniedEmptyResult_ReportsAsFailure(t *testing.T) {
	t.Parallel()

	resultJSON := `{"conversation_id":"conv-denied","status":"SUCCESS","response":"",` +
		`"duration_seconds":22.09,"num_turns":1,` +
		`"usage":{"input_tokens":11299,"output_tokens":146,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":11445}}`
	stderrText := `jetski: no output produced — a tool required the "read_file" permission that headless mode cannot prompt for, so it was auto-denied. Add an allow-rule under permissions.allow in settings.json (e.g. read_file(<target>)). Alternatively, re-run with --dangerously-skip-permissions to auto-approve all tools.`
	cliPath := writeFakeAgyWithStderr(t, resultJSON, stderrText)

	result, err := Query(context.Background(), "read sample.go and report the token", WithCLIPath(cliPath))
	require.Error(t, err, "a denied-tool empty response must not be reported as success")
	assert.Nil(t, result)

	var toolDenied *ToolDeniedError
	require.ErrorAs(t, err, &toolDenied, "error must be identifiable as a tool-denial, not a generic failure")
	assert.Equal(t, "SUCCESS", toolDenied.Status)
	assert.Contains(t, toolDenied.Stderr, "read_file")
}

// TestSession_ToolDeniedEmptyResult_CanceledStatusAlsoCaught ensures the guard
// does not depend on the result status.
func TestSession_ToolDeniedEmptyResult_CanceledStatusAlsoCaught(t *testing.T) {
	t.Parallel()

	resultJSON := `{"conversation_id":"conv-canceled","status":"CANCELED","response":"",` +
		`"duration_seconds":10.3,"num_turns":1,` +
		`"usage":{"input_tokens":24419,"output_tokens":373,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":24792}}`
	stderrText := `jetski: no output produced — a tool required the "command" permission that headless mode cannot prompt for, so it was auto-denied.`
	cliPath := writeFakeAgyWithStderr(t, resultJSON, stderrText)

	result, err := Query(context.Background(), "run a command", WithCLIPath(cliPath))
	require.Error(t, err)
	assert.Nil(t, result)

	var toolDenied *ToolDeniedError
	require.ErrorAs(t, err, &toolDenied)
	assert.Equal(t, "CANCELED", toolDenied.Status)
}

// TestSession_LegitimateEmptyResult_StillSucceeds proves an empty response
// without the denial marker still succeeds.
func TestSession_LegitimateEmptyResult_StillSucceeds(t *testing.T) {
	t.Parallel()

	resultJSON := `{"conversation_id":"conv-quiet","status":"SUCCESS","response":"",` +
		`"duration_seconds":3.1,"num_turns":1,` +
		`"usage":{"input_tokens":50,"output_tokens":5,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":55}}`
	cliPath := writeFakeAgy(t, resultJSON)

	result, err := Query(context.Background(), "silently do nothing", WithCLIPath(cliPath))
	require.NoError(t, err, "an empty response with no denial marker must still succeed")
	require.NotNil(t, result)
	assert.True(t, result.Success)
	assert.Empty(t, result.Text)
}

// TestSession_GenericNoOutputPrefixWithoutDenial_StillSucceeds proves the guard
// keys on the denial clause, not on agy's generic "no output produced" prefix:
// an empty turn that prints that prefix for some other reason must still
// succeed rather than be mislabelled a permission denial.
func TestSession_GenericNoOutputPrefixWithoutDenial_StillSucceeds(t *testing.T) {
	t.Parallel()

	resultJSON := `{"conversation_id":"conv-generic","status":"SUCCESS","response":"",` +
		`"duration_seconds":1.2,"num_turns":1,` +
		`"usage":{"input_tokens":10,"output_tokens":0,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":10}}`
	stderrText := `jetski: no output produced — the model stopped without emitting text.`
	cliPath := writeFakeAgyWithStderr(t, resultJSON, stderrText)

	result, err := Query(context.Background(), "do nothing", WithCLIPath(cliPath))
	require.NoError(t, err, "the generic no-output prefix alone must not be read as a tool denial")
	require.NotNil(t, result)
	assert.True(t, result.Success)
}

// TestSession_ToolDeniedEmptyResult_PreservesAgyError proves a turn that failed
// for its own reason keeps agy's explanation even when the denial branch wins.
func TestSession_ToolDeniedEmptyResult_PreservesAgyError(t *testing.T) {
	t.Parallel()

	resultJSON := `{"conversation_id":"conv-both","status":"ERROR","response":"",` +
		`"error":"timeout waiting for response",` +
		`"duration_seconds":178.2,"num_turns":1,` +
		`"usage":{"input_tokens":10,"output_tokens":0,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":10}}`
	stderrText := `jetski: no output produced — a tool required the "command" permission that headless mode cannot prompt for, so it was auto-denied.`
	cliPath := writeFakeAgyWithStderr(t, resultJSON, stderrText)

	_, err := Query(context.Background(), "do a thing", WithCLIPath(cliPath))
	require.Error(t, err)

	var toolDenied *ToolDeniedError
	require.ErrorAs(t, err, &toolDenied)
	assert.Equal(t, "timeout waiting for response", toolDenied.AgyError,
		"agy's own failure explanation must not be swallowed by the denial branch")
	assert.Contains(t, err.Error(), "timeout waiting for response")
}

// TestSession_NonEmptyResponseWithStderrNoise_StillSucceeds proves a real
// response still succeeds when stderr contains denial noise.
func TestSession_NonEmptyResponseWithStderrNoise_StillSucceeds(t *testing.T) {
	t.Parallel()

	resultJSON := `{"conversation_id":"conv-recovered","status":"SUCCESS","response":"SECRETTOKEN42",` +
		`"duration_seconds":5.0,"num_turns":1,` +
		`"usage":{"input_tokens":100,"output_tokens":10,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":110}}`
	stderrText := `jetski: no output produced — a tool required the "read_file" permission that headless mode cannot prompt for, so it was auto-denied.`
	cliPath := writeFakeAgyWithStderr(t, resultJSON, stderrText)

	result, err := Query(context.Background(), "read sample.go", WithCLIPath(cliPath))
	require.NoError(t, err, "a non-empty response must succeed even if stderr carries denial noise from a recovered tool call")
	require.NotNil(t, result)
	assert.Equal(t, "SECRETTOKEN42", result.Text)
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
