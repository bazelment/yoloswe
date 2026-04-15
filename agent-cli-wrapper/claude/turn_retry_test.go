package claude

import (
	"strings"
	"testing"
)

func TestFinalTurnToolError_IsError(t *testing.T) {
	blocks := []ContentBlock{
		{Type: ContentBlockTypeToolUse, ToolUseID: "t1", ToolName: "Bash"},
		{Type: ContentBlockTypeToolResult, ToolUseID: "t1", ToolResult: "failed", IsError: true},
	}
	name, excerpt, ok := FinalTurnToolError(blocks)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if name != "Bash" {
		t.Errorf("expected tool=Bash, got %q", name)
	}
	if excerpt != "failed" {
		t.Errorf("expected excerpt=failed, got %q", excerpt)
	}
}

func TestFinalTurnToolError_SubstringOnly(t *testing.T) {
	// IsError is false but content carries the marker — format-drift guard.
	blocks := []ContentBlock{
		{Type: ContentBlockTypeToolUse, ToolUseID: "t1", ToolName: "Bash"},
		{
			Type:       ContentBlockTypeToolResult,
			ToolUseID:  "t1",
			ToolResult: "<tool_use_error>Cancelled: parallel tool call</tool_use_error>",
			IsError:    false,
		},
	}
	name, excerpt, ok := FinalTurnToolError(blocks)
	if !ok {
		t.Fatal("expected ok=true (substring match)")
	}
	if name != "Bash" {
		t.Errorf("expected tool=Bash, got %q", name)
	}
	if !strings.Contains(excerpt, "<tool_use_error>") {
		t.Errorf("excerpt should contain marker, got %q", excerpt)
	}
}

func TestFinalTurnToolError_ParallelCancelled(t *testing.T) {
	// Exact PLA-212 shape: two parallel Bash tool_uses; the errored sibling
	// comes first, the cancelled sibling second. Detector returns the first
	// errored result — the real cause, not the cancelled one.
	blocks := []ContentBlock{
		{Type: ContentBlockTypeToolUse, ToolUseID: "ruff", ToolName: "Bash", ToolInput: map[string]interface{}{"command": "ruff check"}},
		{Type: ContentBlockTypeToolUse, ToolUseID: "pytest", ToolName: "Bash", ToolInput: map[string]interface{}{"command": "pytest"}},
		{
			Type:       ContentBlockTypeToolResult,
			ToolUseID:  "ruff",
			ToolResult: "TC003 stdlib import in runtime",
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
		t.Fatal("expected ok=true")
	}
	if name != "Bash" {
		t.Errorf("expected Bash, got %q", name)
	}
	if !strings.Contains(excerpt, "TC003") {
		t.Errorf("expected excerpt to surface the real ruff error, got %q", excerpt)
	}
	if strings.Contains(excerpt, "Cancelled") {
		t.Errorf("detector should have surfaced the ruff error, not the cancelled sibling: %q", excerpt)
	}
}

func TestFinalTurnToolError_Clean(t *testing.T) {
	blocks := []ContentBlock{
		{Type: ContentBlockTypeToolUse, ToolUseID: "t1", ToolName: "Read"},
		{Type: ContentBlockTypeToolResult, ToolUseID: "t1", ToolResult: "file contents", IsError: false},
	}
	if _, _, ok := FinalTurnToolError(blocks); ok {
		t.Fatal("expected ok=false for clean turn")
	}
}

func TestFinalTurnToolError_NoToolUse(t *testing.T) {
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

func TestFinalTurnToolError_BlockShape(t *testing.T) {
	// Wire shape from handleUser at session.go:1045 — Content is often a
	// []interface{} of {type: "text", text: "..."} maps rather than a plain string.
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
	long := strings.Repeat("x", 500)
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
	// Tool_result with no matching tool_use in the same block slice — name
	// falls back to "unknown". Shouldn't happen in practice but the detector
	// must not panic.
	blocks := []ContentBlock{
		{Type: ContentBlockTypeToolResult, ToolUseID: "orphan", ToolResult: "err", IsError: true},
	}
	name, _, ok := FinalTurnToolError(blocks)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if name != "unknown" {
		t.Errorf("expected name=unknown, got %q", name)
	}
}
