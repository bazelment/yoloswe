package tmuxctl

import (
	"context"
	"sync"

	"github.com/bazelment/yoloswe/bramble/session"
)

// FakeCall records one Controller method invocation for assertions in tests.
type FakeCall struct {
	Method  string
	Target  string
	Keys    string
	Text    string
	Special SpecialKey
	Literal bool
}

// FakeController is an in-memory Controller for testing higher layers (the
// dispatcher, the remote client, the hub) without a real tmux. It records
// mutating calls and returns canned read results.
type FakeController struct {
	// Optional error to return from every method (for fault-injection tests).
	Err error

	// Canned read results.
	PaneStatus *session.PaneStatus

	Calls        []FakeCall
	CaptureLines []string
	Sessions     []TmuxSession
	Windows      []TmuxWindow
	Panes        []TmuxPane

	mu sync.Mutex
}

// NewFake returns an empty FakeController.
func NewFake() *FakeController { return &FakeController{} }

func (f *FakeController) record(c FakeCall) {
	f.mu.Lock()
	f.Calls = append(f.Calls, c)
	f.mu.Unlock()
}

// CallsFor returns the recorded calls for a given method name.
func (f *FakeController) CallsFor(method string) []FakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []FakeCall
	for _, c := range f.Calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *FakeController) Capture(_ context.Context, target string, _ int) ([]string, error) {
	f.record(FakeCall{Method: "Capture", Target: target})
	return f.CaptureLines, f.Err
}

func (f *FakeController) CaptureFull(_ context.Context, target string) ([]string, int, error) {
	f.record(FakeCall{Method: "CaptureFull", Target: target})
	return f.CaptureLines, len(f.CaptureLines), f.Err
}

func (f *FakeController) Status(_ context.Context, target string) (*session.PaneStatus, error) {
	f.record(FakeCall{Method: "Status", Target: target})
	return f.PaneStatus, f.Err
}

func (f *FakeController) ListSessions(_ context.Context) ([]TmuxSession, error) {
	f.record(FakeCall{Method: "ListSessions"})
	return f.Sessions, f.Err
}

func (f *FakeController) ListWindows(_ context.Context, target string) ([]TmuxWindow, error) {
	f.record(FakeCall{Method: "ListWindows", Target: target})
	return f.Windows, f.Err
}

func (f *FakeController) ListPanes(_ context.Context, target string) ([]TmuxPane, error) {
	f.record(FakeCall{Method: "ListPanes", Target: target})
	return f.Panes, f.Err
}

func (f *FakeController) SendKeys(_ context.Context, target, keys string, literal bool) error {
	f.record(FakeCall{Method: "SendKeys", Target: target, Keys: keys, Literal: literal})
	return f.Err
}

func (f *FakeController) SendSpecial(_ context.Context, target string, key SpecialKey) error {
	f.record(FakeCall{Method: "SendSpecial", Target: target, Special: key})
	return f.Err
}

func (f *FakeController) Paste(_ context.Context, target, text string) error {
	f.record(FakeCall{Method: "Paste", Target: target, Text: text})
	return f.Err
}

func (f *FakeController) Select(_ context.Context, target string) error {
	f.record(FakeCall{Method: "Select", Target: target})
	return f.Err
}

func (f *FakeController) NewWindow(_ context.Context, name, cwd, cmd string) (string, error) {
	f.record(FakeCall{Method: "NewWindow", Keys: name, Text: cmd, Target: cwd})
	return "@fake", f.Err
}

func (f *FakeController) Rename(_ context.Context, target, name string) error {
	f.record(FakeCall{Method: "Rename", Target: target, Keys: name})
	return f.Err
}

func (f *FakeController) Kill(_ context.Context, target string) error {
	f.record(FakeCall{Method: "Kill", Target: target})
	return f.Err
}

// compile-time assertion that FakeController satisfies Controller.
var _ Controller = (*FakeController)(nil)
