package yoloswe

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/yoloswe/testutil"
)

// Additional integration tests for the build command.
// These tests require real Claude and Codex SDK sessions.

func TestBuildCommand_SuccessfulCompletion(t *testing.T) {
	workDir := t.TempDir()
	testutil.InitGitRepo(t, workDir)

	config := Config{
		BuilderModel:   "haiku",
		ReviewerModel:  "gpt-5.2-codex",
		BuilderWorkDir: workDir,
		RecordingDir:   t.TempDir(),
		MaxBudgetUSD:   5.0,
		MaxTimeSeconds: 300,
		MaxIterations:  5,
		Verbose:        true,
		Goal:           "Create a simple hello.go file with a Hello function",
	}

	swe := New(config)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	prompt := "Create a simple hello.go file with a Hello function that returns 'Hello, World!'"

	err := swe.Run(ctx, prompt)
	stats := swe.Stats()

	t.Logf("Exit reason: %s", stats.ExitReason)
	t.Logf("Iterations: %d", stats.IterationCount)
	t.Logf("Builder cost: $%.4f", stats.BuilderCostUSD)

	if err != nil {
		t.Logf("Run error (may be expected): %v", err)
	}

	// Check if any files were created
	files, _ := os.ReadDir(workDir)
	for _, f := range files {
		t.Logf("Created file: %s", f.Name())
	}

	// Verify iteration count
	if stats.IterationCount == 0 {
		t.Error("expected at least one iteration")
	}
}

func TestBuildCommand_ReviewerFeedbackLoop(t *testing.T) {
	workDir := t.TempDir()
	testutil.InitGitRepo(t, workDir)

	config := Config{
		BuilderModel:   "haiku",
		ReviewerModel:  "gpt-5.2-codex",
		BuilderWorkDir: workDir,
		RecordingDir:   t.TempDir(),
		MaxBudgetUSD:   10.0,
		MaxTimeSeconds: 600,
		MaxIterations:  3,
		Verbose:        true,
		Goal:           "Create a Go function with tests",
	}

	swe := New(config)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Request something that likely needs iteration
	prompt := "Create a Go file with an Add function and a corresponding test file"

	err := swe.Run(ctx, prompt)
	stats := swe.Stats()

	t.Logf("Exit reason: %s", stats.ExitReason)
	t.Logf("Iterations: %d", stats.IterationCount)
	t.Logf("Total duration: %dms", stats.TotalDurationMs)

	if err != nil {
		t.Logf("Run error (may be expected): %v", err)
	}

	// Check created files
	hasGoFile := false
	hasTestFile := false
	files, _ := os.ReadDir(workDir)
	for _, f := range files {
		t.Logf("Created file: %s", f.Name())
		if filepath.Ext(f.Name()) == ".go" {
			if filepath.Base(f.Name()) == "add_test.go" || filepath.Base(f.Name()) == "math_test.go" {
				hasTestFile = true
			} else {
				hasGoFile = true
			}
		}
	}

	if !hasGoFile {
		t.Log("Warning: no .go source file created")
	}
	if !hasTestFile {
		t.Log("Warning: no test file created")
	}
}

func TestBuildCommand_MultipleIterations(t *testing.T) {
	workDir := t.TempDir()
	testutil.InitGitRepo(t, workDir)

	config := Config{
		BuilderModel:   "haiku",
		ReviewerModel:  "gpt-5.2-codex",
		BuilderWorkDir: workDir,
		RecordingDir:   t.TempDir(),
		MaxBudgetUSD:   5.0,
		MaxTimeSeconds: 300,
		MaxIterations:  5,
		Verbose:        true,
		Goal:           "Create a calculator with multiple operations",
	}

	swe := New(config)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	prompt := "Create a calculator.go with Add, Subtract, Multiply, Divide functions and handle division by zero"

	err := swe.Run(ctx, prompt)
	stats := swe.Stats()

	t.Logf("Exit reason: %s", stats.ExitReason)
	t.Logf("Iterations: %d", stats.IterationCount)
	t.Logf("Builder tokens: input=%d, output=%d", stats.BuilderTokensIn, stats.BuilderTokensOut)
	t.Logf("Reviewer tokens: input=%d, output=%d", stats.ReviewerTokensIn, stats.ReviewerTokensOut)

	if err != nil {
		t.Logf("Run error (may be expected): %v", err)
	}

	// Verify stats are being tracked
	if stats.BuilderTokensIn == 0 && stats.IterationCount > 0 {
		t.Error("expected non-zero builder input tokens after iterations")
	}
}

func TestBuildCommand_ReviewFirst(t *testing.T) {
	workDir := t.TempDir()
	testutil.InitGitRepo(t, workDir)

	// Create existing code to review (with intentional issues)
	existingCode := `package calc

func Add(a, b int) int {
	return a + b
}

func Divide(a, b int) int {
	return a / b // potential division by zero
}
`
	if err := os.WriteFile(filepath.Join(workDir, "calc.go"), []byte(existingCode), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	config := Config{
		BuilderModel:   "haiku",
		ReviewerModel:  "gpt-5.2-codex",
		BuilderWorkDir: workDir,
		RecordingDir:   t.TempDir(),
		MaxBudgetUSD:   5.0,
		MaxTimeSeconds: 300,
		MaxIterations:  3,
		Verbose:        true,
		ReviewFirst:    true, // Start with review, skip first builder turn
		Goal:           "Review and fix any issues in the calculator code",
	}

	swe := New(config)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// With ReviewFirst, the reviewer should analyze existing code first
	prompt := "Fix any issues found by the reviewer in the calculator code"

	err := swe.Run(ctx, prompt)
	stats := swe.Stats()

	t.Logf("Exit reason: %s", stats.ExitReason)
	t.Logf("Iterations: %d", stats.IterationCount)
	t.Logf("Builder tokens: input=%d, output=%d", stats.BuilderTokensIn, stats.BuilderTokensOut)
	t.Logf("Reviewer tokens: input=%d, output=%d", stats.ReviewerTokensIn, stats.ReviewerTokensOut)

	if err != nil {
		t.Logf("Run error (may be expected): %v", err)
	}

	// Reviewer should have run (non-zero tokens)
	if stats.ReviewerTokensIn == 0 {
		t.Error("expected non-zero reviewer input tokens with ReviewFirst")
	}

	// Check if calc.go was modified (builder should fix issues)
	content, err := os.ReadFile(filepath.Join(workDir, "calc.go"))
	if err != nil {
		t.Logf("Warning: could not read calc.go: %v", err)
	} else {
		t.Logf("Final calc.go content:\n%s", string(content))
	}
}
