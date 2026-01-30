package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// processManager manages the codex app-server subprocess.
type processManager struct {
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	stderr     io.ReadCloser
	cmd        *exec.Cmd
	reader     *bufio.Reader
	encoder    *json.Encoder
	sessionLog *os.File
	config     ClientConfig
	mu         sync.Mutex
	started    bool
	stopping   bool
}

func newProcessManager(config ClientConfig) *processManager {
	return &processManager{
		config: config,
	}
}

// Start spawns the codex app-server process.
func (pm *processManager) Start(ctx context.Context) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pm.started {
		return ErrAlreadyStarted
	}

	// Determine codex binary path
	codexPath := pm.config.CodexPath
	if codexPath == "" {
		codexPath = "codex"
	}

	// Build command
	pm.cmd = exec.CommandContext(ctx, codexPath, "app-server")

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
		return &ProcessError{Message: "failed to start codex app-server", Cause: err}
	}

	// Set up reader/encoder
	pm.reader = bufio.NewReader(pm.stdout)
	pm.encoder = json.NewEncoder(pm.stdin)

	// Set up session logging if configured
	if pm.config.SessionLogPath != "" {
		f, err := os.Create(pm.config.SessionLogPath)
		if err != nil {
			// Clean up pipes before killing
			pm.stdin.Close()
			pm.stdout.Close()
			pm.stderr.Close()
			pm.cmd.Process.Kill()
			pm.cmd.Wait() // Reap the process
			return &ProcessError{Message: "failed to create session log", Cause: err}
		}
		pm.sessionLog = f

		// Write session log header
		header := NewSessionLogHeader(pm.config.ClientName)
		enc := json.NewEncoder(pm.sessionLog)
		enc.Encode(header)
	}

	pm.started = true
	return nil
}

// ReadLine reads a single line from stdout.
func (pm *processManager) ReadLine() ([]byte, error) {
	pm.mu.Lock()
	reader := pm.reader
	sessionLog := pm.sessionLog
	pm.mu.Unlock()

	if reader == nil {
		return nil, io.EOF
	}

	line, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, err
	}

	// Log to session file if configured
	if sessionLog != nil {
		pm.logMessage("received", line)
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

	// Log to session file if configured
	if pm.sessionLog != nil {
		data, _ := json.Marshal(v)
		pm.logMessageLocked("sent", data)
	}

	return pm.encoder.Encode(v)
}

// Stderr returns the stderr reader for handling stderr output.
func (pm *processManager) Stderr() io.ReadCloser {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.stderr
}

// Stop gracefully stops the process.
func (pm *processManager) Stop() error {
	pm.mu.Lock()
	if !pm.started || pm.stopping {
		pm.mu.Unlock()
		return nil
	}
	pm.stopping = true

	// Close session log
	if pm.sessionLog != nil {
		pm.sessionLog.Close()
		pm.sessionLog = nil
	}
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
		// Send SIGTERM
		if pm.cmd.Process != nil {
			pm.cmd.Process.Signal(os.Interrupt)
		}

		select {
		case <-done:
			// Process exited after SIGTERM
		case <-time.After(500 * time.Millisecond):
			// Force kill
			if pm.cmd.Process != nil {
				pm.cmd.Process.Kill()
			}
			<-done
		}
	}

	return nil
}

// stderrReader starts a goroutine to read stderr.
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

// logMessage logs a message to the session log (acquires lock).
func (pm *processManager) logMessage(direction string, data []byte) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.logMessageLocked(direction, data)
}

// logMessageLocked logs a message to the session log (caller must hold lock).
func (pm *processManager) logMessageLocked(direction string, data []byte) {
	if pm.sessionLog == nil {
		return
	}

	entry := NewSessionLogEntry(direction, data)
	enc := json.NewEncoder(pm.sessionLog)
	enc.Encode(entry)
}
