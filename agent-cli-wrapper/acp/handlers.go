package acp

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
)

// FsHandler handles file system requests from the agent.
type FsHandler interface {
	ReadTextFile(ctx context.Context, req ReadTextFileRequest) (*ReadTextFileResponse, error)
	WriteTextFile(ctx context.Context, req WriteTextFileRequest) (*WriteTextFileResponse, error)
}

// TerminalHandler handles terminal requests from the agent.
type TerminalHandler interface {
	Create(ctx context.Context, req CreateTerminalRequest) (*CreateTerminalResponse, error)
	Output(ctx context.Context, req TerminalOutputRequest) (*TerminalOutputResponse, error)
	WaitForExit(ctx context.Context, req WaitForTerminalExitRequest) (*WaitForTerminalExitResponse, error)
	Kill(ctx context.Context, req KillTerminalRequest) (*KillTerminalResponse, error)
	Release(ctx context.Context, req ReleaseTerminalRequest) (*ReleaseTerminalResponse, error)
}

// PermissionHandler handles permission requests from the agent.
type PermissionHandler interface {
	RequestPermission(ctx context.Context, req RequestPermissionRequest) (*RequestPermissionResponse, error)
}

// --- Default Implementations ---

// DefaultFsHandler reads/writes files directly on the host filesystem.
type DefaultFsHandler struct{}

func (h *DefaultFsHandler) ReadTextFile(_ context.Context, req ReadTextFileRequest) (*ReadTextFileResponse, error) {
	data, err := os.ReadFile(req.Path)
	if err != nil {
		if os.IsNotExist(err) {
			// Return empty content for non-existent files rather than an error.
			// ACP agents (e.g. Gemini CLI) may probe for file existence via
			// read_text_file before writing, and cannot handle JSON-RPC error
			// responses gracefully in this flow.
			return &ReadTextFileResponse{Content: ""}, nil
		}
		return nil, fmt.Errorf("failed to read file %s: %w", req.Path, err)
	}

	content := string(data)

	// Apply line offset and limit if specified
	if req.Line > 0 || req.Limit > 0 {
		lines := strings.Split(content, "\n")
		start := 0
		if req.Line > 0 {
			start = req.Line - 1 // Convert 1-based to 0-based
		}
		if start >= len(lines) {
			content = ""
		} else {
			end := len(lines)
			if req.Limit > 0 && start+req.Limit < end {
				end = start + req.Limit
			}
			content = strings.Join(lines[start:end], "\n")
		}
	}

	return &ReadTextFileResponse{Content: content}, nil
}

func (h *DefaultFsHandler) WriteTextFile(_ context.Context, req WriteTextFileRequest) (*WriteTextFileResponse, error) {
	if err := os.WriteFile(req.Path, []byte(req.Content), 0644); err != nil {
		return nil, fmt.Errorf("failed to write file %s: %w", req.Path, err)
	}
	return &WriteTextFileResponse{}, nil
}

// DefaultTerminalHandler executes commands directly via os/exec.
type DefaultTerminalHandler struct {
	terminals map[string]*terminalProcess
	idGen     atomic.Int64
	mu        sync.Mutex
}

type terminalProcess struct {
	cmd    *exec.Cmd
	done   chan struct{}
	output lockedBuffer
}

// lockedBuffer is a thread-safe bytes.Buffer.
type lockedBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func NewDefaultTerminalHandler() *DefaultTerminalHandler {
	return &DefaultTerminalHandler{
		terminals: make(map[string]*terminalProcess),
	}
}

func (h *DefaultTerminalHandler) Create(ctx context.Context, req CreateTerminalRequest) (*CreateTerminalResponse, error) {
	id := fmt.Sprintf("term-%d", h.idGen.Add(1))

	args := req.Args
	cmd := exec.CommandContext(ctx, req.Command, args...)
	if req.CWD != "" {
		cmd.Dir = req.CWD
	}
	// Inherit parent environment and append custom env vars
	if len(req.Env) > 0 {
		cmd.Env = os.Environ()
		for _, env := range req.Env {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", env.Name, env.Value))
		}
	}

	tp := &terminalProcess{
		cmd:  cmd,
		done: make(chan struct{}),
	}
	cmd.Stdout = &tp.output
	cmd.Stderr = &tp.output

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start command: %w", err)
	}

	go func() {
		cmd.Wait()
		close(tp.done)
	}()

	h.mu.Lock()
	h.terminals[id] = tp
	h.mu.Unlock()

	return &CreateTerminalResponse{TerminalID: id}, nil
}

func (h *DefaultTerminalHandler) Output(_ context.Context, req TerminalOutputRequest) (*TerminalOutputResponse, error) {
	h.mu.Lock()
	tp, ok := h.terminals[req.TerminalID]
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("terminal %s not found", req.TerminalID)
	}

	output := tp.output.String()

	resp := &TerminalOutputResponse{Output: output}

	select {
	case <-tp.done:
		code := tp.cmd.ProcessState.ExitCode()
		resp.ExitStatus = &code
	default:
	}

	return resp, nil
}

func (h *DefaultTerminalHandler) WaitForExit(ctx context.Context, req WaitForTerminalExitRequest) (*WaitForTerminalExitResponse, error) {
	h.mu.Lock()
	tp, ok := h.terminals[req.TerminalID]
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("terminal %s not found", req.TerminalID)
	}

	select {
	case <-tp.done:
		return &WaitForTerminalExitResponse{
			ExitStatus: tp.cmd.ProcessState.ExitCode(),
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (h *DefaultTerminalHandler) Kill(_ context.Context, req KillTerminalRequest) (*KillTerminalResponse, error) {
	h.mu.Lock()
	tp, ok := h.terminals[req.TerminalID]
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("terminal %s not found", req.TerminalID)
	}

	if tp.cmd.Process != nil {
		tp.cmd.Process.Kill()
	}
	return &KillTerminalResponse{}, nil
}

func (h *DefaultTerminalHandler) Release(_ context.Context, req ReleaseTerminalRequest) (*ReleaseTerminalResponse, error) {
	h.mu.Lock()
	delete(h.terminals, req.TerminalID)
	h.mu.Unlock()
	return &ReleaseTerminalResponse{}, nil
}

// BypassPermissionHandler auto-approves all permission requests.
// It selects the first "allow" option, or the first option if no "allow" exists.
type BypassPermissionHandler struct{}

func (h *BypassPermissionHandler) RequestPermission(_ context.Context, req RequestPermissionRequest) (*RequestPermissionResponse, error) {
	// Find the first allow option
	for _, opt := range req.Options {
		if strings.HasPrefix(opt.Kind, "allow") {
			return &RequestPermissionResponse{
				Outcome: PermissionOutcome{
					Type:     "selected",
					OptionID: opt.ID,
				},
			}, nil
		}
	}

	// Fallback: select the first option
	if len(req.Options) > 0 {
		return &RequestPermissionResponse{
			Outcome: PermissionOutcome{
				Type:     "selected",
				OptionID: req.Options[0].ID,
			},
		}, nil
	}

	return &RequestPermissionResponse{
		Outcome: PermissionOutcome{Type: "cancelled"},
	}, nil
}

// PlanOnlyPermissionHandler allows read-only operations and rejects write operations.
// This is suitable for planner sessions that should not execute modifications.
type PlanOnlyPermissionHandler struct{}

func (h *PlanOnlyPermissionHandler) RequestPermission(_ context.Context, req RequestPermissionRequest) (*RequestPermissionResponse, error) {
	toolName := req.ToolCall.ToolName

	// Allow read-only operations
	readOnlyTools := map[string]bool{
		"read_file":       true,
		"read_text_file":  true,
		"list_directory":  true,
		"glob":            true,
		"grep":            true,
		"bash_command":    false, // Most bash commands can modify state
		"execute_command": false, // Command execution can modify state
	}

	// Check if this is a read-only tool
	if isReadOnly, exists := readOnlyTools[toolName]; exists && isReadOnly {
		// Find the first allow option
		for _, opt := range req.Options {
			if strings.HasPrefix(opt.Kind, "allow") {
				return &RequestPermissionResponse{
					Outcome: PermissionOutcome{
						Type:     "selected",
						OptionID: opt.ID,
					},
				}, nil
			}
		}
	}

	// For write operations or unknown tools, find a reject option
	for _, opt := range req.Options {
		if strings.HasPrefix(opt.Kind, "reject") {
			return &RequestPermissionResponse{
				Outcome: PermissionOutcome{
					Type:     "selected",
					OptionID: opt.ID,
				},
			}, nil
		}
	}

	// Fallback: cancel the request
	return &RequestPermissionResponse{
		Outcome: PermissionOutcome{Type: "cancelled"},
	}, nil
}
