package workspace

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"time"
)

// RunHook executes a shell script as a fatal hook: any failure or timeout returns an error.
// The script is run via bash -lc with workDir as the working directory.
// The provided ctx is used as the parent so that cancellation (e.g. worker termination)
// propagates to the hook process before the per-hook timeout fires.
// Spec Section 9.4.
func RunHook(ctx context.Context, script string, workDir string, timeoutMs int) error {
	if script == "" {
		return nil
	}

	timeout := time.Duration(timeoutMs) * time.Millisecond
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-lc", script)
	cmd.Dir = workDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("hook timed out after %dms: %s (stdout: %s, stderr: %s)",
				timeoutMs, script, stdout.String(), stderr.String())
		}
		return fmt.Errorf("hook failed: %w (stdout: %s, stderr: %s)",
			err, stdout.String(), stderr.String())
	}

	return nil
}

// RunHookBestEffort executes a shell script, logging and ignoring any failures.
// Spec Section 9.4: after_run and before_remove failures are logged but ignored.
func RunHookBestEffort(ctx context.Context, script string, workDir string, timeoutMs int, logger *slog.Logger) {
	if script == "" {
		return
	}

	err := RunHook(ctx, script, workDir, timeoutMs)
	if err != nil {
		logger.Warn("hook failed (best-effort, ignoring)",
			"error", err,
			"workDir", workDir,
		)
	}
}
