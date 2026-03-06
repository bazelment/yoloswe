package sessionanalysis

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestParseSession_WithSummaryLimit(t *testing.T) {
	fixturePath := filepath.Join("..", "sessionmodel", "testdata", "full_session.jsonl")
	if _, err := os.Stat(fixturePath); os.IsNotExist(err) {
		t.Skip("test fixture not found")
	}

	cfg := Config{SummaryWordLimit: 5} // Very low limit to trigger summarization
	sess, err := ParseSessionWithConfig(fixturePath, cfg)
	require.NoError(t, err)

	// Check that long responses get summarized.
	for _, turn := range sess.Turns {
		if turn.ResponseWordCount() > 5 {
			assert.NotEmpty(t, turn.ResponseSummary,
				"turn %d with %d words should be summarized", turn.Number, turn.ResponseWordCount())
		}
	}
}

func TestParseSession_NoSummaryWhenDisabled(t *testing.T) {
	fixturePath := filepath.Join("..", "sessionmodel", "testdata", "full_session.jsonl")
	if _, err := os.Stat(fixturePath); os.IsNotExist(err) {
		t.Skip("test fixture not found")
	}

	cfg := Config{SummaryWordLimit: 0}
	sess, err := ParseSessionWithConfig(fixturePath, cfg)
	require.NoError(t, err)

	for _, turn := range sess.Turns {
		assert.Empty(t, turn.ResponseSummary,
			"turn %d should not be summarized when limit is 0", turn.Number)
	}
}

func TestSummarizeText(t *testing.T) {
	// Short text should pass through.
	short := "hello world"
	assert.Equal(t, short, summarizeText(short, 10))

	// Long text should be summarized.
	words := make([]string, 100)
	for i := range words {
		words[i] = "word"
	}
	long := "start " + joinWords(words) + " end"
	summary := summarizeText(long, 20)
	assert.Contains(t, summary, "words omitted")
	assert.Contains(t, summary, "start")
}

func joinWords(words []string) string {
	result := ""
	for i, w := range words {
		if i > 0 {
			result += " "
		}
		result += w
	}
	return result
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
