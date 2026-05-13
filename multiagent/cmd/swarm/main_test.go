package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveSessionDir(t *testing.T) {
	restore := snapshotGlobals()
	defer restore()

	workDir = filepath.Join(t.TempDir(), "repo")
	sessionDir = ""
	if got, want := resolveSessionDir(), filepath.Join(workDir, ".claude-swarm", "sessions"); got != want {
		t.Fatalf("resolveSessionDir() = %q, want %q", got, want)
	}

	sessionDir = filepath.Join(t.TempDir(), "sessions")
	if got := resolveSessionDir(); got != sessionDir {
		t.Fatalf("resolveSessionDir() = %q, want explicit sessionDir %q", got, sessionDir)
	}
}

func TestCreateSwarmConfig(t *testing.T) {
	restore := snapshotGlobals()
	defer restore()

	workDir = "/tmp/work"
	sessionDir = "/tmp/sessions"
	orchestratorModel = "opus"
	plannerModel = "sonnet"
	designerModel = "haiku"
	builderModel = "sonnet"
	reviewerModel = "gpt"
	budget = 12.5
	maxIterations = 7
	enableCheckpoint = false

	cfg := createSwarmConfig(nil)
	if cfg.WorkDir != workDir || cfg.SessionDir != sessionDir {
		t.Fatalf("config paths = (%q, %q), want (%q, %q)", cfg.WorkDir, cfg.SessionDir, workDir, sessionDir)
	}
	if cfg.OrchestratorModel != "opus" || cfg.PlannerModel != "sonnet" || cfg.DesignerModel != "haiku" ||
		cfg.BuilderModel != "sonnet" || cfg.ReviewerModel != "gpt" {
		t.Fatalf("config models = %+v", cfg)
	}
	if cfg.TotalBudgetUSD != 12.5 || cfg.MaxIterations != 7 || cfg.EnableCheckpointing {
		t.Fatalf("config limits = %+v", cfg)
	}
	if cfg.Progress != nil {
		t.Fatalf("config Progress = %v, want nil", cfg.Progress)
	}
}

func TestSetupContext(t *testing.T) {
	restore := snapshotGlobals()
	defer restore()

	parent := context.Background()

	timeout = 0
	ctx, cancel := setupContext(parent)
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("setupContext() with no timeout unexpectedly has a deadline")
	}

	timeout = time.Minute
	ctx, cancel = setupContext(parent)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("setupContext() with timeout has no deadline")
	}
	if until := time.Until(deadline); until <= 0 || until > time.Minute {
		t.Fatalf("deadline is %s away, want within configured timeout", until)
	}
}

func snapshotGlobals() func() {
	oldWorkDir := workDir
	oldSessionDir := sessionDir
	oldEnableCheckpoint := enableCheckpoint
	oldOrchestratorModel := orchestratorModel
	oldPlannerModel := plannerModel
	oldDesignerModel := designerModel
	oldBuilderModel := builderModel
	oldReviewerModel := reviewerModel
	oldBudget := budget
	oldMaxIterations := maxIterations
	oldTimeout := timeout

	return func() {
		workDir = oldWorkDir
		sessionDir = oldSessionDir
		enableCheckpoint = oldEnableCheckpoint
		orchestratorModel = oldOrchestratorModel
		plannerModel = oldPlannerModel
		designerModel = oldDesignerModel
		builderModel = oldBuilderModel
		reviewerModel = oldReviewerModel
		budget = oldBudget
		maxIterations = oldMaxIterations
		timeout = oldTimeout
	}
}
