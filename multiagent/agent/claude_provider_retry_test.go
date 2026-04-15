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

func turnResult(text string, blocks []claude.ContentBlock, bgLive bool) *claude.TurnResult {
	return &claude.TurnResult{
		Text:                  text,
		ContentBlocks:         blocks,
		HasLiveBackgroundWork: bgLive,
		Success:               true,
	}
}

func TestRunRetryLoop_RetriesUntilClean(t *testing.T) {
	t.Parallel()
	initial := turnResult("err1", realToolUseErrorBlocks("first"), false)
	fake := &scriptedSession{
		responses: []*claude.TurnResult{
			turnResult("err2", realToolUseErrorBlocks("second"), false),
			turnResult("clean", cleanBlocks(), false),
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
	initial := turnResult("err1", realToolUseErrorBlocks("same"), false)
	fake := &scriptedSession{
		responses: []*claude.TurnResult{
			// second excerpt must differ or no_progress would short-circuit
			turnResult("err2", realToolUseErrorBlocks("different"), false),
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
	initial := turnResult("err1", realToolUseErrorBlocks("identical"), false)
	fake := &scriptedSession{
		responses: []*claude.TurnResult{
			turnResult("err2", realToolUseErrorBlocks("identical"), false),
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
	initial := turnResult("err1", realToolUseErrorBlocks("first"), false)
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
	initial := turnResult("err1", realToolUseErrorBlocks("first"), false)
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

// TestRunRetryLoop_SkipsWhenBgWorkLive is the G2 regression for
// evidence log 1: a Skill disable-model-invocation tool_use_error fired
// while bg Bash work was parked. Retry would orphan the parked work;
// the gate must block before the content walk.
func TestRunRetryLoop_SkipsWhenBgWorkLive(t *testing.T) {
	t.Parallel()
	initial := turnResult(
		"parked",
		realToolUseErrorBlocks("Skill sy:pr-polish cannot be used with Skill tool due to disable-model-invocation"),
		true, // HasLiveBackgroundWork
	)
	fake := &scriptedSession{}
	cfg := ExecuteConfig{MaxToolErrorRetries: 3}

	_, attempts, reason, err := runRetryLoop(context.Background(), fake, initial, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 0 {
		t.Errorf("expected no retries when bg work is live, got %d", attempts)
	}
	if len(fake.asks) != 0 {
		t.Errorf("expected 0 Ask calls, got %d", len(fake.asks))
	}
	if reason != RetryStopBgWorkLive {
		t.Errorf("expected stopReason=%s, got %s", RetryStopBgWorkLive, reason)
	}
}

// TestRunRetryLoop_SkipsOnNonzeroExitBash is the G1 regression for
// evidence log 2: `gh pr checks` exit 8 sets IsError but no wrapper.
// FinalTurnToolError returns ok=false, retry does not fire.
func TestRunRetryLoop_SkipsOnNonzeroExitBash(t *testing.T) {
	t.Parallel()
	initial := turnResult("polling", nonzeroExitBashBlocks(), false)
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

// TestRunRetryLoop_RetriesCleanlyOnRealToolUseError ensures G1+G2
// tightening did not break the PLA-212 recover-on-retry path.
func TestRunRetryLoop_RetriesCleanlyOnRealToolUseError(t *testing.T) {
	t.Parallel()
	initial := turnResult("parallel-cancelled", realToolUseErrorBlocks("Cancelled: parallel tool call"), false)
	fake := &scriptedSession{
		responses: []*claude.TurnResult{
			turnResult("fixed", cleanBlocks(), false),
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

// TestRunRetryLoop_SkipsWhenBgWorkLiveAndParallelCancel checks that G2
// dominates G1 — even a real tool_use_error with the marker does not
// retry when bg work is live.
func TestRunRetryLoop_SkipsWhenBgWorkLiveAndParallelCancel(t *testing.T) {
	t.Parallel()
	initial := turnResult(
		"parked+error",
		realToolUseErrorBlocks("Cancelled: parallel tool call Bash(ruff check) errored"),
		true,
	)
	fake := &scriptedSession{}
	cfg := ExecuteConfig{MaxToolErrorRetries: 3}

	_, attempts, reason, err := runRetryLoop(context.Background(), fake, initial, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 0 {
		t.Errorf("expected no retries, got %d", attempts)
	}
	if reason != RetryStopBgWorkLive {
		t.Errorf("expected stopReason=%s, got %s", RetryStopBgWorkLive, reason)
	}
}

// TestRunRetryLoop_PropagatesAskError ensures an Ask transport error
// aborts the loop cleanly without masking.
func TestRunRetryLoop_PropagatesAskError(t *testing.T) {
	t.Parallel()
	initial := turnResult("err1", realToolUseErrorBlocks("first"), false)
	askErr := errors.New("transport boom")
	fake := &scriptedSession{askErrs: []error{askErr}}
	cfg := ExecuteConfig{MaxToolErrorRetries: 3}

	_, _, _, err := runRetryLoop(context.Background(), fake, initial, cfg)
	if err == nil || !strings.Contains(err.Error(), "transport boom") {
		t.Errorf("expected transport error to propagate, got %v", err)
	}
}
