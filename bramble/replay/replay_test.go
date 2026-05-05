package replay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	"github.com/bazelment/yoloswe/bramble/session"
)

// --- Format detection tests ---

func TestDetectFormat_CodexHeader(t *testing.T) {
	path := writeLog(t, []string{
		`{"format":"codex","version":"1.0","client":"test","timestamp":"2026-02-12T00:00:00Z"}`,
		`{"timestamp":"2026-02-12T00:00:01Z","direction":"sent","message":{}}`,
	})
	format, err := DetectFormat(path)
	require.NoError(t, err)
	assert.Equal(t, FormatCodex, format)
}

func TestDetectFormat_ClaudeSessionJSONL(t *testing.T) {
	path := writeLog(t, []string{
		`{"timestamp":"2026-01-01T00:00:00Z","direction":"sent","message":{"type":"user","message":{"content":"hello"}}}`,
		`{"timestamp":"2026-01-01T00:00:01Z","direction":"received","message":{"type":"stream_event","event":{}}}`,
	})
	format, err := DetectFormat(path)
	require.NoError(t, err)
	assert.Equal(t, FormatClaude, format)
}

func TestDetectFormat_ClaudeDirectory(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "messages.jsonl"), []byte("{}"), 0o644))
	format, err := DetectFormat(dir)
	require.NoError(t, err)
	assert.Equal(t, FormatClaude, format)
}

func TestDetectFormat_UnknownFormat(t *testing.T) {
	path := writeLog(t, []string{`{"random":"data"}`})
	_, err := DetectFormat(path)
	assert.Error(t, err)
}

// --- Parse auto-detection tests ---

func TestParse_AutoDetectsCodex(t *testing.T) {
	path := writeLog(t, []string{
		`{"format":"codex","version":"1.0","client":"test","timestamp":"2026-02-12T00:00:00Z"}`,
		`{"timestamp":"2026-02-12T00:00:01Z","direction":"sent","message":{"method":"turn/start","params":{"threadId":"t1","input":[{"type":"text","text":"hello codex"}]}}}`,
		`{"timestamp":"2026-02-12T00:00:02Z","direction":"received","message":{"method":"turn/completed","params":{"threadId":"t1","turn":{"id":"turn-1","status":"completed","error":null,"items":[]}}}}`,
	})
	result, err := Parse(path)
	require.NoError(t, err)
	assert.Equal(t, FormatCodex, result.Format)
	assert.Equal(t, "hello codex", result.Prompt)
	assert.Equal(t, session.StatusCompleted, result.Status)
}

func TestParse_AutoDetectsClaude(t *testing.T) {
	path := writeLog(t, []string{
		`{"timestamp":"2026-01-01T00:00:00Z","direction":"sent","message":{"type":"user","message":{"content":"hello claude"}}}`,
	})
	result, err := Parse(path)
	require.NoError(t, err)
	assert.Equal(t, FormatClaude, result.Format)
	assert.Equal(t, "hello claude", result.Prompt)
}

// --- Codex parser tests (migrated from codexlogview) ---

func TestCodexParser_TrimsPromptAndFollowUp(t *testing.T) {
	path := writeLog(t, []string{
		`{"format":"codex","version":"1.0","client":"test","timestamp":"2026-02-12T00:00:00Z"}`,
		`{"timestamp":"2026-02-12T00:00:01Z","direction":"sent","message":{"method":"turn/start","params":{"threadId":"t1","input":[{"type":"text","text":"  first prompt  "}]}}}`,
		`{"timestamp":"2026-02-12T00:00:02Z","direction":"received","message":{"method":"turn/completed","params":{"threadId":"t1","turn":{"id":"turn-1","status":"completed","error":null,"items":[]}}}}`,
		`{"timestamp":"2026-02-12T00:00:03Z","direction":"sent","message":{"method":"turn/start","params":{"threadId":"t1","input":[{"type":"text","text":"\nfollow up prompt\n"}]}}}`,
		`{"timestamp":"2026-02-12T00:00:04Z","direction":"received","message":{"method":"turn/completed","params":{"threadId":"t1","turn":{"id":"turn-2","status":"completed","error":null,"items":[]}}}}`,
	})

	result, err := parseCodexLog(path)
	require.NoError(t, err)
	assert.Equal(t, "first prompt", result.Prompt)
	assert.Equal(t, session.StatusCompleted, result.Status)

	var followUp string
	for _, line := range result.Lines {
		if line.Type == session.OutputTypeText && strings.Contains(line.Content, "follow up prompt") {
			followUp = line.Content
			break
		}
	}
	require.NotEmpty(t, followUp)
	assert.Equal(t, "follow up prompt", followUp)
}

func TestCodexParser_ApprovalRequestIsShownAndSetsIdle(t *testing.T) {
	path := writeLog(t, []string{
		`{"format":"codex","version":"1.0","client":"test","timestamp":"2026-02-12T00:00:00Z"}`,
		`{"timestamp":"2026-02-12T00:00:01Z","direction":"sent","message":{"method":"turn/start","params":{"threadId":"t1","input":[{"type":"text","text":"hello"}]}}}`,
		`{"timestamp":"2026-02-12T00:00:02Z","direction":"received","message":{"method":"item/commandExecution/requestApproval","params":{"threadId":"t1","turnId":"0","itemId":"call_1","reason":"Need write access","command":"/bin/zsh -lc \"echo hi > out.txt\""}}}`,
	})

	result, err := parseCodexLog(path)
	require.NoError(t, err)
	assert.Equal(t, session.StatusIdle, result.Status)

	statusCount := 0
	approvalDetails := ""
	for _, line := range result.Lines {
		if line.Type == session.OutputTypeStatus && line.Content == "Approval required before command execution" {
			statusCount++
		}
		if line.Type == session.OutputTypeText && strings.Contains(line.Content, "Need write access") {
			approvalDetails = line.Content
		}
	}
	assert.Equal(t, 1, statusCount)
	assert.Contains(t, approvalDetails, "/bin/zsh -lc")
	assert.Contains(t, approvalDetails, "Need write access")
}

func TestCodexParser_DedupesApprovalEventsByCallID(t *testing.T) {
	path := writeLog(t, []string{
		`{"format":"codex","version":"1.0","client":"test","timestamp":"2026-02-12T00:00:00Z"}`,
		`{"timestamp":"2026-02-12T00:00:01Z","direction":"sent","message":{"method":"turn/start","params":{"threadId":"t1","input":[{"type":"text","text":"hello"}]}}}`,
		`{"timestamp":"2026-02-12T00:00:02Z","direction":"received","message":{"method":"codex/event/exec_approval_request","params":{"id":"0","conversationId":"t1","msg":{"call_id":"call_1","command":["/bin/zsh","-lc","echo hi > out.txt"],"reason":"Need write access"}}}}`,
		`{"timestamp":"2026-02-12T00:00:03Z","direction":"received","message":{"method":"item/commandExecution/requestApproval","params":{"threadId":"t1","turnId":"0","itemId":"call_1","reason":"Need write access","command":"/bin/zsh -lc \"echo hi > out.txt\""}}}`,
	})

	result, err := parseCodexLog(path)
	require.NoError(t, err)

	statusCount := 0
	for _, line := range result.Lines {
		if line.Type == session.OutputTypeStatus && line.Content == "Approval required before command execution" {
			statusCount++
		}
	}
	assert.Equal(t, 1, statusCount)
}

func TestCodexParser_ApprovalRequestReemitsAfterTurnComplete(t *testing.T) {
	path := writeLog(t, []string{
		`{"format":"codex","version":"1.0","client":"test","timestamp":"2026-02-12T00:00:00Z"}`,
		`{"timestamp":"2026-02-12T00:00:01Z","direction":"sent","message":{"method":"turn/start","params":{"threadId":"t1","input":[{"type":"text","text":"turn1"}]}}}`,
		`{"timestamp":"2026-02-12T00:00:02Z","direction":"received","message":{"method":"item/commandExecution/requestApproval","params":{"threadId":"t1","turnId":"0","itemId":"call_1","reason":"Need write access","command":"echo hi > out.txt"}}}`,
		`{"timestamp":"2026-02-12T00:00:03Z","direction":"received","message":{"method":"turn/completed","params":{"threadId":"t1","turn":{"id":"turn-1","status":"completed","error":null,"items":[]}}}}`,
		`{"timestamp":"2026-02-12T00:00:04Z","direction":"sent","message":{"method":"turn/start","params":{"threadId":"t1","input":[{"type":"text","text":"turn2"}]}}}`,
		`{"timestamp":"2026-02-12T00:00:05Z","direction":"received","message":{"method":"item/commandExecution/requestApproval","params":{"threadId":"t1","turnId":"1","itemId":"call_1","reason":"Need write access","command":"echo hi > out.txt"}}}`,
	})

	result, err := parseCodexLog(path)
	require.NoError(t, err)
	assert.Equal(t, session.StatusIdle, result.Status)

	statusCount := 0
	for _, line := range result.Lines {
		if line.Type == session.OutputTypeStatus && line.Content == "Approval required before command execution" {
			statusCount++
		}
	}
	assert.Equal(t, 2, statusCount)
}

func TestCodexParser_TurnCompletionPreservesOtherThreadApprovals(t *testing.T) {
	path := writeLog(t, []string{
		`{"format":"codex","version":"1.0","client":"test","timestamp":"2026-02-12T00:00:00Z"}`,
		`{"timestamp":"2026-02-12T00:00:01Z","direction":"sent","message":{"method":"turn/start","params":{"threadId":"t1","input":[{"type":"text","text":"thread1"}]}}}`,
		`{"timestamp":"2026-02-12T00:00:02Z","direction":"sent","message":{"method":"turn/start","params":{"threadId":"t2","input":[{"type":"text","text":"thread2"}]}}}`,
		`{"timestamp":"2026-02-12T00:00:03Z","direction":"received","message":{"method":"item/commandExecution/requestApproval","params":{"threadId":"t1","turnId":"0","itemId":"call_t1","reason":"Need approval","command":"touch t1.txt"}}}`,
		`{"timestamp":"2026-02-12T00:00:04Z","direction":"received","message":{"method":"item/commandExecution/requestApproval","params":{"threadId":"t2","turnId":"0","itemId":"call_t2","reason":"Need approval","command":"touch t2.txt"}}}`,
		`{"timestamp":"2026-02-12T00:00:05Z","direction":"received","message":{"method":"turn/completed","params":{"threadId":"t1","turn":{"id":"turn-1","status":"completed","error":null,"items":[]}}}}`,
	})

	result, err := parseCodexLog(path)
	require.NoError(t, err)
	assert.Equal(t, session.StatusIdle, result.Status)
}

// --- Compact tests ---

// TestCodexParser_CumulativeUsageRendersAsPerTurnDelta verifies that on
// older Codex protocol versions where MappedEventTokenUsage carries a
// cumulative TotalTokenUsage (UsageIsCumulative=true), replay subtracts
// the prior turn's cumulative baseline so the rendered "Tokens:" line
// shows real per-turn deltas, not the running total.
//
// Without baseline subtraction the second turn would render
// "Tokens: 250 input / 90 output" (the cumulative) instead of the
// actual per-turn delta of "Tokens: 150 input / 50 output."
func TestCodexParser_CumulativeUsageRendersAsPerTurnDelta(t *testing.T) {
	p := newCodexReplayParser()
	ts := time.Time{}

	// Turn 1: cumulative 100 input / 40 output.
	p.handleMappedEvent(codex.MappedEvent{
		Kind:              codex.MappedEventTokenUsage,
		ThreadID:          "t1",
		Usage:             codex.TurnUsage{InputTokens: 100, OutputTokens: 40, TotalTokens: 140},
		UsageIsCumulative: true,
	}, ts)
	p.handleMappedEvent(codex.MappedEvent{
		Kind:     codex.MappedEventTurnCompleted,
		ThreadID: "t1",
		TurnID:   "1",
		Success:  true,
	}, ts)

	// Turn 2: cumulative 250 input / 90 output (delta should be 150 / 50).
	p.handleMappedEvent(codex.MappedEvent{
		Kind:              codex.MappedEventTokenUsage,
		ThreadID:          "t1",
		Usage:             codex.TurnUsage{InputTokens: 250, OutputTokens: 90, TotalTokens: 340},
		UsageIsCumulative: true,
	}, ts)
	p.handleMappedEvent(codex.MappedEvent{
		Kind:     codex.MappedEventTurnCompleted,
		ThreadID: "t1",
		TurnID:   "2",
		Success:  true,
	}, ts)

	// Collect all status lines that look like token summaries.
	var tokenLines []string
	for _, l := range p.lines {
		if l.Type == session.OutputTypeStatus && strings.HasPrefix(l.Content, "Tokens:") {
			tokenLines = append(tokenLines, l.Content)
		}
	}
	require.Len(t, tokenLines, 2, "expected one token summary per turn")
	assert.Equal(t, "Tokens: 100 input / 40 output", tokenLines[0],
		"turn 1 has no prior baseline, so cumulative IS the delta")
	assert.Equal(t, "Tokens: 150 input / 50 output", tokenLines[1],
		"turn 2 must subtract turn 1's cumulative baseline (250-100, 90-40)")
}

// TestCodexParser_PerTurnUsageNotSubtracted verifies that when
// MappedEventTokenUsage carries non-cumulative LastTokenUsage
// (UsageIsCumulative=false, the modern Codex path), replay does NOT
// subtract a baseline — the value already IS the per-turn delta.
func TestCodexParser_PerTurnUsageNotSubtracted(t *testing.T) {
	p := newCodexReplayParser()
	ts := time.Time{}

	for i, deltas := range [][2]int64{{100, 40}, {150, 50}} {
		p.handleMappedEvent(codex.MappedEvent{
			Kind:     codex.MappedEventTokenUsage,
			ThreadID: "t1",
			Usage: codex.TurnUsage{
				InputTokens: deltas[0], OutputTokens: deltas[1],
				TotalTokens: deltas[0] + deltas[1],
			},
			UsageIsCumulative: false, // per-turn, no baseline subtraction
		}, ts)
		p.handleMappedEvent(codex.MappedEvent{
			Kind:     codex.MappedEventTurnCompleted,
			ThreadID: "t1",
			TurnID:   string(rune('1' + i)),
			Success:  true,
		}, ts)
	}

	var tokenLines []string
	for _, l := range p.lines {
		if l.Type == session.OutputTypeStatus && strings.HasPrefix(l.Content, "Tokens:") {
			tokenLines = append(tokenLines, l.Content)
		}
	}
	require.Len(t, tokenLines, 2)
	assert.Equal(t, "Tokens: 100 input / 40 output", tokenLines[0])
	assert.Equal(t, "Tokens: 150 input / 50 output", tokenLines[1],
		"per-turn usage must render as-is, no cumulative subtraction")
}

func TestCompactLines_MergesTurnAndTokenLines(t *testing.T) {
	lines := []session.OutputLine{
		{Type: session.OutputTypeText, Content: "hello"},
		{Type: session.OutputTypeTurnEnd, TurnNumber: 1, CostUSD: 0},
		{Type: session.OutputTypeStatus, Content: "Tokens: 8735 input / 27 output"},
		{Type: session.OutputTypeStatus, Content: "Follow-up prompt:"},
	}

	got := CompactLines(lines)
	require.Len(t, got, 3)
	assert.Equal(t, session.OutputTypeStatus, got[1].Type)
	assert.Equal(t, "T1 $0.0000 in:8735 out:27", got[1].Content)
	assert.Equal(t, "Follow-up prompt:", got[2].Content)
}

// --- Helpers ---

func writeLog(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	content := strings.Join(lines, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}
