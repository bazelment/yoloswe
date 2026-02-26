package cursor

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/internal/ndjson"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/internal/procattr"
)

// processManager manages the Cursor Agent CLI process.
// Unlike Claude, Cursor operates in one-shot mode (no stdin writer).
type processManager struct {
	stdout   io.ReadCloser
	stderr   io.ReadCloser
	cmd      *exec.Cmd
	reader   *ndjson.Reader
	config   SessionConfig
	prompt   string
	mu       sync.Mutex
	started  bool
	stopping bool
}

// newProcessManager creates a new process manager.
func newProcessManager(prompt string, config SessionConfig) *processManager {
	return &processManager{
		config: config,
		prompt: prompt,
	}
}

// BuildCLIArgs builds the CLI arguments from the config and prompt.
//
// The Cursor Agent CLI uses: agent chat -p <prompt> --output-format stream-json [options]
func (pm *processManager) BuildCLIArgs() []string {
	args := []string{
		"chat",
		"-p", pm.prompt,
		"--output-format", "stream-json",
	}

	if pm.config.Model != "" {
		args = append(args, "--model", pm.config.Model)
	}

	if pm.config.Force {
		args = append(args, "--force")
	}

	if pm.config.Trust {
		args = append(args, "--trust")
	}

	if pm.config.Sandbox {
		args = append(args, "--sandbox")
	}

	// Add extra args (escape hatch)
	args = append(args, pm.config.ExtraArgs...)

	return args
}

// Start spawns the Cursor Agent CLI process.
func (pm *processManager) Start(ctx context.Context) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pm.started {
		return ErrAlreadyStarted
	}

	args := pm.BuildCLIArgs()

	cliPath := pm.config.CLIPath
	if cliPath == "" {
		cliPath = "agent"
	}

	pm.cmd = exec.CommandContext(ctx, cliPath, args...)

	// Set environment variables
	pm.cmd.Env = os.Environ()
	for k, v := range pm.config.Env {
		pm.cmd.Env = append(pm.cmd.Env, k+"="+v)
	}

	// Configure process group for orphan prevention
	procattr.Set(pm.cmd)

	if pm.config.WorkDir != "" {
		pm.cmd.Dir = pm.config.WorkDir
	}

	var err error
	pm.stdout, err = pm.cmd.StdoutPipe()
	if err != nil {
		return &ProcessError{Message: "failed to create stdout pipe", Cause: err}
	}

	pm.stderr, err = pm.cmd.StderrPipe()
	if err != nil {
		return &ProcessError{Message: "failed to create stderr pipe", Cause: err}
	}

	pm.reader = ndjson.NewReader(pm.stdout)

	if err := pm.cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return &CLINotFoundError{Path: cliPath, Cause: err}
		}
		return &ProcessError{Message: "failed to start CLI process", Cause: err}
	}

	pm.started = true
	return nil
}

// ReadLine reads the next JSON line from stdout.
func (pm *processManager) ReadLine() ([]byte, error) {
	pm.mu.Lock()
	reader := pm.reader
	pm.mu.Unlock()

	if reader == nil {
		return nil, ErrNotStarted
	}

	return reader.ReadLine()
}

// Stderr returns the stderr reader.
func (pm *processManager) Stderr() io.Reader {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.stderr
}

// Stop gracefully shuts down the CLI process.
func (pm *processManager) Stop() error {
	pm.mu.Lock()
	if !pm.started || pm.stopping {
		pm.mu.Unlock()
		return nil
	}
	pm.stopping = true
	pm.mu.Unlock()

	// Create a channel to wait for process exit
	done := make(chan error, 1)
	go func() {
		done <- pm.cmd.Wait()
	}()

	// Graceful shutdown: SIGTERM → wait 500ms → SIGKILL
	if pm.cmd.Process != nil {
		_ = procattr.SignalGroup(pm.cmd.Process, syscall.SIGTERM)
	}

	select {
	case <-done:
		return nil
	case <-time.After(500 * time.Millisecond):
		// Process didn't respond to SIGTERM, force kill
	}

	if pm.cmd.Process != nil {
		_ = procattr.KillGroup(pm.cmd.Process)
	}

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
	}

	return nil
}

