package jiradozer

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ValidationResult is the outcome of running a single validation command.
type ValidationResult struct {
	Command  string
	Stdout   string
	Stderr   string
	Duration time.Duration
	ExitCode int
	Passed   bool
}

// RunValidation executes a list of shell commands in the working directory
// and returns their results. All commands are run sequentially.
func RunValidation(ctx context.Context, workDir string, commands []string, timeout time.Duration) ([]ValidationResult, error) {
	if len(commands) == 0 {
		return nil, nil
	}

	var results []ValidationResult
	for _, cmdStr := range commands {
		result := runSingleValidation(ctx, workDir, cmdStr, timeout)
		results = append(results, result)
	}
	return results, nil
}

func runSingleValidation(ctx context.Context, workDir, command string, timeout time.Duration) ValidationResult {
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(cmdCtx, "sh", "-c", command)
	cmd.Dir = workDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	duration := time.Since(start)

	result := ValidationResult{
		Command:  command,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: duration,
		Passed:   err == nil,
	}

	if exitErr, ok := err.(*exec.ExitError); ok {
		result.ExitCode = exitErr.ExitCode()
	} else if err != nil {
		result.ExitCode = -1
	}

	return result
}

// ValidationAllPassed returns true if all validation results passed.
func ValidationAllPassed(results []ValidationResult) bool {
	for _, r := range results {
		if !r.Passed {
			return false
		}
	}
	return true
}

// FormatValidationResults formats results as markdown suitable for an issue comment.
func FormatValidationResults(results []ValidationResult) string {
	var b strings.Builder
	b.WriteString("## Validation Results\n\n")

	allPassed := ValidationAllPassed(results)
	if allPassed {
		b.WriteString("All checks passed.\n\n")
	} else {
		b.WriteString("Some checks failed.\n\n")
	}

	for _, r := range results {
		status := "PASS"
		if !r.Passed {
			status = "FAIL"
		}
		fmt.Fprintf(&b, "### `%s` — %s (%.1fs)\n\n", r.Command, status, r.Duration.Seconds())

		if !r.Passed {
			output := r.Stdout + r.Stderr
			output = truncateOutput(output, 2000)
			if output != "" {
				fmt.Fprintf(&b, "```\n%s\n```\n\n", output)
			}
		}
	}

	return b.String()
}

func truncateOutput(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "\n... (truncated)"
}
