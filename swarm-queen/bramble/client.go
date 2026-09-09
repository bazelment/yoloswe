// Package bramble is a typed client for the bramble TUI's control socket.
//
// It exists because the tool surface is not what the skill documents, and every
// mined run rediscovered that the hard way. The facts below were verified by
// invocation against a live TUI, not read from help text:
//
//   - The socket path carries a PID suffix. The skill's documented preflight
//     (`${XDG_RUNTIME_DIR}/bramble-$(id -u).sock`) does not exist, so
//     `test -S "$BRAMBLE_SOCK"` fails on step 1 of every documented run.
//   - list-sessions returns a DICT wrapping a list, not a bare list. One
//     orchestrator re-derived a defensive `isinstance` guard ~15 times because
//     it never learned the shape.
//   - Session rows carry worktree_name, NOT worktree_path. Match lanes on the
//     name.
//   - There is no `bramble kill-session`. Reaping is three manual layers.
//
// Anything here that drifts should break capability_test.go rather than a run.
package bramble

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Session is one row of `bramble list-sessions`.
//
// Note the absence of a worktree PATH: only worktree_name is reported, so lanes
// must be matched on the name.
//
// Three fields are OPTIONAL and were absent from part of a live 9-session
// sample. TmuxTarget in particular is missing exactly on a `failed` session — a
// dead session has no pane — so code that assumes a pane always exists breaks on
// precisely the session it most needs to notice. Treat its absence as evidence,
// not as a parse error.
type Session struct {
	ID           string `json:"id"`
	Model        string `json:"model"`
	Prompt       string `json:"prompt"`
	Status       string `json:"status"`
	Type         string `json:"type"`
	WorktreeName string `json:"worktree_name"`

	// TmuxTarget is the pane id (e.g. "@1380"). Empty when the session has no
	// live window.
	TmuxTarget string `json:"tmux_target,omitempty"`
	// Backend is the CLI actually driving the session; absent on older rows.
	Backend string `json:"backend,omitempty"`
	// ParentSessionID is set when this session was spawned as a subagent. It is
	// the lineage a swarm needs to tell its own lanes from unrelated sessions.
	ParentSessionID string `json:"parent_session_id,omitempty"`
}

// HasPane reports whether the TUI still knows a pane for this session. A false
// result means the window is gone, which is a decision now rather than at the
// next stall timeout.
func (s Session) HasPane() bool { return s.TmuxTarget != "" }

// sessionList is the dict wrapper list-sessions actually returns.
type sessionList struct {
	Sessions []Session `json:"sessions"`
}

// Client talks to a running bramble TUI over its control socket.
type Client struct {
	// Bin is the bramble binary; defaults to "bramble" on PATH.
	Bin string
	// Sock is the control socket path, exported to the child as BRAMBLE_SOCK.
	Sock string
}

// SocketPath derives the control socket path for the running TUI.
//
// The suffix is the TUI's PID, so the path cannot be hardcoded or guessed from
// the uid alone. Finding more than one socket is an error rather than a
// newest-wins guess: picking silently would attach the swarm to the wrong TUI,
// and "select live evidence by identity, never by list position" is the rule
// that the mined runs learned the expensive way.
func SocketPath() (string, error) {
	uid := os.Getuid()
	pattern := fmt.Sprintf("/run/user/%d/bramble-%d-*.sock", uid, uid)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}
	// bramble-control-<uid>-<pid>.sock is a different socket; exclude it.
	var socks []string
	for _, m := range matches {
		if !strings.Contains(filepath.Base(m), "-control-") {
			socks = append(socks, m)
		}
	}
	switch len(socks) {
	case 1:
		return socks[0], nil
	case 0:
		return "", fmt.Errorf("no bramble socket matching %s — is the TUI running?", pattern)
	default:
		return "", fmt.Errorf("%d bramble sockets match %s (%s) — refusing to guess which TUI owns this swarm",
			len(socks), pattern, strings.Join(socks, ", "))
	}
}

// New returns a Client bound to the running TUI's socket.
func New() (*Client, error) {
	sock, err := SocketPath()
	if err != nil {
		return nil, err
	}
	return &Client{Bin: "bramble", Sock: sock}, nil
}

func (c *Client) bin() string {
	if c.Bin == "" {
		return "bramble"
	}
	return c.Bin
}

// run invokes a bramble subcommand with BRAMBLE_SOCK set.
func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	cmd.Env = append(os.Environ(), "BRAMBLE_SOCK="+c.Sock)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("bramble %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Ping reports whether the TUI is reachable.
func (c *Client) Ping(ctx context.Context) error {
	out, err := c.run(ctx, "ping")
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(string(out)); got != "pong" {
		return fmt.Errorf("bramble ping returned %q, want %q", got, "pong")
	}
	return nil
}

// ListSessions returns the live sessions known to the TUI.
func (c *Client) ListSessions(ctx context.Context) ([]Session, error) {
	out, err := c.run(ctx, "list-sessions")
	if err != nil {
		return nil, err
	}
	return parseSessions(out)
}

// parseSessions decodes list-sessions output.
//
// Split out so the dict-shape contract is testable against a captured fixture
// without a live TUI.
func parseSessions(b []byte) ([]Session, error) {
	var wrapped sessionList
	if err := json.Unmarshal(b, &wrapped); err != nil {
		return nil, fmt.Errorf("parse list-sessions: %w", err)
	}
	return wrapped.Sessions, nil
}

// CapturePane returns the last n lines of a session's pane.
func (c *Client) CapturePane(ctx context.Context, sessionID string, lines int) (string, error) {
	out, err := c.run(ctx, "capture-pane", "--session-id", sessionID)
	if err != nil {
		return "", err
	}
	text := string(out)
	if lines <= 0 {
		return text, nil
	}
	all := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n"), nil
}

// SpawnRequest describes a session to create.
type SpawnRequest struct {
	// Repo is the target repository NAME. Always set it: repository inference
	// can pick the TUI's unrelated repository.
	Repo string
	// Worktree is an existing worktree path. Prefer this over CreateWorktree for
	// any lane forking from a branch with local commits, because bramble's
	// --from resolves against the REMOTE.
	Worktree string
	// Branch and From describe a worktree bramble should create.
	Branch string
	From   string
	// Parent makes the new session a subagent that reports completion back.
	// Without it, completed lanes report nowhere.
	Parent string
	Type   string // planner | builder | codetalk
	Model  string
	Goal   string
	Prompt string
	// Backend selects the CLI independently of the model.
	Backend        string
	CreateWorktree bool
}

// Validate rejects a request that would spawn something unusable.
func (r SpawnRequest) Validate() error {
	if r.Prompt == "" {
		return fmt.Errorf("prompt is required: a session with no brief does nothing")
	}
	if r.Worktree == "" && !r.CreateWorktree {
		return fmt.Errorf("either Worktree or CreateWorktree must be set")
	}
	if r.CreateWorktree && r.Branch == "" {
		return fmt.Errorf("CreateWorktree requires a branch")
	}
	if r.Repo == "" && r.Worktree == "" {
		return fmt.Errorf("repo is required when no worktree path is given: " +
			"repository inference can choose the TUI's unrelated repository")
	}
	return nil
}

// Args renders the CLI invocation, exported so a dry run can show exactly what
// would be executed rather than describing it.
func (r SpawnRequest) Args() []string {
	args := []string{"new-session"}
	add := func(flag, val string) {
		if val != "" {
			args = append(args, flag, val)
		}
	}
	add("-r", r.Repo)
	if r.CreateWorktree {
		args = append(args, "--create-worktree")
		add("-b", r.Branch)
		add("-f", r.From)
	} else {
		add("-w", r.Worktree)
	}
	add("--parent", r.Parent)
	add("-t", r.Type)
	add("-m", r.Model)
	add("--backend", r.Backend)
	add("-g", r.Goal)
	add("-p", r.Prompt)
	return args
}

// SpawnResult identifies the created session.
type SpawnResult struct {
	SessionID    string `json:"session_id"`
	WorktreePath string `json:"worktree_path"`
}

// NewSession creates a session and returns its identity.
//
// The caller MUST record the result in the ledger in the same operation that
// spawns. Recording as a separate step is why window_id and brief decayed to
// near-zero population in real runs: under load the second step is the one that
// gets skipped.
func (c *Client) NewSession(ctx context.Context, req SpawnRequest) (SpawnResult, error) {
	if err := req.Validate(); err != nil {
		return SpawnResult{}, err
	}
	out, err := c.run(ctx, req.Args()...)
	if err != nil {
		return SpawnResult{}, err
	}
	return parseSpawnResult(out)
}

// parseSpawnResult decodes new-session output, tolerating a bare session id for
// forward compatibility with older bramble builds.
func parseSpawnResult(b []byte) (SpawnResult, error) {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" {
		return SpawnResult{}, fmt.Errorf("new-session returned no output")
	}
	var res SpawnResult
	if err := json.Unmarshal([]byte(trimmed), &res); err == nil && res.SessionID != "" {
		return res, nil
	}
	if strings.HasPrefix(trimmed, "{") {
		return SpawnResult{}, fmt.Errorf("new-session returned JSON without a session_id: %s", trimmed)
	}
	if strings.ContainsAny(trimmed, " \n") {
		return SpawnResult{}, fmt.Errorf("unrecognised new-session output: %s", trimmed)
	}
	return SpawnResult{SessionID: trimmed}, nil
}

// SendInput delivers text to a session's composer.
//
// "Queued for delivery" is NOT delivery: text can sit unsent in the composer as
// a pasted block. Confirm with the pane going busy or git state moving before
// treating a nudge as landed, and never resend blind -- one lane was found
// holding 465 stacked pastes.
func (c *Client) SendInput(ctx context.Context, sessionID, text string) error {
	_, err := c.run(ctx, "send-input", "--session-id", sessionID, "--text", text)
	return err
}

// SendKey sends one named key (Enter, Escape, C-c, ...) to a session's pane.
// This is the supported way to submit a composer; raw tmux send-keys fights it.
func (c *Client) SendKey(ctx context.Context, sessionID, key string) error {
	_, err := c.run(ctx, "send-key", "--session-id", sessionID, "--key", key)
	return err
}
