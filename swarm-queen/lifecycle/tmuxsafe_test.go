package lifecycle

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// privateTmux starts a tmux server on its own socket so these tests can never
// touch a live session. The original shell test made the same choice for the
// same reason: the code under test kills windows.
func privateTmux(t *testing.T) (ExecTmux, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	sock := filepath.Join(t.TempDir(), "swarm-queen-test.sock")
	tm := ExecTmux{Socket: sock}

	out, err := exec.Command("tmux", "-S", sock, "new-session", "-d",
		"-s", "guardtest", "-n", "base").CombinedOutput()
	if err != nil {
		t.Skipf("cannot start a private tmux server: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-S", sock, "kill-server").Run()
	})
	return tm, sock
}

func newWindow(t *testing.T, tm ExecTmux, name string) string {
	t.Helper()
	id, err := tm.Run(context.Background(), "new-window", "-P", "-F", "#{window_id}", "-n", name)
	if err != nil {
		t.Fatalf("new-window %s: %v", name, err)
	}
	return id
}

// THE 2026-08-27 INCIDENT, as an assertion.
//
// The orchestrator selected windows to kill with
//
//	tmux list-windows -F '#{window_index} #{window_name}' | grep ' !'
//
// believing ' !' matched a dead/bell FLAG. That format emits no flags field, so
// it matched the NAME, and bramble prefixes '!' to a window that WANTS
// ATTENTION. It killed 18 healthy sessions including itself.
//
// This asserts both halves: the historical selector really does match a healthy
// window, and the guard refuses anything that is not a @N id.
func TestIncident20260827_NameMatchingSelectsHealthyWindows(t *testing.T) {
	t.Parallel()
	tm, _ := privateTmux(t)
	decoy := newWindow(t, tm, "!kernel/decoy-bang")

	// The historical selector's format string emits no flags field at all.
	out, err := tm.Run(context.Background(), "list-windows", "-a",
		"-F", "#{window_index} #{window_name}")
	if err != nil {
		t.Fatal(err)
	}
	var matched bool
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, " !") {
			matched = true
		}
	}
	if !matched {
		t.Fatal("premise broken: the historical selector no longer matches the bang-named window")
	}

	// And window_flags never contains '!' for a healthy window.
	flags, err := tm.Run(context.Background(), "list-windows", "-a", "-F", "#{window_flags}")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(flags, "!") {
		t.Errorf("window_flags unexpectedly contains '!': %q", flags)
	}

	// The guard refuses a name outright; only @N ids are ever acceptable.
	err = SafeKillWindow(context.Background(), tm, "!kernel/decoy-bang", "@999", "decoy")
	if !errors.Is(err, ErrMalformedTarget) {
		t.Errorf("guard must refuse a window NAME, got %v", err)
	}
	if !WindowExists(context.Background(), tm, decoy) {
		t.Error("the healthy decoy window was killed")
	}
}

// Every refusal path must fail CLOSED: no kill, and a distinguishable error.
func TestSafeKillWindowRefusals(t *testing.T) {
	t.Parallel()
	tm, _ := privateTmux(t)
	victim := newWindow(t, tm, "victim")

	cases := []struct {
		want   error
		name   string
		target string
		self   string
	}{
		{ErrMalformedTarget, "window name", "kernel/lane", "@999"},
		{ErrMalformedTarget, "window index", "3", "@999"},
		{ErrMalformedTarget, "empty target", "", "@999"},
		{ErrMalformedTarget, "pane id not window id", "%17", "@999"},
		{ErrSelfUnresolvable, "self unresolvable", victim, ""},
		{ErrTargetIsSelf, "target is self", victim, victim},
		{ErrNoSuchWindow, "absent window", "@99999", "@1"},
	}
	for _, tc := range cases {
		err := SafeKillWindow(context.Background(), tm, tc.target, tc.self, tc.name)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if !WindowExists(context.Background(), tm, victim) {
		t.Error("a refused kill still destroyed the window")
	}
}

// An unresolvable self must never permit a kill. This is the fail-open hole the
// shell version documents: an unset TMUX_PANE resolves to somebody else's
// window, so the self-check would compare against the wrong session.
func TestUnresolvableSelfBlocksTheKill(t *testing.T) {
	t.Parallel()
	tm, _ := privateTmux(t)
	victim := newWindow(t, tm, "victim")

	if err := SafeKillWindow(context.Background(), tm, victim, "", "no self"); !errors.Is(err, ErrSelfUnresolvable) {
		t.Errorf("err = %v, want ErrSelfUnresolvable", err)
	}
	if !WindowExists(context.Background(), tm, victim) {
		t.Fatal("window was killed despite an unresolvable self")
	}
}

// The measured tmux behaviours that make a naive existence check fail open.
func TestTmuxFailOpenBehavioursStillHold(t *testing.T) {
	t.Parallel()
	tm, _ := privateTmux(t)

	// A bad target is not an error: rc=0 with empty output.
	out, err := tm.Run(context.Background(), "display-message", "-p", "-t", "@99999", "#{window_id}")
	if err == nil && out == "" {
		// Expected: this is exactly why WindowExists uses list-windows.
	} else if err == nil && out != "" {
		t.Errorf("display-message on a bad target returned %q; the fail-open premise changed", out)
	}

	// WindowExists must say no regardless.
	if WindowExists(context.Background(), tm, "@99999") {
		t.Error("WindowExists reported a nonexistent window as present")
	}
}

// The happy path: a real window, provably not us, is killed.
func TestSafeKillWindowKillsAProvenTarget(t *testing.T) {
	t.Parallel()
	tm, _ := privateTmux(t)
	self := newWindow(t, tm, "self")
	target := newWindow(t, tm, "lane")

	if err := SafeKillWindow(context.Background(), tm, target, self, "lane"); err != nil {
		t.Fatalf("SafeKillWindow: %v", err)
	}
	if WindowExists(context.Background(), tm, target) {
		t.Error("target survived the kill")
	}
	if !WindowExists(context.Background(), tm, self) {
		t.Error("self was killed")
	}
}

// ResolveSelf must refuse a malformed TMUX_PANE rather than interpolate it,
// because an empty -t means "the active window".
func TestResolveSelfValidatesBeforeInterpolating(t *testing.T) {
	tm, _ := privateTmux(t)
	t.Setenv("TMUX", "/tmp/fake,1,0")
	for _, bad := range []string{"", "not-a-pane", "@12", "%"} {
		t.Setenv("TMUX_PANE", bad)
		if _, err := ResolveSelf(context.Background(), tm); err == nil {
			t.Errorf("TMUX_PANE=%q was accepted; it must fail closed", bad)
		}
	}
}

func TestResolveSelfRequiresTmux(t *testing.T) {
	tm, _ := privateTmux(t)
	t.Setenv("TMUX", "")
	if _, err := ResolveSelf(context.Background(), tm); err == nil {
		t.Error("outside tmux, ResolveSelf must fail")
	}
}
