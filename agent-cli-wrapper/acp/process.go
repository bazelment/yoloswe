package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/internal/procattr"
)

// processManager manages the ACP agent subprocess.
type processManager struct {
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	stderr   io.ReadCloser
	cmd      *exec.Cmd
	reader   *bufio.Reader
	encoder  *json.Encoder
	config   ClientConfig
	mu       sync.Mutex
	started  bool
	stopping bool
}

func newProcessManager(config ClientConfig) *processManager {
	return &processManager{config: config}
}

// Start spawns the ACP agent process.
func (pm *processManager) Start(ctx context.Context) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pm.started {
		return ErrAlreadyStarted
	}

	// Build command
	pm.cmd = exec.CommandContext(ctx, pm.config.BinaryPath, pm.config.BinaryArgs...)

	// Configure process group for orphan prevention.
	procattr.Set(pm.cmd)

	// Set environment variables
	if len(pm.config.Env) > 0 {
		pm.cmd.Env = os.Environ()
		for k, v := range pm.config.Env {
			pm.cmd.Env = append(pm.cmd.Env, k+"="+v)
		}
	}

	// Set up pipes
	var err error
	pm.stdin, err = pm.cmd.StdinPipe()
	if err != nil {
		return &ProcessError{Message: "failed to get stdin pipe", Cause: err}
	}

	pm.stdout, err = pm.cmd.StdoutPipe()
	if err != nil {
		return &ProcessError{Message: "failed to get stdout pipe", Cause: err}
	}

	pm.stderr, err = pm.cmd.StderrPipe()
	if err != nil {
		return &ProcessError{Message: "failed to get stderr pipe", Cause: err}
	}

	// Start the process
	if err := pm.cmd.Start(); err != nil {
		return &ProcessError{Message: "failed to start agent process", Cause: err}
	}

	// Set up reader/encoder
	pm.reader = bufio.NewReader(pm.stdout)
	pm.encoder = json.NewEncoder(pm.stdin)

	pm.started = true
	return nil
}

// ReadLine reads a single newline-delimited JSON line from stdout.
func (pm *processManager) ReadLine() ([]byte, error) {
	pm.mu.Lock()
	reader := pm.reader
	pm.mu.Unlock()

	if reader == nil {
		return nil, io.EOF
	}

	line, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, err
	}

	// Trim the newline
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}

	return line, nil
}

// WriteJSON writes a JSON message to stdin.
func (pm *processManager) WriteJSON(v interface{}) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pm.encoder == nil {
		return ErrNotStarted
	}
	if pm.stopping {
		return ErrStopping
	}

	return pm.encoder.Encode(v)
}

// Stop gracefully stops the process.
func (pm *processManager) Stop() error {
	pm.mu.Lock()
	if !pm.started || pm.stopping {
		pm.mu.Unlock()
		return nil
	}
	pm.stopping = true
	pm.mu.Unlock()

	// Close stdin to signal shutdown
	if pm.stdin != nil {
		pm.stdin.Close()
	}

	// Wait for process to exit with timeout
	done := make(chan error, 1)
	go func() {
		done <- pm.cmd.Wait()
	}()

	select {
	case <-done:
		// Process exited cleanly
	case <-time.After(500 * time.Millisecond):
		// Send SIGINT to the entire process group for graceful shutdown.
		if pm.cmd.Process != nil {
			_ = procattr.SignalGroup(pm.cmd.Process, syscall.SIGINT)
		}

		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			// Force kill the entire process group.
			if pm.cmd.Process != nil {
				_ = procattr.KillGroup(pm.cmd.Process)
			}
			select {
			case <-done:
			case <-time.After(200 * time.Millisecond):
			}
		}
	}

	return nil
}

// startStderrReader starts a goroutine to read stderr.
func (pm *processManager) startStderrReader(handler func([]byte)) {
	if pm.stderr == nil || handler == nil {
		return
	}

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := pm.stderr.Read(buf)
			if err != nil {
				return
			}
			if n > 0 {
				handler(buf[:n])
			}
		}
	}()
}
