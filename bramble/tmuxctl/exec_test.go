package tmuxctl

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSendSpecialArgs(t *testing.T) {
	t.Parallel()

	got, err := sendSpecialArgs("@3", KeyEnter)
	require.NoError(t, err)
	assert.Equal(t, []string{"send-keys", "-t", "@3", "Enter"}, got)

	got, err = sendSpecialArgs("@3", KeyCtrlC)
	require.NoError(t, err)
	assert.Equal(t, []string{"send-keys", "-t", "@3", "C-c"}, got)

	_, err = sendSpecialArgs("@3", SpecialKey("rm -rf /"))
	assert.Error(t, err, "unknown special key must be rejected, never passed to tmux")
}

func TestPasteArgs(t *testing.T) {
	t.Parallel()

	buf := pasteBufferName("@3")
	assert.Equal(t, "bramble-w3", buf)
	// Distinct targets get distinct buffer names so concurrent pastes don't race.
	assert.NotEqual(t, pasteBufferName("@3"), pasteBufferName("%3"))

	assert.Equal(t,
		[]string{"set-buffer", "-b", buf, "--", "multi\nline\nprompt"},
		setBufferArgs(buf, "multi\nline\nprompt"))
	// Bracketed paste (-p) so embedded newlines are not interpreted as submits.
	assert.Equal(t,
		[]string{"paste-buffer", "-d", "-p", "-t", "@3", "-b", buf},
		pasteBufferArgs("@3", buf))
}

func TestCheckOp(t *testing.T) {
	t.Parallel()

	require.NoError(t, checkOp("send-keys"))
	require.NoError(t, checkOp("capture-pane"))
	// Destructive/server-wide commands are not allowlisted.
	assert.Error(t, checkOp("kill-server"))
	assert.Error(t, checkOp("kill-session"))
	assert.Error(t, checkOp("run-shell"))
	assert.Error(t, checkOp(""))
}

func TestExecTmuxRejectsDisallowedOp(t *testing.T) {
	t.Parallel()

	// execTmux must refuse a disallowed subcommand before ever invoking tmux.
	_, err := execTmux(context.Background(), "", []string{"kill-server"})
	assert.Error(t, err)
}

// TestExecControllerWritesUseInjectedRunner verifies the Controller write
// methods build the expected argv and route through the executor, with no real
// tmux involved.
func TestExecControllerWritesUseInjectedRunner(t *testing.T) {
	t.Parallel()

	var got [][]string
	c := &execController{run: func(_ context.Context, args []string) (string, error) {
		got = append(got, args)
		return "", nil
	}}
	ctx := context.Background()

	require.NoError(t, c.SendSpecial(ctx, "@3", KeyEnter))
	require.NoError(t, c.Paste(ctx, "@3", "prompt text"))

	require.Len(t, got, 3) // send-keys(Enter), set-buffer, paste-buffer
	assert.Equal(t, []string{"send-keys", "-t", "@3", "Enter"}, got[0])
	assert.Equal(t, "set-buffer", got[1][0])
	assert.Equal(t, "paste-buffer", got[2][0])
}

func TestExecControllerPropagatesError(t *testing.T) {
	t.Parallel()

	want := errors.New("boom")
	c := &execController{run: func(_ context.Context, _ []string) (string, error) {
		return "", want
	}}
	err := c.SendSpecial(context.Background(), "@3", KeyEnter)
	assert.ErrorIs(t, err, want)
}

func TestListParsing(t *testing.T) {
	t.Parallel()

	c := &execController{run: func(_ context.Context, args []string) (string, error) {
		// Fields are "|"-delimited with free-text (names/paths) last — mirrors the
		// production format strings, which avoid tab because tmux sanitizes control
		// bytes to "_" under a minimal locale.
		switch args[0] {
		case "list-sessions":
			return "$0|3|1|main\n$1|1|0|work\n", nil
		case "list-windows":
			return "@0|0|1|1|bash\n@1|1|0|2|claude\n", nil
		case "list-panes":
			return "%0|0|1|120|40|claude|/home/u/proj\n", nil
		}
		return "", nil
	}}
	ctx := context.Background()

	sessions, err := c.ListSessions(ctx)
	require.NoError(t, err)
	require.Len(t, sessions, 2)
	assert.Equal(t, TmuxSession{ID: "$0", Name: "main", Windows: 3, Attached: true}, sessions[0])
	assert.False(t, sessions[1].Attached)

	windows, err := c.ListWindows(ctx, "$0")
	require.NoError(t, err)
	require.Len(t, windows, 2)
	assert.Equal(t, TmuxWindow{ID: "@1", Index: 1, Name: "claude", Active: false, Panes: 2}, windows[1])
	assert.True(t, windows[0].Active)

	panes, err := c.ListPanes(ctx, "@1")
	require.NoError(t, err)
	require.Len(t, panes, 1)
	assert.Equal(t, TmuxPane{ID: "%0", Index: 0, Active: true, Command: "claude", CWD: "/home/u/proj", Width: 120, Height: 40}, panes[0])
}

// TestListWindowsPipeInName verifies a "|" embedded in the free-text name field
// (which is parsed last with a split limit) does not corrupt the fixed fields.
func TestListWindowsPipeInName(t *testing.T) {
	t.Parallel()

	c := &execController{run: func(_ context.Context, _ []string) (string, error) {
		return "@2|5|1|3|build | test\n", nil
	}}
	windows, err := c.ListWindows(context.Background(), "")
	require.NoError(t, err)
	require.Len(t, windows, 1)
	assert.Equal(t, TmuxWindow{ID: "@2", Index: 5, Active: true, Panes: 3, Name: "build | test"}, windows[0])
}

func TestCaptureStripsANSIAndBlankLines(t *testing.T) {
	t.Parallel()

	c := &execController{run: func(_ context.Context, _ []string) (string, error) {
		return "\x1b[32mhello\x1b[0m\n\n  world  \n", nil
	}}
	lines, err := c.Capture(context.Background(), "@3", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"hello", "  world"}, lines)
}
