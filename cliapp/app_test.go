package cliapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestRun_ExitCodes uses the standard re-exec pattern: the test invokes
// itself as a subprocess with TEST_RUN_SCENARIO set, and the child invokes
// Run with a scripted RunFunc that returns nil / an error / a
// context-cancellation. We assert on the child's exit code.
func TestRun_ExitCodes(t *testing.T) {
	if scenario := os.Getenv("TEST_RUN_SCENARIO"); scenario != "" {
		runChildScenario(scenario)
		return
	}

	cases := []struct {
		scenario string
		wantCode int
	}{
		{"success", 0},
		{"plain_error", 1},
		{"ctx_cancelled_via_signal", 130},
		{"bad_verbosity", 2},
		{"bad_color", 2},
		{"missing_toolname", 2},
	}
	for _, c := range cases {
		t.Run(c.scenario, func(t *testing.T) {
			t.Parallel()
			tmpHome := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run", "TestRun_ExitCodes")
			cmd.Env = append(os.Environ(),
				"TEST_RUN_SCENARIO="+c.scenario,
				"HOME="+tmpHome,
			)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			err := cmd.Run()
			gotCode := exitCode(err)
			if gotCode != c.wantCode {
				t.Errorf("scenario %q: exit code = %d, want %d\nstderr:\n%s",
					c.scenario, gotCode, c.wantCode, stderr.String())
			}
		})
	}
}

func TestRun_LogFileContainsBanner(t *testing.T) {
	if os.Getenv("TEST_RUN_SCENARIO") == "banner" {
		runChildScenario("banner")
		return
	}
	tmpHome := t.TempDir()
	// We can't pass real flags to the child (Go test framework rejects
	// unknown flags). Instead we splice them into os.Args inside the child
	// via TEST_FAKE_ARGS.
	cmd := exec.Command(os.Args[0], "-test.run", "TestRun_LogFileContainsBanner")
	cmd.Env = append(os.Environ(),
		"TEST_RUN_SCENARIO=banner",
		"TEST_FAKE_ARGS=--token|secret-value|--port|8080",
		"HOME="+tmpHome,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("child exited unexpectedly: %v\nstderr:\n%s", err, stderr.String())
	}

	logDir := filepath.Join(tmpHome, ".testtool", "logs")
	entries, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatalf("read log dir %q: %v", logDir, err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 log file, got %d entries: %v", len(entries), entries)
	}
	data, err := os.ReadFile(filepath.Join(logDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "testtool starting") {
		t.Errorf("log missing banner; got:\n%s", got)
	}
	if !strings.Contains(got, "***") {
		t.Errorf("log should contain redacted ***; got:\n%s", got)
	}
	if strings.Contains(got, "secret-value") {
		t.Errorf("log leaked sensitive value secret-value; got:\n%s", got)
	}

	if !strings.Contains(stderr.String(), "Logging to") {
		t.Errorf("stderr missing 'Logging to' status; got:\n%s", stderr.String())
	}
}

// runChildScenario is invoked when the test re-execs itself. It calls Run
// with a scripted RunFunc and exits with the resulting code.
func runChildScenario(scenario string) {
	// If the parent set TEST_FAKE_ARGS, splice the values into os.Args so
	// Run's banner-redaction sees them. Real CLI flags can't be passed to a
	// Go test binary as the test framework rejects unknown flags.
	if extra := os.Getenv("TEST_FAKE_ARGS"); extra != "" {
		os.Args = append([]string{os.Args[0]}, strings.Split(extra, "|")...)
	}
	var code int
	switch scenario {
	case "success":
		code = Run(Options{ToolName: "testtool"}, func(ctx context.Context, app *App) error {
			return nil
		})
	case "plain_error":
		code = Run(Options{ToolName: "testtool"}, func(ctx context.Context, app *App) error {
			return errors.New("scripted failure")
		})
	case "ctx_cancelled_via_signal":
		code = Run(Options{ToolName: "testtool"}, func(ctx context.Context, app *App) error {
			// Trigger Run's signal handler by SIGINTing ourselves.
			_ = syscall.Kill(syscall.Getpid(), syscall.SIGINT)
			<-ctx.Done()
			return ctx.Err()
		})
	case "bad_verbosity":
		code = Run(Options{ToolName: "testtool", Verbosity: "loud"}, func(ctx context.Context, app *App) error {
			return nil
		})
	case "bad_color":
		code = Run(Options{ToolName: "testtool", Color: "rainbow"}, func(ctx context.Context, app *App) error {
			return nil
		})
	case "missing_toolname":
		code = Run(Options{}, func(ctx context.Context, app *App) error { return nil })
	case "banner":
		code = Run(Options{
			ToolName:       "testtool",
			SensitiveFlags: []string{"--token"},
		}, func(ctx context.Context, app *App) error {
			slog.Default().Info("child running")
			return nil
		})
	default:
		fmt.Fprintf(os.Stderr, "unknown TEST_RUN_SCENARIO: %s\n", scenario)
		os.Exit(99)
	}
	os.Exit(code)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
