package agy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/internal/procattr"
)

type processManager struct {
	cmd      *exec.Cmd
	prompt   string
	config   SessionConfig
	mu       sync.Mutex
	started  bool
	stopping bool
}

func newProcessManager(prompt string, config SessionConfig) *processManager {
	return &processManager{
		config: config,
		prompt: prompt,
	}
}

// BuildCLIArgs builds the agy print-mode argument list.
func (pm *processManager) BuildCLIArgs() []string {
	args := []string{"-p", pm.prompt}

	if pm.config.PrintTimeout > 0 {
		args = append(args, "--print-timeout", formatDuration(pm.config.PrintTimeout))
	}
	if pm.config.ConversationID != "" {
		args = append(args, "--conversation", pm.config.ConversationID)
	} else if pm.config.Continue {
		args = append(args, "--continue")
	}
	if pm.config.LogFile != "" {
		args = append(args, "--log-file", pm.config.LogFile)
	}
	for _, dir := range pm.config.AddDirs {
		args = append(args, "--add-dir", dir)
	}
	if pm.config.DangerouslySkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	if pm.config.Sandbox {
		args = append(args, "--sandbox")
	}
	args = append(args, pm.config.ExtraArgs...)
	return args
}

func formatDuration(d time.Duration) string {
	if d%time.Second == 0 {
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	return d.String()
}

func (pm *processManager) Start(ctx context.Context) (stdout []byte, stderr []byte, err error) {
	pm.mu.Lock()
	if pm.started {
		pm.mu.Unlock()
		return nil, nil, ErrAlreadyStarted
	}
	pm.started = true
	pm.mu.Unlock()

	cliPath := pm.config.CLIPath
	if cliPath == "" {
		cliPath = "agy"
	}

	cmd := exec.CommandContext(ctx, cliPath, pm.BuildCLIArgs()...)
	cmd.Env = os.Environ()
	for k, v := range pm.config.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if pm.config.WorkDir != "" {
		cmd.Dir = pm.config.WorkDir
	}
	procattr.Set(cmd)

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	pm.mu.Lock()
	pm.cmd = cmd
	pm.mu.Unlock()

	err = cmd.Run()
	stderr = errBuf.Bytes()
	if len(stderr) > 0 && pm.config.StderrHandler != nil {
		pm.config.StderrHandler(stderr)
	}
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return outBuf.Bytes(), stderr, &CLINotFoundError{Path: cliPath, Cause: err}
		}
		return outBuf.Bytes(), stderr, &ProcessError{
			Message: fmt.Sprintf("agy exited with stderr: %s", bytes.TrimSpace(stderr)),
			Cause:   err,
		}
	}
	return outBuf.Bytes(), stderr, nil
}

func (pm *processManager) Stop() error {
	pm.mu.Lock()
	if !pm.started || pm.stopping {
		pm.mu.Unlock()
		return nil
	}
	pm.stopping = true
	cmd := pm.cmd
	pm.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return nil
	}
	_ = procattr.SignalGroup(cmd.Process, syscall.SIGTERM)
	time.Sleep(100 * time.Millisecond)
	_ = procattr.KillGroup(cmd.Process)
	return nil
}
