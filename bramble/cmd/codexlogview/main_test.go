package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
)

func TestParseCLIArgs_MultipleFilesLegacyTailOptions(t *testing.T) {
	cfg, err := parseCLIArgs([]string{"a.jsonl", "b.jsonl", "100", "20", "plain", "compact"})
	require.NoError(t, err)
	assert.Equal(t, []string{"a.jsonl", "b.jsonl"}, cfg.paths)
	assert.Equal(t, 100, cfg.width)
	assert.Equal(t, 20, cfg.height)
	assert.False(t, cfg.enableMarkdown)
	assert.True(t, cfg.compact)
}

func TestParseCLIArgs_DefaultCompact(t *testing.T) {
	cfg, err := parseCLIArgs([]string{"a.jsonl"})
	require.NoError(t, err)
	assert.Equal(t, []string{"a.jsonl"}, cfg.paths)
	assert.True(t, cfg.compact)
	assert.True(t, cfg.enableMarkdown)
}

func TestCompactReplayLines_MergesTurnAndTokenLines(t *testing.T) {
	lines := []session.OutputLine{
		{Type: session.OutputTypeText, Content: "hello"},
		{Type: session.OutputTypeTurnEnd, TurnNumber: 1, CostUSD: 0},
		{Type: session.OutputTypeStatus, Content: "Tokens: 8735 input / 27 output"},
		{Type: session.OutputTypeStatus, Content: "Follow-up prompt:"},
	}

	got := compactReplayLines(lines)
	require.Len(t, got, 3)
	assert.Equal(t, session.OutputTypeStatus, got[1].Type)
	assert.Equal(t, "T1 $0.0000 in:8735 out:27", got[1].Content)
	assert.Equal(t, "Follow-up prompt:", got[2].Content)
}

func TestParseCodexProtocolLog_TrimsPromptAndFollowUp(t *testing.T) {
	logPath := writeLog(t, []string{
		`{"format":"codex","version":"1.0","client":"test","timestamp":"2026-02-12T00:00:00Z"}`,
		`{"timestamp":"2026-02-12T00:00:01Z","direction":"sent","message":{"method":"turn/start","params":{"threadId":"t1","input":[{"type":"text","text":"  first prompt  "}]}}}`,
		`{"timestamp":"2026-02-12T00:00:02Z","direction":"received","message":{"method":"turn/completed","params":{"threadId":"t1","turn":{"id":"turn-1","status":"completed","error":null,"items":[]}}}}`,
		`{"timestamp":"2026-02-12T00:00:03Z","direction":"sent","message":{"method":"turn/start","params":{"threadId":"t1","input":[{"type":"text","text":"\nfollow up prompt\n"}]}}}`,
		`{"timestamp":"2026-02-12T00:00:04Z","direction":"received","message":{"method":"turn/completed","params":{"threadId":"t1","turn":{"id":"turn-2","status":"completed","error":null,"items":[]}}}}`,
	})

	replay, err := parseCodexProtocolLog(logPath)
	require.NoError(t, err)

	assert.Equal(t, "first prompt", replay.prompt)
	assert.Equal(t, session.StatusCompleted, replay.status)

	var followUp string
	for _, line := range replay.lines {
		if line.Type == session.OutputTypeText && strings.Contains(line.Content, "follow up prompt") {
			followUp = line.Content
			break
		}
	}
	require.NotEmpty(t, followUp)
	assert.Equal(t, "follow up prompt", followUp)
}

func TestParseCodexProtocolLog_ApprovalRequestIsShownAndSetsIdle(t *testing.T) {
	logPath := writeLog(t, []string{
		`{"format":"codex","version":"1.0","client":"test","timestamp":"2026-02-12T00:00:00Z"}`,
		`{"timestamp":"2026-02-12T00:00:01Z","direction":"sent","message":{"method":"turn/start","params":{"threadId":"t1","input":[{"type":"text","text":"hello"}]}}}`,
		`{"timestamp":"2026-02-12T00:00:02Z","direction":"received","message":{"method":"item/commandExecution/requestApproval","params":{"threadId":"t1","turnId":"0","itemId":"call_1","reason":"Need write access","command":"/bin/zsh -lc \"echo hi > out.txt\""}}}`,
	})

	replay, err := parseCodexProtocolLog(logPath)
	require.NoError(t, err)
	assert.Equal(t, session.StatusIdle, replay.status)

	statusCount := 0
	approvalDetails := ""
	for _, line := range replay.lines {
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

func TestParseCodexProtocolLog_DedupesApprovalEventsByCallID(t *testing.T) {
	logPath := writeLog(t, []string{
		`{"format":"codex","version":"1.0","client":"test","timestamp":"2026-02-12T00:00:00Z"}`,
		`{"timestamp":"2026-02-12T00:00:01Z","direction":"sent","message":{"method":"turn/start","params":{"threadId":"t1","input":[{"type":"text","text":"hello"}]}}}`,
		`{"timestamp":"2026-02-12T00:00:02Z","direction":"received","message":{"method":"codex/event/exec_approval_request","params":{"id":"0","conversationId":"t1","msg":{"call_id":"call_1","command":["/bin/zsh","-lc","echo hi > out.txt"],"reason":"Need write access"}}}}`,
		`{"timestamp":"2026-02-12T00:00:03Z","direction":"received","message":{"method":"item/commandExecution/requestApproval","params":{"threadId":"t1","turnId":"0","itemId":"call_1","reason":"Need write access","command":"/bin/zsh -lc \"echo hi > out.txt\""}}}`,
	})

	replay, err := parseCodexProtocolLog(logPath)
	require.NoError(t, err)

	statusCount := 0
	for _, line := range replay.lines {
		if line.Type == session.OutputTypeStatus && line.Content == "Approval required before command execution" {
			statusCount++
		}
	}
	assert.Equal(t, 1, statusCount)
}

func writeLog(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	content := strings.Join(lines, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}
