// Package agent implements the Codex app-server client for Symphony.
// Symphony owns the subprocess lifecycle and JSON-RPC protocol directly,
// launching via `bash -lc <command>` per spec Section 10.1.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Process manages a Codex app-server subprocess.
type Process struct {
	stdin   io.WriteCloser
	stderr  io.ReadCloser
	cmd     *exec.Cmd
	stdout  *bufio.Reader
	logger  *slog.Logger
	mu      sync.Mutex
	started bool
	stopped bool
}

// StartProcess launches the codex command via `bash -lc <command>` in the given working directory.
// Spec Section 10.1: Launch Contract.
func StartProcess(ctx context.Context, command, workDir string, logger *slog.Logger) (*Process, error) {
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	cmd.Dir = workDir

	// Set process group for orphan prevention + parent-death signal.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGTERM,
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start process: %w", err)
	}

	p := &Process{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  bufio.NewReaderSize(stdout, 10*1024*1024), // 10MB max line per spec
		stderr:  stderr,
		started: true,
		logger:  logger,
	}

	// Drain stderr in background.
	go p.drainStderr()

	return p, nil
}

// WriteJSON sends a JSON message followed by a newline to the process stdin.
func (p *Process) WriteJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	data = append(data, '\n')
	_, err = p.stdin.Write(data)
	return err
}

// ReadLine reads one line from stdout, blocking until a complete line arrives.
func (p *Process) ReadLine() ([]byte, error) {
	line, err := p.stdout.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return line, nil
}

// PID returns the process ID, or nil if not started.
func (p *Process) PID() *int {
	if p.cmd != nil && p.cmd.Process != nil {
		pid := p.cmd.Process.Pid
		return &pid
	}
	return nil
}

// Stop gracefully stops the subprocess.
// Sequence: close stdin → wait 500ms → SIGINT → wait 500ms → SIGKILL.
func (p *Process) Stop() error {
	p.mu.Lock()
	if !p.started || p.stopped {
		p.mu.Unlock()
		return nil
	}
	p.stopped = true
	p.mu.Unlock()

	// Close stdin to signal shutdown.
	if p.stdin != nil {
		p.stdin.Close()
	}

	done := make(chan error, 1)
	go func() {
		done <- p.cmd.Wait()
	}()

	select {
	case <-done:
		return nil
	case <-time.After(500 * time.Millisecond):
	}

	// Send SIGINT to process group.
	if p.cmd.Process != nil {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGINT)
	}

	select {
	case <-done:
		return nil
	case <-time.After(500 * time.Millisecond):
	}

	// Force kill process group.
	if p.cmd.Process != nil {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
	}

	return nil
}

// drainStderr reads stderr and logs it as diagnostics.
func (p *Process) drainStderr() {
	scanner := bufio.NewScanner(p.stderr)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	for scanner.Scan() {
		p.logger.Debug("codex stderr", "line", scanner.Text())
	}
}

// Exited returns a channel that is closed when the process exits.
func (p *Process) Exited() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		p.cmd.Wait()
		close(ch)
	}()
	return ch
}

// ExitCode returns the exit code of the process, or -1 if still running or unknown.
func (p *Process) ExitCode() int {
	if p.cmd.ProcessState == nil {
		return -1
	}
	code := p.cmd.ProcessState.ExitCode()
	return code
}
