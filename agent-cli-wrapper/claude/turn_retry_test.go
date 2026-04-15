package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFinalTurnToolError_IsErrorPlusMarker: both IsError and the marker
// are required. The canonical positive case.
func TestFinalTurnToolError_IsErrorPlusMarker(t *testing.T) {
	t.Parallel()
	blocks := []ContentBlock{
		{Type: ContentBlockTypeToolUse, ToolUseID: "t1", ToolName: "Skill"},
		{
			Type:       ContentBlockTypeToolResult,
			ToolUseID:  "t1",
			ToolResult: "<tool_use_error>Skill example:pr-polish cannot be used with Skill tool due to disable-model-invocation</tool_use_error>",
			IsError:    true,
		},
	}
	name, excerpt, ok := FinalTurnToolError(blocks)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if name != "Skill" {
		t.Errorf("expected tool=Skill, got %q", name)
	}
	if !strings.Contains(excerpt, "disable-model-invocation") {
		t.Errorf("excerpt should contain the reason, got %q", excerpt)
	}
}

// TestFinalTurnToolError_NonzeroExitBash: is_error without the marker is
// a nonzero-exit Bash the agent ran to inspect output (gh pr checks,
// grep, git diff). These must NOT trigger retry. Regression for the
// evidence log 2 false positive.
func TestFinalTurnToolError_NonzeroExitBash(t *testing.T) {
	t.Parallel()
	blocks := []ContentBlock{
		{Type: ContentBlockTypeToolUse, ToolUseID: "t1", ToolName: "Bash"},
		{
			Type:       ContentBlockTypeToolResult,
			ToolUseID:  "t1",
			ToolResult: "Exit code 8\nForge Visual Tests\tpending\nPython Tests\tpending",
			IsError:    true,
		},
	}
	if _, _, ok := FinalTurnToolError(blocks); ok {
		t.Error("nonzero-exit Bash must not be treated as a tool_use_error")
	}
}

// TestFinalTurnToolError_WriteNotReadYet: Write tool_use_error shape
// from evidence log 2. The CLI emits the wrapper, so retry fires.
func TestFinalTurnToolError_WriteNotReadYet(t *testing.T) {
	t.Parallel()
	blocks := []ContentBlock{
		{Type: ContentBlockTypeToolUse, ToolUseID: "t1", ToolName: "Write"},
		{
			Type:       ContentBlockTypeToolResult,
			ToolUseID:  "t1",
			ToolResult: "<tool_use_error>File has not been read yet. Read it first before writing to it.</tool_use_error>",
			IsError:    true,
		},
	}
	if _, _, ok := FinalTurnToolError(blocks); !ok {
		t.Error("Write tool_use_error must be detected")
	}
}

// TestFinalTurnToolError_SubstringWithoutIsError: content happens to
// contain the marker literal (e.g. a grep over a log file) but IsError
// is false. Must not fire — the AND rule requires both.
func TestFinalTurnToolError_SubstringWithoutIsError(t *testing.T) {
	t.Parallel()
	blocks := []ContentBlock{
		{Type: ContentBlockTypeToolUse, ToolUseID: "t1", ToolName: "Bash"},
		{
			Type:       ContentBlockTypeToolResult,
			ToolUseID:  "t1",
			ToolResult: "log line mentions <tool_use_error> in text",
			IsError:    false,
		},
	}
	if _, _, ok := FinalTurnToolError(blocks); ok {
		t.Error("IsError=false must not trigger retry even with marker in content")
	}
}

// TestFinalTurnToolError_ParallelCancelled: the real PLA-212 wire shape.
// The real error (ruff TC003) is a plain nonzero-exit with no wrapper;
// the cancelled sibling carries the <tool_use_error> wrapper. Under the
// AND rule, the cancelled sibling is the one that matches, which is
// sufficient to trigger retry — the retry prompt is just "retry" and
// the model sees the full history in context.
func TestFinalTurnToolError_ParallelCancelled(t *testing.T) {
	t.Parallel()
	blocks := []ContentBlock{
		{Type: ContentBlockTypeToolUse, ToolUseID: "ruff", ToolName: "Bash", ToolInput: map[string]interface{}{"command": "ruff check"}},
		{Type: ContentBlockTypeToolUse, ToolUseID: "pytest", ToolName: "Bash", ToolInput: map[string]interface{}{"command": "pytest"}},
		{
			Type:       ContentBlockTypeToolResult,
			ToolUseID:  "ruff",
			ToolResult: "Exit code 1\nTC003 stdlib import in runtime",
			IsError:    true,
		},
		{
			Type:       ContentBlockTypeToolResult,
			ToolUseID:  "pytest",
			ToolResult: "<tool_use_error>Cancelled: parallel tool call Bash(ruff check) errored</tool_use_error>",
			IsError:    true,
		},
	}
	name, excerpt, ok := FinalTurnToolError(blocks)
	if !ok {
		t.Fatal("expected ok=true — cancelled sibling carries the marker")
	}
	if name != "Bash" {
		t.Errorf("expected Bash, got %q", name)
	}
	if !strings.Contains(excerpt, "Cancelled") {
		t.Errorf("expected excerpt to surface the cancelled sibling, got %q", excerpt)
	}
}

func TestFinalTurnToolError_Clean(t *testing.T) {
	t.Parallel()
	blocks := []ContentBlock{
		{Type: ContentBlockTypeToolUse, ToolUseID: "t1", ToolName: "Read"},
		{Type: ContentBlockTypeToolResult, ToolUseID: "t1", ToolResult: "file contents", IsError: false},
	}
	if _, _, ok := FinalTurnToolError(blocks); ok {
		t.Fatal("expected ok=false for clean turn")
	}
}

func TestFinalTurnToolError_NoToolUse(t *testing.T) {
	t.Parallel()
	blocks := []ContentBlock{
		{Type: ContentBlockTypeText, Text: "hello"},
	}
	if _, _, ok := FinalTurnToolError(blocks); ok {
		t.Fatal("expected ok=false for text-only turn")
	}
	if _, _, ok := FinalTurnToolError(nil); ok {
		t.Fatal("expected ok=false for nil blocks")
	}
}

// TestFinalTurnToolError_BlockShape exercises the []interface{} wire
// shape handleUser stores for structured tool_result content.
func TestFinalTurnToolError_BlockShape(t *testing.T) {
	t.Parallel()
	wireShape := []interface{}{
		map[string]interface{}{
			"type": "text",
			"text": "<tool_use_error>Cancelled</tool_use_error>",
		},
	}
	blocks := []ContentBlock{
		{Type: ContentBlockTypeToolUse, ToolUseID: "t1", ToolName: "Bash"},
		{Type: ContentBlockTypeToolResult, ToolUseID: "t1", ToolResult: wireShape, IsError: true},
	}
	_, excerpt, ok := FinalTurnToolError(blocks)
	if !ok {
		t.Fatal("expected ok=true for wire-shape content")
	}
	if !strings.Contains(excerpt, "<tool_use_error>") {
		t.Errorf("expected excerpt to extract text field, got %q", excerpt)
	}
}

func TestFinalTurnToolError_ExcerptLength(t *testing.T) {
	t.Parallel()
	long := "<tool_use_error>" + strings.Repeat("x", 500) + "</tool_use_error>"
	blocks := []ContentBlock{
		{Type: ContentBlockTypeToolUse, ToolUseID: "t1", ToolName: "Bash"},
		{Type: ContentBlockTypeToolResult, ToolUseID: "t1", ToolResult: long, IsError: true},
	}
	_, excerpt, ok := FinalTurnToolError(blocks)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len([]rune(excerpt)) != toolErrorExcerptMaxRunes {
		t.Errorf("expected excerpt of %d runes, got %d", toolErrorExcerptMaxRunes, len([]rune(excerpt)))
	}
}

func TestFinalTurnToolError_UnknownTool(t *testing.T) {
	t.Parallel()
	blocks := []ContentBlock{
		{
			Type:       ContentBlockTypeToolResult,
			ToolUseID:  "orphan",
			ToolResult: "<tool_use_error>err</tool_use_error>",
			IsError:    true,
		},
	}
	name, _, ok := FinalTurnToolError(blocks)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if name != "unknown" {
		t.Errorf("expected name=unknown, got %q", name)
	}
}

// TestFinalTurnToolError_Fixture_NonzeroExitBash drives the detector
// with a wire-shape snapshot captured from evidence log 2 (gh pr checks
// exit 8). Must not trigger retry.
func TestFinalTurnToolError_Fixture_NonzeroExitBash(t *testing.T) {
	t.Parallel()
	blocks := loadRetryFixture(t, "nonzero_exit_bash.json")
	if _, _, ok := FinalTurnToolError(blocks); ok {
		t.Error("nonzero-exit gh pr checks fixture must not trigger retry")
	}
}

// TestFinalTurnToolError_Fixture_ParkedWithSkillError drives the
// detector with evidence log 1's Skill disable-model-invocation shape.
// The Skill error carries the marker, so detection fires at the turn
// level — the bg-work gate (G2) is what prevents the actual retry.
func TestFinalTurnToolError_Fixture_ParkedWithSkillError(t *testing.T) {
	t.Parallel()
	blocks := loadRetryFixture(t, "parked_with_skill_error.json")
	name, excerpt, ok := FinalTurnToolError(blocks)
	if !ok {
		t.Fatal("Skill disable-model-invocation must be detected at the detector level")
	}
	if !strings.Contains(excerpt, "disable-model-invocation") {
		t.Errorf("expected disable-model-invocation in excerpt, got %q", excerpt)
	}
	if name == "" {
		t.Error("expected non-empty tool name")
	}
}

// TestFinalTurnToolError_Fixture_RealToolUseError is the PLA-212
// parallel-cancelled shape — real error + cancelled sibling. Detection
// must fire on the cancelled sibling's wrapped content.
func TestFinalTurnToolError_Fixture_RealToolUseError(t *testing.T) {
	t.Parallel()
	blocks := loadRetryFixture(t, "real_tool_use_error.json")
	_, excerpt, ok := FinalTurnToolError(blocks)
	if !ok {
		t.Fatal("parallel-cancelled fixture must be detected")
	}
	if !strings.Contains(excerpt, "Cancelled") {
		t.Errorf("expected the wrapped sibling to be surfaced, got %q", excerpt)
	}
}

// loadRetryFixture reads a JSON snapshot of []ContentBlock from
// testdata/retry/. Fixtures are hand-extracted from real Claude CLI
// session JSONL files to guard the detector against regressions.
func loadRetryFixture(t *testing.T, name string) []ContentBlock {
	t.Helper()
	path := filepath.Join("testdata", "retry", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(data, &blocks); err != nil {
		t.Fatalf("unmarshal fixture %s: %v", name, err)
	}
	return blocks
}
