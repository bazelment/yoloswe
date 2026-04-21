package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// scriptedSession drives runRetryLoop with a pre-scripted sequence of
// TurnResults. The first result is returned by the caller of Execute
// before runRetryLoop is invoked, so the slice held here represents
// the responses to each follow-up "retry" Ask.
type scriptedSession struct {
	responses []*claude.TurnResult
	asks      []string
	askErrs   []error
	cursor    int
}

func (s *scriptedSession) Ask(ctx context.Context, content string) (*claude.TurnResult, error) {
	s.asks = append(s.asks, content)
	if s.cursor < len(s.askErrs) {
		if err := s.askErrs[s.cursor]; err != nil {
			s.cursor++
			return nil, err
		}
	}
	if s.cursor >= len(s.responses) {
		return nil, errors.New("scriptedSession: no more responses queued")
	}
	r := s.responses[s.cursor]
	s.cursor++
	return r, nil
}

// realToolUseErrorBlocks is the minimal set of ContentBlocks that
// FinalTurnToolError will flag as retry-worthy: IsError true and the
// <tool_use_error> wrapper present.
func realToolUseErrorBlocks(excerpt string) []claude.ContentBlock {
	return []claude.ContentBlock{
		{Type: claude.ContentBlockTypeToolUse, ToolUseID: "t1", ToolName: "Bash"},
		{
			Type:       claude.ContentBlockTypeToolResult,
			ToolUseID:  "t1",
			ToolResult: "<tool_use_error>" + excerpt + "</tool_use_error>",
			IsError:    true,
		},
	}
}

// nonzeroExitBashBlocks mimics a `gh pr checks` exit-8 shape: IsError
// true but no <tool_use_error> wrapper. Must not trigger retry.
func nonzeroExitBashBlocks() []claude.ContentBlock {
	return []claude.ContentBlock{
		{Type: claude.ContentBlockTypeToolUse, ToolUseID: "t1", ToolName: "Bash"},
		{
			Type:       claude.ContentBlockTypeToolResult,
			ToolUseID:  "t1",
			ToolResult: "Exit code 8\nForge Visual Tests\tpending",
			IsError:    true,
		},
	}
}

func cleanBlocks() []claude.ContentBlock {
	return []claude.ContentBlock{{Type: claude.ContentBlockTypeText, Text: "done"}}
}

func turnResult(text string, blocks []claude.ContentBlock) *claude.TurnResult {
	return &claude.TurnResult{
		Text:          text,
		ContentBlocks: blocks,
		Success:       true,
	}
}

func TestRunRetryLoop_RetriesUntilClean(t *testing.T) {
	t.Parallel()
	initial := turnResult("err1", realToolUseErrorBlocks("first"))
	fake := &scriptedSession{
		responses: []*claude.TurnResult{
			turnResult("err2", realToolUseErrorBlocks("second")),
			turnResult("clean", cleanBlocks()),
		},
	}
	cfg := ExecuteConfig{MaxToolErrorRetries: 3}

	result, attempts, reason, err := runRetryLoop(context.Background(), fake, initial, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 2 {
		t.Errorf("expected 2 retry Asks, got %d", attempts)
	}
	if len(fake.asks) != 2 {
		t.Errorf("expected 2 Ask calls, got %d", len(fake.asks))
	}
	if result.Text != "clean" {
		t.Errorf("expected final text=clean, got %q", result.Text)
	}
	// stopReason is the last assigned value — default "exhausted" when
	// the loop exited via the "no more error" break.
	if reason != RetryStopExhausted {
		t.Errorf("expected stopReason=%s, got %s", RetryStopExhausted, reason)
	}
}

func TestRunRetryLoop_RespectsCountLimit(t *testing.T) {
	t.Parallel()
	initial := turnResult("err1", realToolUseErrorBlocks("same"))
	fake := &scriptedSession{
		responses: []*claude.TurnResult{
			// second excerpt must differ or no_progress would short-circuit
			turnResult("err2", realToolUseErrorBlocks("different")),
		},
	}
	cfg := ExecuteConfig{MaxToolErrorRetries: 1}

	result, attempts, reason, err := runRetryLoop(context.Background(), fake, initial, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 1 {
		t.Errorf("expected exactly 1 retry Ask, got %d", attempts)
	}
	if reason != RetryStopExhausted {
		t.Errorf("expected stopReason=%s, got %s", RetryStopExhausted, reason)
	}
	if result.Text != "err2" {
		t.Errorf("expected final text=err2, got %q", result.Text)
	}
}

func TestRunRetryLoop_NoProgressAborts(t *testing.T) {
	t.Parallel()
	initial := turnResult("err1", realToolUseErrorBlocks("identical"))
	fake := &scriptedSession{
		responses: []*claude.TurnResult{
			turnResult("err2", realToolUseErrorBlocks("identical")),
		},
	}
	cfg := ExecuteConfig{MaxToolErrorRetries: 5}

	_, attempts, reason, err := runRetryLoop(context.Background(), fake, initial, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 1 {
		t.Errorf("expected 1 retry before no_progress abort, got %d", attempts)
	}
	if reason != RetryStopNoProgress {
		t.Errorf("expected stopReason=%s, got %s", RetryStopNoProgress, reason)
	}
}

func TestRunRetryLoop_CtxCancelled(t *testing.T) {
	t.Parallel()
	initial := turnResult("err1", realToolUseErrorBlocks("first"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the loop runs
	fake := &scriptedSession{}
	cfg := ExecuteConfig{MaxToolErrorRetries: 3}

	_, attempts, reason, err := runRetryLoop(ctx, fake, initial, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 0 {
		t.Errorf("expected no Asks before ctx check, got %d", attempts)
	}
	if len(fake.asks) != 0 {
		t.Errorf("expected 0 Ask calls, got %d", len(fake.asks))
	}
	if reason != RetryStopCtxCancelled {
		t.Errorf("expected stopReason=%s, got %s", RetryStopCtxCancelled, reason)
	}
}

func TestRunRetryLoop_DisabledByDefault(t *testing.T) {
	t.Parallel()
	initial := turnResult("err1", realToolUseErrorBlocks("first"))
	fake := &scriptedSession{}
	cfg := ExecuteConfig{MaxToolErrorRetries: 0}

	result, attempts, _, err := runRetryLoop(context.Background(), fake, initial, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 0 {
		t.Errorf("expected 0 retries, got %d", attempts)
	}
	if len(fake.asks) != 0 {
		t.Errorf("expected 0 Ask calls, got %d", len(fake.asks))
	}
	if result != initial {
		t.Error("expected the initial result returned unchanged")
	}
}

// TestRunRetryLoop_SkipsOnNonzeroExitBash is the G1 regression for
// evidence log 2: `gh pr checks` exit 8 sets IsError but no wrapper.
// FinalTurnToolError returns ok=false, retry does not fire.
func TestRunRetryLoop_SkipsOnNonzeroExitBash(t *testing.T) {
	t.Parallel()
	initial := turnResult("polling", nonzeroExitBashBlocks())
	fake := &scriptedSession{}
	cfg := ExecuteConfig{MaxToolErrorRetries: 3}

	result, attempts, _, err := runRetryLoop(context.Background(), fake, initial, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 0 {
		t.Errorf("expected no retries on nonzero-exit Bash, got %d", attempts)
	}
	if len(fake.asks) != 0 {
		t.Errorf("expected 0 Ask calls, got %d", len(fake.asks))
	}
	if result != initial {
		t.Error("expected the initial result returned unchanged")
	}
}

// TestRunRetryLoop_RetriesCleanlyOnRealToolUseError ensures the G1
// tightening did not break the PLA-212 recover-on-retry path.
func TestRunRetryLoop_RetriesCleanlyOnRealToolUseError(t *testing.T) {
	t.Parallel()
	initial := turnResult("parallel-cancelled", realToolUseErrorBlocks("Cancelled: parallel tool call"))
	fake := &scriptedSession{
		responses: []*claude.TurnResult{
			turnResult("fixed", cleanBlocks()),
		},
	}
	cfg := ExecuteConfig{MaxToolErrorRetries: 2}

	result, attempts, _, err := runRetryLoop(context.Background(), fake, initial, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 1 {
		t.Errorf("expected 1 retry Ask, got %d", attempts)
	}
	if result.Text != "fixed" {
		t.Errorf("expected recovered result, got %q", result.Text)
	}
}

// recoveredErrorBlocks returns a block sequence where an early Edit
// tool_use_error is followed by a successful Read + Edit, mirroring the
// 2026-04-18 production failure (fc29ffb6 session, line 132 → 156).
func recoveredErrorBlocks() []claude.ContentBlock {
	return []claude.ContentBlock{
		{Type: claude.ContentBlockTypeToolUse, ToolUseID: "edit1", ToolName: "Edit"},
		{
			Type:       claude.ContentBlockTypeToolResult,
			ToolUseID:  "edit1",
			ToolResult: "<tool_use_error>File has not been read yet. Read it first before writing to it.</tool_use_error>",
			IsError:    true,
		},
		{Type: claude.ContentBlockTypeToolUse, ToolUseID: "read1", ToolName: "Read"},
		{Type: claude.ContentBlockTypeToolResult, ToolUseID: "read1", ToolResult: "file contents", IsError: false},
		{Type: claude.ContentBlockTypeToolUse, ToolUseID: "edit2", ToolName: "Edit"},
		{Type: claude.ContentBlockTypeToolResult, ToolUseID: "edit2", ToolResult: "edit applied", IsError: false},
	}
}

// TestRunRetryLoop_SkipsWhenAgentRecovered is the G4 regression for the
// 2026-04-18 production failure: a turn that had an early Edit error
// followed by a successful recovery must not trigger retry.
func TestRunRetryLoop_SkipsWhenAgentRecovered(t *testing.T) {
	t.Parallel()
	initial := &claude.TurnResult{
		Text:          "Validated successfully.",
		ContentBlocks: recoveredErrorBlocks(),
		Success:       true,
	}
	fake := &scriptedSession{}
	cfg := ExecuteConfig{MaxToolErrorRetries: 3}

	result, attempts, _, err := runRetryLoop(context.Background(), fake, initial, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 0 {
		t.Errorf("expected no retries when agent self-recovered, got %d", attempts)
	}
	if len(fake.asks) != 0 {
		t.Errorf("expected 0 Ask calls, got %d", len(fake.asks))
	}
	if result != initial {
		t.Error("expected the original result returned unchanged")
	}
}

// TestRunRetryLoop_PropagatesAskError ensures an Ask transport error
// aborts the loop cleanly without masking.
func TestRunRetryLoop_PropagatesAskError(t *testing.T) {
	t.Parallel()
	initial := turnResult("err1", realToolUseErrorBlocks("first"))
	askErr := errors.New("transport boom")
	fake := &scriptedSession{askErrs: []error{askErr}}
	cfg := ExecuteConfig{MaxToolErrorRetries: 3}

	_, _, _, err := runRetryLoop(context.Background(), fake, initial, cfg)
	if err == nil || !strings.Contains(err.Error(), "transport boom") {
		t.Errorf("expected transport error to propagate, got %v", err)
	}
}
