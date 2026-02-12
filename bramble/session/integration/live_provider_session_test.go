package integration

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	"github.com/bazelment/yoloswe/bramble/session"
)

// TestLiveProvider_NonTmuxSessionRendersAgentResponse reproduces and validates
// non-tmux builder/planner behavior with real provider binaries.
//
// Manual run examples:
//
//	BRAMBLE_LIVE_PROVIDER=codex bazel test //bramble/session/integration:integration_test --test_timeout=600 --test_filter=TestLiveProvider_NonTmuxSessionRendersAgentResponse
//	BRAMBLE_LIVE_PROVIDER=gemini bazel test //bramble/session/integration:integration_test --test_timeout=600 --test_filter=TestLiveProvider_NonTmuxSessionRendersAgentResponse
//
// Optional:
//
//	BRAMBLE_LIVE_MODEL=<model-id> to override the provider model.
func TestLiveProvider_NonTmuxSessionRendersAgentResponse(t *testing.T) {
	provider := strings.ToLower(strings.TrimSpace(os.Getenv("BRAMBLE_LIVE_PROVIDER")))
	if provider == "" {
		t.Skip("set BRAMBLE_LIVE_PROVIDER=codex or gemini to run this manual live-provider test")
	}

	model := strings.TrimSpace(os.Getenv("BRAMBLE_LIVE_MODEL"))
	switch provider {
	case session.ProviderCodex:
		if model == "" {
			model = "gpt-5.3-codex"
		}
		requireBinary(t, "codex")
	case session.ProviderGemini:
		if model == "" {
			model = "gemini-2.5-flash"
		}
		requireBinary(t, "gemini")
	default:
		t.Fatalf("unsupported BRAMBLE_LIVE_PROVIDER=%q; expected codex or gemini", provider)
	}

	workDir := t.TempDir()
	logDir := resolveLiveLogDir(t)

	manager := session.NewManagerWithConfig(session.ManagerConfig{
		RepoName:       "live-provider-repro",
		SessionMode:    session.SessionModeTUI,
		ProtocolLogDir: logDir,
	})
	defer manager.Close()

	initialToken := "BRAMBLE_NON_TMUX_INITIAL_OK"
	followUpToken := "BRAMBLE_NON_TMUX_FOLLOWUP_OK"

	initialPrompt := fmt.Sprintf("Reply with exactly %s", initialToken)
	sessID, err := manager.StartSession(session.SessionTypeBuilder, workDir, initialPrompt, model)
	require.NoError(t, err)

	waitForIdleOrFail(t, manager, sessID, 180*time.Second)
	requireOutputContainsToken(t, manager.GetSessionOutput(sessID), initialToken)

	followPrompt := fmt.Sprintf("Now reply with exactly %s", followUpToken)
	require.NoError(t, manager.SendFollowUp(sessID, followPrompt))

	waitForOutputToken(t, manager, sessID, followUpToken, 180*time.Second)
	waitForIdleOrFail(t, manager, sessID, 180*time.Second)

	switch provider {
	case session.ProviderCodex:
		matches, err := filepath.Glob(filepath.Join(logDir, "*-codex.protocol.jsonl"))
		require.NoError(t, err)
		require.NotEmpty(t, matches, "expected Codex protocol logs in %s", logDir)
	case session.ProviderGemini:
		matches, err := filepath.Glob(filepath.Join(logDir, "*-gemini.stderr.log"))
		require.NoError(t, err)
		require.NotEmpty(t, matches, "expected Gemini stderr logs in %s", logDir)
	}
}

// TestLiveProvider_CodexMultiTurnToolConversation validates a real multi-turn
// Codex builder session that triggers shell/read/write tool usage and a
// question-answer follow-up loop.
//
// Manual run:
//
//	bazel test //bramble/session/integration:integration_test --test_timeout=900 --test_filter=TestLiveProvider_CodexMultiTurnToolConversation --test_output=all --test_sharding_strategy=disabled --test_arg=-test.v --test_env=BRAMBLE_LIVE_PROVIDER=codex --test_env=BRAMBLE_LIVE_LOG_DIR=/tmp/bramble-live-logs
func TestLiveProvider_CodexMultiTurnToolConversation(t *testing.T) {
	provider := strings.ToLower(strings.TrimSpace(os.Getenv("BRAMBLE_LIVE_PROVIDER")))
	if provider == "" {
		t.Skip("set BRAMBLE_LIVE_PROVIDER=codex to run this manual live-provider test")
	}
	if provider != session.ProviderCodex {
		t.Skipf("this test only supports codex; got BRAMBLE_LIVE_PROVIDER=%q", provider)
	}

	requireBinary(t, "codex")

	model := strings.TrimSpace(os.Getenv("BRAMBLE_LIVE_MODEL"))
	if model == "" {
		model = "gpt-5.3-codex"
	}

	workDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "seed.txt"), []byte("seed-value\n"), 0o644))

	logDir := resolveLiveLogDir(t)

	manager := session.NewManagerWithConfig(session.ManagerConfig{
		RepoName:       "live-provider-multiturn",
		SessionMode:    session.SessionModeTUI,
		ProtocolLogDir: logDir,
	})
	defer manager.Close()

	t1Token := "BRAMBLE_MULTI_TOOL_TURN1_OK"
	t2Token := "BRAMBLE_MULTI_TOOL_TURN2_OK"

	initialPrompt := fmt.Sprintf(
		`Run these shell commands in order: "pwd", "ls -1", and "cat seed.txt".
Then ask exactly this question on its own line: "What content should I write to output.txt?"
Include token %s in your response.`,
		t1Token,
	)

	sessID, err := manager.StartSession(session.SessionTypeBuilder, workDir, initialPrompt, model)
	require.NoError(t, err)

	waitForIdleOrFail(t, manager, sessID, 240*time.Second)
	initialOutput := flattenOutput(manager.GetSessionOutput(sessID))
	require.Contains(t, initialOutput, t1Token, "expected first-turn token in output")
	require.Contains(t, initialOutput, "What content should I write to output.txt?", "expected explicit assistant question")

	followPrompt := fmt.Sprintf(
		`Write exactly "alpha-from-user" into output.txt using a shell command.
Then run "cat output.txt".
Reply with exactly %s.`,
		t2Token,
	)
	require.NoError(t, manager.SendFollowUp(sessID, followPrompt))

	waitForOutputToken(t, manager, sessID, t2Token, 240*time.Second)
	waitForIdleOrFail(t, manager, sessID, 240*time.Second)

	finalOutput := flattenOutput(manager.GetSessionOutput(sessID))
	require.Contains(t, finalOutput, t2Token, "expected second-turn token in output")

	// Validate protocol log contains command execution events and a write attempt.
	logFile := findSingleCodexProtocolLog(t, logDir)
	stats := scanCodexProtocolStats(t, logFile)
	if stats.execBeginCount > 0 {
		require.True(t, stats.sawReadCmd, "expected a read command (cat/ls/pwd) in protocol log")
		require.True(t, stats.sawWriteCmd, "expected a write attempt command (output.txt redirection/tee) in protocol log")
		return
	}

	// In some environments, command execution is blocked at sandbox setup and no
	// exec_command_begin events are emitted; validate that Codex still reports the
	// blocked shell attempts in streamed text.
	require.True(t, stats.sawShellBlocked, "expected either exec events or explicit sandbox-blocked shell attempt text")
}

func waitForIdleOrFail(t *testing.T, manager *session.Manager, id session.SessionID, timeout time.Duration) {
	t.Helper()

	var info session.SessionInfo
	require.Eventually(t, func() bool {
		var ok bool
		info, ok = manager.GetSessionInfo(id)
		if !ok {
			return false
		}
		return info.Status == session.StatusIdle || info.Status.IsTerminal()
	}, timeout, 250*time.Millisecond, "session did not reach idle or terminal state in time")

	require.Equalf(t, session.StatusIdle, info.Status, "session ended as %s; output:\n%s", info.Status, flattenOutput(manager.GetSessionOutput(id)))
}

func requireOutputContainsToken(t *testing.T, lines []session.OutputLine, token string) {
	t.Helper()
	output := flattenOutput(lines)
	require.Containsf(t, output, token, "expected token %q in output; got:\n%s", token, output)
}

func waitForOutputToken(t *testing.T, manager *session.Manager, id session.SessionID, token string, timeout time.Duration) {
	t.Helper()
	require.Eventually(t, func() bool {
		lines := manager.GetSessionOutput(id)
		return strings.Contains(flattenOutput(lines), token)
	}, timeout, 250*time.Millisecond, "token %q not observed in session output", token)
}

func flattenOutput(lines []session.OutputLine) string {
	var b strings.Builder
	for i := range lines {
		if lines[i].Content == "" {
			continue
		}
		b.WriteString(lines[i].Content)
		b.WriteByte('\n')
	}
	return b.String()
}

func requireBinary(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("binary %q not found in PATH", name)
	}
}

func resolveLiveLogDir(t *testing.T) string {
	t.Helper()

	override := strings.TrimSpace(os.Getenv("BRAMBLE_LIVE_LOG_DIR"))
	if override == "" {
		return filepath.Join(t.TempDir(), "protocol-logs")
	}

	dir := filepath.Join(override, fmt.Sprintf("live-provider-%d", time.Now().UnixNano()))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	t.Logf("preserving protocol logs in %s", dir)
	return dir
}

func findSingleCodexProtocolLog(t *testing.T, logDir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(logDir, "*-codex.protocol.jsonl"))
	require.NoError(t, err)
	require.NotEmpty(t, matches, "expected codex protocol log in %s", logDir)
	return matches[0]
}

type codexProtocolStats struct {
	execBeginCount  int
	sawReadCmd      bool
	sawWriteCmd     bool
	sawShellBlocked bool
}

func scanCodexProtocolStats(t *testing.T, logPath string) codexProtocolStats {
	t.Helper()

	f, err := os.Open(logPath)
	require.NoError(t, err)
	defer f.Close()

	var stats codexProtocolStats
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 10*1024*1024)

	for scanner.Scan() {
		var entry codex.SessionLogEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Direction != "received" {
			continue
		}

		var msg struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(entry.Message, &msg); err != nil {
			continue
		}
		if msg.Method != codex.NotifyCodexEventExecBegin {
			if msg.Method != codex.NotifyAgentMessageDelta {
				continue
			}

			var delta codex.AgentMessageDeltaNotification
			if err := json.Unmarshal(msg.Params, &delta); err != nil {
				continue
			}
			lc := strings.ToLower(delta.Delta)
			if strings.Contains(lc, "command execution is blocked") || strings.Contains(lc, "sandbox") || strings.Contains(lc, "operation not permitted") {
				stats.sawShellBlocked = true
			}
			continue
		}

		var notif codex.CodexEventNotification
		if err := json.Unmarshal(msg.Params, &notif); err != nil {
			continue
		}
		var begin codex.ExecCommandBeginMsg
		if err := json.Unmarshal(notif.Msg, &begin); err != nil {
			continue
		}

		stats.execBeginCount++
		cmd := commandFromExecBegin(begin)
		lc := strings.ToLower(cmd)
		if strings.Contains(lc, "cat ") || strings.Contains(lc, "ls ") || strings.Contains(lc, "pwd") {
			stats.sawReadCmd = true
		}
		if strings.Contains(lc, "output.txt") && (strings.Contains(lc, ">") || strings.Contains(lc, "tee ")) {
			stats.sawWriteCmd = true
		}
	}

	require.NoError(t, scanner.Err())
	return stats
}

func commandFromExecBegin(begin codex.ExecCommandBeginMsg) string {
	if len(begin.ParsedCmd) > 0 && strings.TrimSpace(begin.ParsedCmd[0].Cmd) != "" {
		return strings.TrimSpace(begin.ParsedCmd[0].Cmd)
	}
	return strings.TrimSpace(strings.Join(begin.Command, " "))
}
