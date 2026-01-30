package claude

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/internal/ndjson"
)

// processManager manages the Claude CLI process.
type processManager struct {
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	stderr   io.ReadCloser
	cmd      *exec.Cmd
	reader   *ndjson.Reader
	writer   *ndjson.Writer
	config   SessionConfig
	mu       sync.Mutex
	started  bool
	stopping bool
}

// newProcessManager creates a new process manager.
func newProcessManager(config SessionConfig) *processManager {
	return &processManager{
		config: config,
	}
}

// BuildCLIArgs builds the CLI arguments from the config.
// This is exposed so callers can see what flags will be used.
func (pm *processManager) BuildCLIArgs() ([]string, error) {
	args := []string{
		"--print",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--model", pm.config.Model,
	}

	if pm.config.PermissionMode != "" {
		args = append(args, "--permission-mode", string(pm.config.PermissionMode))
	}

	if pm.config.DangerouslySkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}

	if pm.config.DisablePlugins {
		args = append(args, "--plugin-dir", "/dev/null")
	}

	if pm.config.PermissionPromptToolStdio {
		args = append(args, "--permission-prompt-tool", "stdio")
	}

	// Add MCP configuration if provided
	if pm.config.MCPConfig != nil && len(pm.config.MCPConfig.MCPServers) > 0 {
		mcpJSON, err := json.Marshal(pm.config.MCPConfig)
		if err != nil {
			return nil, &ProcessError{Message: "failed to marshal MCP config", Cause: err}
		}
		args = append(args, "--mcp-config", string(mcpJSON))
	}

	// Add system prompt if provided
	if pm.config.SystemPrompt != "" {
		args = append(args, "--system-prompt", pm.config.SystemPrompt)
	}

	// Add resume session ID if provided
	if pm.config.Resume != "" {
		args = append(args, "--resume", pm.config.Resume)
	}

	// Always include partial messages for tool progress tracking
	args = append(args, "--include-partial-messages")

	return args, nil
}

// Start spawns the Claude CLI process.
func (pm *processManager) Start(ctx context.Context) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pm.started {
		return ErrAlreadyStarted
	}

	// Build command arguments
	args, err := pm.BuildCLIArgs()
	if err != nil {
		return err
	}

	// Determine CLI path
	cliPath := pm.config.CLIPath
	if cliPath == "" {
		cliPath = "claude"
	}

	// Create command
	pm.cmd = exec.CommandContext(ctx, cliPath, args...)

	// Set working directory
	if pm.config.WorkDir != "" {
		pm.cmd.Dir = pm.config.WorkDir
	}

	// Get pipes
	pm.stdin, err = pm.cmd.StdinPipe()
	if err != nil {
		return &ProcessError{Message: "failed to create stdin pipe", Cause: err}
	}

	pm.stdout, err = pm.cmd.StdoutPipe()
	if err != nil {
		return &ProcessError{Message: "failed to create stdout pipe", Cause: err}
	}

	pm.stderr, err = pm.cmd.StderrPipe()
	if err != nil {
		return &ProcessError{Message: "failed to create stderr pipe", Cause: err}
	}

	// Create NDJSON reader/writer
	pm.reader = ndjson.NewReader(pm.stdout)
	pm.writer = ndjson.NewWriter(pm.stdin)

	// Start the process
	if err := pm.cmd.Start(); err != nil {
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

// WriteMessage writes a message to stdin.
func (pm *processManager) WriteMessage(msg interface{}) error {
	pm.mu.Lock()
	writer := pm.writer
	pm.mu.Unlock()

	if writer == nil {
		return ErrNotStarted
	}

	return writer.Write(msg)
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

	// Close stdin to signal graceful shutdown
	if pm.stdin != nil {
		pm.stdin.Close()
	}

	// Create a channel to wait for process exit
	done := make(chan error, 1)
	go func() {
		done <- pm.cmd.Wait()
	}()

	// Graceful shutdown sequence: stdin close -> wait 500ms -> SIGTERM -> wait 500ms -> SIGKILL
	select {
	case <-done:
		// Process exited cleanly after stdin close
		return nil
	case <-time.After(500 * time.Millisecond):
		// Process didn't exit, send SIGTERM
	}

	if pm.cmd.Process != nil {
		// Send SIGTERM for graceful shutdown
		_ = pm.cmd.Process.Signal(syscall.SIGTERM)
	}

	// Wait for process to exit after SIGTERM
	select {
	case <-done:
		return nil
	case <-time.After(500 * time.Millisecond):
		// Process didn't respond to SIGTERM, force kill
	}

	if pm.cmd.Process != nil {
		// Force kill with SIGKILL
		_ = pm.cmd.Process.Kill()
	}

	// Wait briefly for kill to take effect
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
	}

	return nil
}

// Wait waits for the process to exit and returns the exit code.
func (pm *processManager) Wait() (int, error) {
	pm.mu.Lock()
	cmd := pm.cmd
	pm.mu.Unlock()

	if cmd == nil {
		return 0, ErrNotStarted
	}

	err := cmd.Wait()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), err
		}
		return -1, err
	}
	return 0, nil
}

// IsRunning returns true if the process is running.
func (pm *processManager) IsRunning() bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if !pm.started || pm.cmd == nil || pm.cmd.Process == nil {
		return false
	}

	// Check if process has exited
	// This is a non-blocking check
	if pm.cmd.ProcessState != nil {
		return false
	}

	return true
}
