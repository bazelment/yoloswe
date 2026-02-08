package wt

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatherContext(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	wtPath := filepath.Join(repoDir, "feature")
	if err := os.MkdirAll(wtPath, 0755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	mockGH := NewMockGHRunner()

	// Goal and parent
	mockGit.Results["config branch.feature.goal"] = &CmdResult{Stdout: "Fix the login bug\n"}
	mockGit.Results["config branch.feature.description"] = &CmdResult{Stdout: "parent:main\n"}

	// Diff stat
	mockGit.Results["diff --stat"] = &CmdResult{Stdout: " auth.go | 5 ++---\n 1 file changed, 2 insertions(+), 3 deletions(-)\n"}

	// Full diff
	mockGit.Results["diff"] = &CmdResult{Stdout: "diff --git a/auth.go b/auth.go\n--- a/auth.go\n+++ b/auth.go\n@@ -1 +1 @@\n-old\n+new\n"}
	mockGit.Results["diff --cached"] = &CmdResult{Stdout: ""}

	// Changed files
	mockGit.Results["diff --name-only HEAD"] = &CmdResult{Stdout: "auth.go\n"}
	mockGit.Results["ls-files --others --exclude-standard"] = &CmdResult{Stdout: "newfile.go\n"}

	// Status (for dirty/ahead/behind)
	mockGit.Results["status --porcelain"] = &CmdResult{Stdout: " M auth.go\n"}
	mockGit.Results["rev-list --left-right --count origin/feature...HEAD"] = &CmdResult{Stdout: "0\t2\n"}
	mockGit.Results["log -1 --format=%ct|%s"] = &CmdResult{Stdout: "1700000000|Fix login\n"}

	// PR info
	mockGH.Results["pr view --json number,url,state,isDraft,reviewDecision"] = &CmdResult{
		Stdout: `{"number":42,"url":"https://github.com/org/repo/pull/42","state":"OPEN","isDraft":false,"reviewDecision":"APPROVED"}`,
	}

	// Recent commits
	mockGit.Results["log -10 --format=%H|%s|%an|%ct"] = &CmdResult{
		Stdout: "abc1234567890|Fix login|Alice|1700000000\ndef5678901234|Add tests|Bob|1699990000\n",
	}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo",
		WithGitRunner(mockGit),
		WithGHRunner(mockGH),
		WithOutput(output))

	wt := Worktree{Path: wtPath, Branch: "feature", Commit: "abc12345"}
	ctx := context.Background()

	wctx, err := m.GatherContext(ctx, wt, DefaultContextOptions())
	if err != nil {
		t.Fatalf("GatherContext() error = %v", err)
	}

	// Verify identity
	if wctx.Branch != "feature" {
		t.Errorf("Branch = %q, want %q", wctx.Branch, "feature")
	}
	if wctx.Goal != "Fix the login bug" {
		t.Errorf("Goal = %q, want %q", wctx.Goal, "Fix the login bug")
	}
	if wctx.Parent != "main" {
		t.Errorf("Parent = %q, want %q", wctx.Parent, "main")
	}

	// Verify diff
	if !strings.Contains(wctx.DiffStat, "auth.go") {
		t.Errorf("DiffStat should contain 'auth.go', got %q", wctx.DiffStat)
	}
	if !strings.Contains(wctx.DiffContent, "diff --git") {
		t.Errorf("DiffContent should contain diff header, got %q", wctx.DiffContent)
	}

	// Verify files
	if len(wctx.ChangedFiles) != 1 || wctx.ChangedFiles[0] != "auth.go" {
		t.Errorf("ChangedFiles = %v, want [auth.go]", wctx.ChangedFiles)
	}
	if len(wctx.UntrackedFiles) != 1 || wctx.UntrackedFiles[0] != "newfile.go" {
		t.Errorf("UntrackedFiles = %v, want [newfile.go]", wctx.UntrackedFiles)
	}

	// Verify status
	if !wctx.IsDirty {
		t.Error("IsDirty should be true")
	}
	if wctx.Ahead != 2 {
		t.Errorf("Ahead = %d, want 2", wctx.Ahead)
	}

	// Verify PR
	if wctx.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42", wctx.PRNumber)
	}
	if wctx.PRState != "OPEN" {
		t.Errorf("PRState = %q, want %q", wctx.PRState, "OPEN")
	}

	// Verify commits
	if len(wctx.RecentCommits) != 2 {
		t.Fatalf("RecentCommits len = %d, want 2", len(wctx.RecentCommits))
	}
	if wctx.RecentCommits[0].Subject != "Fix login" {
		t.Errorf("RecentCommits[0].Subject = %q, want %q", wctx.RecentCommits[0].Subject, "Fix login")
	}
	if wctx.RecentCommits[0].Author != "Alice" {
		t.Errorf("RecentCommits[0].Author = %q, want %q", wctx.RecentCommits[0].Author, "Alice")
	}

	// Verify GatheredAt is set
	if wctx.GatheredAt.IsZero() {
		t.Error("GatheredAt should be set")
	}
}

func TestGatherContextMinimalOptions(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	wtPath := filepath.Join(repoDir, "feature")
	if err := os.MkdirAll(wtPath, 0755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	mockGH := NewMockGHRunner()
	mockGH.Result = &CmdResult{Stdout: "", ExitCode: 1}

	// Only status calls should happen
	mockGit.Results["status --porcelain"] = &CmdResult{Stdout: ""}
	mockGit.Results["rev-list --left-right --count origin/feature...HEAD"] = &CmdResult{Stdout: "0\t0\n"}
	mockGit.Results["log -1 --format=%ct|%s"] = &CmdResult{Stdout: "1700000000|Init\n"}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo",
		WithGitRunner(mockGit),
		WithGHRunner(mockGH),
		WithOutput(output))

	wt := Worktree{Path: wtPath, Branch: "feature"}
	ctx := context.Background()

	opts := ContextOptions{} // Everything off
	wctx, err := m.GatherContext(ctx, wt, opts)
	if err != nil {
		t.Fatalf("GatherContext() error = %v", err)
	}

	if wctx.DiffStat != "" {
		t.Error("DiffStat should be empty with IncludeDiffStat=false")
	}
	if wctx.DiffContent != "" {
		t.Error("DiffContent should be empty with IncludeDiff=false")
	}
	if wctx.ChangedFiles != nil {
		t.Error("ChangedFiles should be nil with IncludeFileList=false")
	}
	if wctx.RecentCommits != nil {
		t.Error("RecentCommits should be nil with IncludeCommits=0")
	}
	if wctx.PRNumber != 0 {
		t.Error("PRNumber should be 0 with IncludePRInfo=false")
	}
}

func TestGatherContextDiffTruncation(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	wtPath := filepath.Join(repoDir, "feature")
	if err := os.MkdirAll(wtPath, 0755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	mockGH := NewMockGHRunner()
	mockGH.Result = &CmdResult{Stdout: "", ExitCode: 1}

	// Large diff
	largeDiff := strings.Repeat("x", 200)
	mockGit.Results["diff"] = &CmdResult{Stdout: largeDiff}
	mockGit.Results["diff --cached"] = &CmdResult{Stdout: ""}

	// Status calls
	mockGit.Results["status --porcelain"] = &CmdResult{Stdout: ""}
	mockGit.Results["rev-list --left-right --count origin/feature...HEAD"] = &CmdResult{Stdout: "0\t0\n"}
	mockGit.Results["log -1 --format=%ct|%s"] = &CmdResult{Stdout: "1700000000|Init\n"}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo",
		WithGitRunner(mockGit),
		WithGHRunner(mockGH),
		WithOutput(output))

	wt := Worktree{Path: wtPath, Branch: "feature"}
	ctx := context.Background()

	opts := ContextOptions{
		IncludeDiff:  true,
		MaxDiffBytes: 100,
	}
	wctx, err := m.GatherContext(ctx, wt, opts)
	if err != nil {
		t.Fatalf("GatherContext() error = %v", err)
	}

	if !strings.HasSuffix(wctx.DiffContent, "... (truncated)") {
		t.Errorf("DiffContent should end with truncation marker, got %q", wctx.DiffContent[len(wctx.DiffContent)-30:])
	}
	// Should be truncated to 100 bytes + truncation suffix
	if len(wctx.DiffContent) > 200 {
		t.Errorf("DiffContent length = %d, should be close to MaxDiffBytes", len(wctx.DiffContent))
	}
}

func TestFormatForPrompt(t *testing.T) {
	t.Parallel()

	wctx := &WorktreeContext{
		Branch:         "feature-login",
		Path:           "/worktrees/repo/feature-login",
		Goal:           "Fix the OAuth login bug",
		Parent:         "main",
		DiffStat:       " auth.go | 5 ++---\n 1 file changed",
		ChangedFiles:   []string{"auth.go", "config.go"},
		UntrackedFiles: []string{"auth_test.go"},
		Ahead:          2,
		Behind:         1,
		IsDirty:        true,
		PRNumber:       42,
		PRState:        "OPEN",
		PRURL:          "https://github.com/org/repo/pull/42",
		RecentCommits: []CommitInfo{
			{Hash: "abc1234567890", Subject: "Fix login", Author: "Alice"},
		},
	}

	prompt := wctx.FormatForPrompt()

	// Check key sections are present
	for _, expected := range []string{
		"## Worktree Context",
		"**Branch:** feature-login",
		"**Goal:** Fix the OAuth login bug",
		"**Parent:** main",
		"dirty",
		"2 ahead",
		"1 behind",
		"PR:** #42 (OPEN)",
		"### Changes Summary",
		"auth.go",
		"### Modified Files",
		"config.go",
		"### Untracked Files",
		"auth_test.go",
		"### Recent Commits",
		"Fix login",
		"Alice",
	} {
		if !strings.Contains(prompt, expected) {
			t.Errorf("FormatForPrompt() missing %q", expected)
		}
	}
}

func TestFormatForPromptMinimal(t *testing.T) {
	t.Parallel()

	wctx := &WorktreeContext{
		Branch: "main",
		Path:   "/worktrees/repo/main",
	}

	prompt := wctx.FormatForPrompt()

	if !strings.Contains(prompt, "**Branch:** main") {
		t.Error("FormatForPrompt() should include branch")
	}
	if strings.Contains(prompt, "### Diff") {
		t.Error("FormatForPrompt() should not include empty diff section")
	}
	if strings.Contains(prompt, "### Modified Files") {
		t.Error("FormatForPrompt() should not include empty modified files section")
	}
}

func TestParseCommitLog(t *testing.T) {
	t.Parallel()

	output := "abc1234567890|Fix login bug|Alice|1700000000\ndef5678901234|Add tests for auth|Bob|1699990000\n"
	commits := parseCommitLog(output)

	if len(commits) != 2 {
		t.Fatalf("parseCommitLog() returned %d commits, want 2", len(commits))
	}

	if commits[0].Hash != "abc1234567890" {
		t.Errorf("commits[0].Hash = %q, want %q", commits[0].Hash, "abc1234567890")
	}
	if commits[0].Subject != "Fix login bug" {
		t.Errorf("commits[0].Subject = %q, want %q", commits[0].Subject, "Fix login bug")
	}
	if commits[0].Author != "Alice" {
		t.Errorf("commits[0].Author = %q, want %q", commits[0].Author, "Alice")
	}
	if commits[0].Date.Unix() != 1700000000 {
		t.Errorf("commits[0].Date.Unix() = %d, want 1700000000", commits[0].Date.Unix())
	}
}

func TestParseCommitLogEmpty(t *testing.T) {
	t.Parallel()

	commits := parseCommitLog("")
	if len(commits) != 0 {
		t.Errorf("parseCommitLog(\"\") returned %d commits, want 0", len(commits))
	}
}

func TestSplitNonEmpty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		expected []string
	}{
		{"a\nb\nc\n", []string{"a", "b", "c"}},
		{"\n\n", nil},
		{"single", []string{"single"}},
		{"  spaces  \n  tabs  \n", []string{"spaces", "tabs"}},
	}

	for _, tt := range tests {
		result := splitNonEmpty(tt.input)
		if len(result) != len(tt.expected) {
			t.Errorf("splitNonEmpty(%q) = %v, want %v", tt.input, result, tt.expected)
			continue
		}
		for i := range result {
			if result[i] != tt.expected[i] {
				t.Errorf("splitNonEmpty(%q)[%d] = %q, want %q", tt.input, i, result[i], tt.expected[i])
			}
		}
	}
}

func TestDefaultContextOptions(t *testing.T) {
	t.Parallel()

	opts := DefaultContextOptions()
	if !opts.IncludeDiff {
		t.Error("DefaultContextOptions().IncludeDiff should be true")
	}
	if !opts.IncludeDiffStat {
		t.Error("DefaultContextOptions().IncludeDiffStat should be true")
	}
	if !opts.IncludeFileList {
		t.Error("DefaultContextOptions().IncludeFileList should be true")
	}
	if opts.IncludeCommits != 10 {
		t.Errorf("DefaultContextOptions().IncludeCommits = %d, want 10", opts.IncludeCommits)
	}
	if !opts.IncludePRInfo {
		t.Error("DefaultContextOptions().IncludePRInfo should be true")
	}
	if opts.MaxDiffBytes != 100_000 {
		t.Errorf("DefaultContextOptions().MaxDiffBytes = %d, want 100000", opts.MaxDiffBytes)
	}
}
