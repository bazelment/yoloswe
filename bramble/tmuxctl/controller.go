package tmuxctl

import (
	"context"

	"github.com/bazelment/yoloswe/bramble/session"
)

// SpecialKey is a named key that maps to a tmux send-keys key argument. Using a
// closed set (rather than passing raw key strings) keeps the remote surface
// constrained: a caller can press Enter or Ctrl-C but cannot smuggle arbitrary
// send-keys flags.
type SpecialKey string

const (
	KeyEnter     SpecialKey = "Enter"
	KeyEscape    SpecialKey = "Escape"
	KeyCtrlC     SpecialKey = "C-c"
	KeyCtrlD     SpecialKey = "C-d"
	KeyTab       SpecialKey = "Tab"
	KeyBackspace SpecialKey = "BSpace"
	KeyUp        SpecialKey = "Up"
	KeyDown      SpecialKey = "Down"
	KeyLeft      SpecialKey = "Left"
	KeyRight     SpecialKey = "Right"
)

// tmuxKey is the literal token tmux send-keys understands for each SpecialKey.
// All current values are identity mappings, but the indirection keeps the wire
// vocabulary (SpecialKey) decoupled from tmux's key names and rejects unknowns.
var tmuxKey = map[SpecialKey]string{
	KeyEnter:     "Enter",
	KeyEscape:    "Escape",
	KeyCtrlC:     "C-c",
	KeyCtrlD:     "C-d",
	KeyTab:       "Tab",
	KeyBackspace: "BSpace",
	KeyUp:        "Up",
	KeyDown:      "Down",
	KeyLeft:      "Left",
	KeyRight:     "Right",
}

// TmuxSession describes a tmux session from `list-sessions`.
type TmuxSession struct {
	ID       string `json:"id"`       // e.g. "$0"
	Name     string `json:"name"`     // e.g. "main"
	Windows  int    `json:"windows"`  // window count
	Attached bool   `json:"attached"` // whether a client is attached
}

// TmuxWindow describes a tmux window from `list-windows`.
type TmuxWindow struct {
	ID     string `json:"id"`     // e.g. "@3"
	Name   string `json:"name"`   // window name
	Index  int    `json:"index"`  // window index within its session
	Active bool   `json:"active"` // whether this is the active window
	Panes  int    `json:"panes"`  // pane count
}

// TmuxPane describes a tmux pane from `list-panes`.
type TmuxPane struct {
	ID      string `json:"id"`      // e.g. "%5"
	Command string `json:"command"` // current command (pane_current_command)
	CWD     string `json:"cwd"`     // current working directory
	Index   int    `json:"index"`   // pane index within its window
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Active  bool   `json:"active"` // whether this is the active pane
}

// Controller is the tmux control vocabulary. Reads delegate to the bramble
// session primitives; writes and listing are implemented here. The target
// string is a tmux target-window/target-pane (e.g. "@3", "%5", or a window
// name) — callers driving bramble agent sessions resolve a SessionID to a
// target before calling (see control.Dispatcher), so target is never
// user-supplied raw input on the session-centric path.
type Controller interface {
	// Reads.
	Capture(ctx context.Context, target string, lines int) ([]string, error)
	CaptureFull(ctx context.Context, target string) (lines []string, cursorY int, err error)
	Status(ctx context.Context, target string) (*session.PaneStatus, error)
	ListSessions(ctx context.Context) ([]TmuxSession, error)
	ListWindows(ctx context.Context, sessionTarget string) ([]TmuxWindow, error)
	ListPanes(ctx context.Context, windowTarget string) ([]TmuxPane, error)

	// Writes.
	SendKeys(ctx context.Context, target, keys string, literal bool) error
	SendSpecial(ctx context.Context, target string, key SpecialKey) error
	Paste(ctx context.Context, target, text string) error

	// Navigation / lifecycle.
	Select(ctx context.Context, target string) error
	NewWindow(ctx context.Context, name, cwd, cmd string) (windowID string, err error)
	Rename(ctx context.Context, target, name string) error
	Kill(ctx context.Context, target string) error
}
