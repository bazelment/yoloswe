package sessionanalysis

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/sessionmodel"
)

func TestParseSession_FullSession(t *testing.T) {
	// Use the existing test fixture from sessionmodel.
	fixturePath := filepath.Join("..", "sessionmodel", "testdata", "full_session.jsonl")
	if _, err := os.Stat(fixturePath); os.IsNotExist(err) {
		t.Skip("test fixture not found")
	}

	sess, err := ParseSession(fixturePath)
	require.NoError(t, err)
	require.NotNil(t, sess)

	assert.NotEmpty(t, sess.ID, "session should have an ID")
	assert.NotEmpty(t, sess.Turns, "session should have turns")

	// First turn should have user input.
	assert.NotEmpty(t, sess.Turns[0].UserInput, "first turn should have user input")

	// Session summary should be generated.
	assert.NotEmpty(t, sess.Summary, "session should have a summary")
	assert.Contains(t, sess.Summary, "Goal:")
	assert.Contains(t, sess.Summary, "Outcome:")
}

func TestResponseWordCount(t *testing.T) {
	turn := Turn{Response: "one two three four five"}
	assert.Equal(t, 5, turn.ResponseWordCount())

	empty := Turn{Response: ""}
	assert.Equal(t, 0, empty.ResponseWordCount())
}

func TestSessionDuration(t *testing.T) {
	sess := Session{}
	assert.Equal(t, 0, int(sess.Duration()), "zero-value times should give zero duration")

	now := time.Now()
	sess.StartTime = now
	sess.EndTime = now.Add(5 * time.Minute)
	assert.Equal(t, 5*time.Minute, sess.Duration(), "should return correct positive duration")
}

func TestTotalToolCalls(t *testing.T) {
	sess := Session{
		Turns: []Turn{
			{ToolCalls: []ToolCall{{Name: "Read"}, {Name: "Grep"}}},
			{ToolCalls: []ToolCall{{Name: "Edit"}}},
			{ToolCalls: nil},
		},
	}
	assert.Equal(t, 3, sess.TotalToolCalls())
}

func TestParseSession_NonexistentFile(t *testing.T) {
	_, err := ParseSession("/nonexistent/path.jsonl")
	assert.Error(t, err)
}

func TestParseSession_AssistantPreservesUnknownAndThinkingBlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	lines := []byte(
		`{"type":"user","timestamp":"2026-05-05T00:00:00Z","message":{"role":"user","content":"hi"}}` + "\n" +
			`{"type":"assistant","timestamp":"2026-05-05T00:00:01Z","message":{"model":"claude","id":"m1","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"reasoning"},{"type":"text","text":"answer"},{"type":"future_block_xyz","payload":"opaque"}],"stop_reason":"end_turn"}}` + "\n")
	require.NoError(t, os.WriteFile(path, lines, 0o600))

	sess, err := ParseSession(path)
	require.NoError(t, err)
	require.Len(t, sess.Turns, 1)
	resp := sess.Turns[0].Response
	assert.Contains(t, resp, "reasoning")
	assert.Contains(t, resp, "answer")
	assert.Contains(t, resp, "future_block_xyz")
}

func TestParseSession_ResultErrorSubtypeRecordsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	lines := []byte(
		`{"type":"user","timestamp":"2026-05-05T00:00:00Z","message":{"role":"user","content":"hello"}}` + "\n" +
			`{"type":"result","timestamp":"2026-05-05T00:00:01Z","message":{"subtype":"error_max_turns","session_id":"s1","uuid":"u1","errors":["max turns exceeded"],"num_turns":1,"duration_ms":1000,"duration_api_ms":800,"total_cost_usd":0.05,"usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n")
	require.NoError(t, os.WriteFile(path, lines, 0o600))

	sess, err := ParseSession(path)
	require.NoError(t, err)
	require.Len(t, sess.Turns, 1)
	require.NotEmpty(t, sess.Turns[0].Errors)
}

func TestCleanUserInput_TaskNotification(t *testing.T) {
	input := `<task-notification><task-id>abc123</task-id><summary>Background command completed</summary></task-notification>`
	meta := &sessionmodel.RawEnvelopeMeta{IsMeta: true}
	result := cleanUserInput(input, meta)
	assert.Equal(t, "[task notification] Background command completed", result)
}

func TestCleanUserInput_PlainText(t *testing.T) {
	input := "fix the bug in main.go"
	meta := &sessionmodel.RawEnvelopeMeta{}
	assert.Equal(t, input, cleanUserInput(input, meta))
}

func TestCleanUserInput_AgentMessage(t *testing.T) {
	input := `<teammate-message teammate_id="team-lead">Your task is to instrument the service</teammate-message>`
	meta := &sessionmodel.RawEnvelopeMeta{AgentName: "tua-agent"}
	result := cleanUserInput(input, meta)
	assert.Equal(t, "[tua-agent] Your task is to instrument the service", result)
}

func TestCleanUserInput_NilMeta(t *testing.T) {
	input := "plain text"
	assert.Equal(t, "plain text", cleanUserInput(input, nil))
}

func TestCleanSummary(t *testing.T) {
	assert.Equal(t, "The session did X.", cleanSummary("**Session Summary:**\n\nThe session did X."))
	assert.Equal(t, "The session did X.", cleanSummary("## Summary\n\nThe session did X."))
	assert.Equal(t, "Plain text.", cleanSummary("Plain text."))
}
