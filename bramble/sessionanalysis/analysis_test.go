package sessionanalysis

import (
	"os"
	"path/filepath"
	"testing"

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
	assert.Equal(t, 0*1, int(sess.Duration()))

	sess.StartTime = sess.StartTime.Add(0)
	sess.EndTime = sess.EndTime.Add(0)
	assert.Equal(t, 0*1, int(sess.Duration()))
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
