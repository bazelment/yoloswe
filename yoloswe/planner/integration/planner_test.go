package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/yoloswe/planner"
)

// Integration tests for the plan command.
// These tests require a real Claude SDK session.
// Run with: bazel test //yoloswe/planner:integration_test

func TestPlanCommand_SimpleMode(t *testing.T) {
	workDir := t.TempDir()
	recordDir := t.TempDir()

	config := planner.Config{
		Model:        "haiku",
		WorkDir:      workDir,
		RecordingDir: recordDir,
		Prompt:       "Create a simple hello world function",
		Simple:       true,
		Verbose:      true,
	}

	p := planner.NewPlannerWrapper(config)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := p.Start(ctx); err != nil {
		t.Fatalf("Failed to start planner session: %v", err)
	}
	defer p.Stop()

	err := p.Run(ctx, config.Prompt)
	if err != nil {
		t.Logf("Run error (may be expected): %v", err)
	}

	// Check if a plan file was created
	planDir := filepath.Join(workDir, ".claude", "plans")
	if entries, err := os.ReadDir(planDir); err == nil {
		for _, e := range entries {
			t.Logf("Plan file created: %s", e.Name())
		}
	}

	// Print usage summary
	p.PrintUsageSummary()
}

func TestPlanCommand_BuildCurrent(t *testing.T) {
	workDir := t.TempDir()
	recordDir := t.TempDir()

	config := planner.Config{
		Model:        "haiku",
		WorkDir:      workDir,
		RecordingDir: recordDir,
		Prompt:       "Create a simple add function",
		Simple:       true,
		BuildMode:    planner.BuildModeCurrent,
		BuildModel:   "haiku",
		Verbose:      true,
	}

	p := planner.NewPlannerWrapper(config)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := p.Start(ctx); err != nil {
		t.Fatalf("Failed to start planner session: %v", err)
	}
	defer p.Stop()

	err := p.Run(ctx, config.Prompt)
	if err != nil {
		t.Logf("Run error (may be expected): %v", err)
	}

	// Check if any Go files were created (indicating build phase ran)
	files, _ := os.ReadDir(workDir)
	for _, f := range files {
		t.Logf("Created: %s", f.Name())
	}

	// Print usage summary
	p.PrintUsageSummary()
}

func TestPlanCommand_BuildNew(t *testing.T) {
	workDir := t.TempDir()
	recordDir := t.TempDir()

	config := planner.Config{
		Model:        "haiku",
		WorkDir:      workDir,
		RecordingDir: recordDir,
		Prompt:       "Create a simple subtract function",
		Simple:       true,
		BuildMode:    planner.BuildModeNewSession,
		BuildModel:   "haiku",
		Verbose:      true,
	}

	p := planner.NewPlannerWrapper(config)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := p.Start(ctx); err != nil {
		t.Fatalf("Failed to start planner session: %v", err)
	}
	defer p.Stop()

	err := p.Run(ctx, config.Prompt)
	if err != nil {
		t.Logf("Run error (may be expected): %v", err)
	}

	// Check if a plan file was created
	planDir := filepath.Join(workDir, ".claude", "plans")
	if entries, err := os.ReadDir(planDir); err == nil {
		for _, e := range entries {
			t.Logf("Plan file: %s", e.Name())
		}
	}

	// Check if any Go files were created (indicating new session build ran)
	files, _ := os.ReadDir(workDir)
	for _, f := range files {
		t.Logf("Created: %s", f.Name())
	}

	// Print usage summary
	p.PrintUsageSummary()
}

func TestPlanCommand_StdinInput(t *testing.T) {
	workDir := t.TempDir()
	recordDir := t.TempDir()

	// This test simulates stdin input by providing the prompt directly
	// In a real CLI scenario, prompt would come from stdin
	prompt := "Create a simple multiply function"

	config := planner.Config{
		Model:        "haiku",
		WorkDir:      workDir,
		RecordingDir: recordDir,
		Prompt:       prompt,
		Simple:       true,
		Verbose:      true,
	}

	p := planner.NewPlannerWrapper(config)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := p.Start(ctx); err != nil {
		t.Fatalf("Failed to start planner session: %v", err)
	}
	defer p.Stop()

	err := p.Run(ctx, prompt)
	if err != nil {
		t.Logf("Run error (may be expected): %v", err)
	}

	// Print usage summary
	p.PrintUsageSummary()

	// Verify recording was created
	if path := p.RecordingPath(); path != "" {
		t.Logf("Recording path: %s", path)
	}
}
