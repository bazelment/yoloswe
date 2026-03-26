package logging_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/voice/stt"
	"github.com/bazelment/yoloswe/voice/stt/logging"
)

func TestLogger_LogEvent(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := logging.New(&buf, start)

	evt := stt.Event{
		Type:       stt.EventFinalText,
		Text:       "hello world",
		Confidence: 0.98,
		IsFinal:    true,
		Timestamp:  start.Add(500 * time.Millisecond),
	}
	l.LogEvent("deepgram", evt)

	var entry logging.Entry
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))
	assert.Equal(t, "deepgram", entry.Provider)
	assert.Equal(t, "final", entry.EventType)
	assert.Equal(t, "hello world", entry.Text)
	assert.InDelta(t, 0.98, entry.Confidence, 0.001)
	assert.True(t, entry.IsFinal)
	assert.Equal(t, int64(500), entry.LatencyMs)
}

func TestLogger_LogEvent_WithError(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	start := time.Now()
	l := logging.New(&buf, start)

	evt := stt.Event{
		Type:      stt.EventError,
		Timestamp: start.Add(100 * time.Millisecond),
		Error:     assert.AnError,
	}
	l.LogEvent("deepgram", evt)

	var entry logging.Entry
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))
	assert.Equal(t, "error", entry.EventType)
	assert.Contains(t, entry.Error, "assert.AnError")
}

func TestLogger_LogEvent_WithRaw(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	start := time.Now()
	l := logging.New(&buf, start)

	evt := stt.Event{
		Type:      stt.EventPartialText,
		Text:      "test",
		Timestamp: start,
		Raw:       json.RawMessage(`{"custom":"field"}`),
	}
	l.LogEvent("deepgram", evt)

	var entry logging.Entry
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))
	require.NotNil(t, entry.Raw)
	assert.JSONEq(t, `{"custom":"field"}`, string(*entry.Raw))
}

func TestLogger_SessionStartEnd(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	start := time.Now()
	l := logging.New(&buf, start)

	l.LogSessionStart("deepgram")
	l.LogSessionEnd("deepgram")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 2)

	var startEntry, endEntry logging.Entry
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &startEntry))
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &endEntry))
	assert.Equal(t, "session-start", startEntry.EventType)
	assert.Equal(t, "session-end", endEntry.EventType)
}

func TestLogger_MultipleEvents_ValidJSONL(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	start := time.Now()
	l := logging.New(&buf, start)

	for i := 0; i < 5; i++ {
		l.LogEvent("test", stt.Event{
			Type:      stt.EventPartialText,
			Text:      "word",
			Timestamp: start.Add(time.Duration(i) * time.Second),
		})
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 5)

	var prevLatency int64 = -1
	for i, line := range lines {
		var entry logging.Entry
		require.NoError(t, json.Unmarshal([]byte(line), &entry), "line %d is not valid JSON", i)
		assert.GreaterOrEqual(t, entry.LatencyMs, prevLatency, "latency should be monotonic")
		prevLatency = entry.LatencyMs
	}
}
