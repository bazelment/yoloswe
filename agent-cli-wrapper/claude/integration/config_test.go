//go:build integration && manual && local
// +build integration,manual,local

// Integration tests for new configuration options.
//
// Run with: bazel test //agent-cli-wrapper/claude/integration:integration_test --test_tag_filters=manual,local

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

func TestSession_MaxTurns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	session := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithMaxTurns(2), // Limit to 2 turns
	)

	if err := session.Start(ctx); err != nil {
		t.Fatalf("failed to start session: %v", err)
	}
	defer session.Stop()

	// Turn 1 - should succeed
	result1, err := session.Ask(ctx, "Say hello")
	if err != nil {
		t.Fatalf("turn 1 failed: %v", err)
	}
	if !result1.Success {
		t.Errorf("turn 1 expected success, got error: %v", result1.Error)
	}

	// Turn 2 - should succeed but return ErrMaxTurnsExceeded
	result2, err := session.Ask(ctx, "Say goodbye")
	if err != nil {
		t.Fatalf("turn 2 failed: %v", err)
	}
	if result2.Error != claude.ErrMaxTurnsExceeded {
		t.Errorf("turn 2 expected ErrMaxTurnsExceeded, got: %v", result2.Error)
	}

	t.Logf("MaxTurns test passed: turn 1 success=%v, turn 2 error=%v",
		result1.Success, result2.Error)
}

func TestSession_AllowedTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Only allow Read tool
	session := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithAllowedTools("Read"),
	)

	if err := session.Start(ctx); err != nil {
		t.Fatalf("failed to start session: %v", err)
	}
	defer session.Stop()

	// This should work (Read is allowed)
	result, err := session.Ask(ctx, "Read the README file if it exists, otherwise just say 'no file found'")
	if err != nil {
		t.Fatalf("Ask failed: %v", err)
	}

	if !result.Success {
		t.Logf("Turn result: %v (this is expected if no README exists)", result.Error)
	}

	t.Logf("AllowedTools test completed: success=%v", result.Success)
}

func TestSession_Betas(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Test with beta flags (even if they don't exist, CLI should accept them)
	session := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithBetas("test-feature"),
	)

	if err := session.Start(ctx); err != nil {
		t.Fatalf("failed to start session with betas: %v", err)
	}
	defer session.Stop()

	result, err := session.Ask(ctx, "Say hello")
	if err != nil {
		t.Fatalf("Ask with betas failed: %v", err)
	}

	if !result.Success {
		t.Errorf("expected success with betas, got error: %v", result.Error)
	}

	t.Logf("Betas test passed")
}

func TestSession_ExtraArgs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Test with extra CLI args
	session := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithExtraArgs("--verbose"), // Extra verbose flag
	)

	if err := session.Start(ctx); err != nil {
		t.Fatalf("failed to start session with extra args: %v", err)
	}
	defer session.Stop()

	result, err := session.Ask(ctx, "Say hello")
	if err != nil {
		t.Fatalf("Ask with extra args failed: %v", err)
	}

	if !result.Success {
		t.Errorf("expected success with extra args, got error: %v", result.Error)
	}

	t.Logf("ExtraArgs test passed")
}

func TestSession_Env(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Test with custom environment variables
	session := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithEnv(map[string]string{
			"TEST_VAR": "test_value",
		}),
	)

	if err := session.Start(ctx); err != nil {
		t.Fatalf("failed to start session with env: %v", err)
	}
	defer session.Stop()

	result, err := session.Ask(ctx, "Say hello")
	if err != nil {
		t.Fatalf("Ask with env failed: %v", err)
	}

	if !result.Success {
		t.Errorf("expected success with env, got error: %v", result.Error)
	}

	t.Logf("Env test passed")
}
