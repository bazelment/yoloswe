package session

import (
	"slices"
	"strings"
	"testing"
)

// A session that does not receive its own ID and the control socket can read
// its peers (capture-pane rides BRAMBLE_SOCK) but can never address itself or
// write to them. Assert both are exported into the tmux window.
func TestTmuxRunnerEnvArgs_CarriesIdentityAndControlSocket(t *testing.T) {
	r := &tmuxRunner{
		sessionID:   "builder-abc123",
		brambleSock: "/run/user/1000/bramble-42.sock",
		controlSock: "/run/user/1000/bramble-control-42.sock",
	}

	args := r.envArgs()

	want := []string{
		"BRAMBLE_SOCK=/run/user/1000/bramble-42.sock",
		"BRAMBLE_SESSION_ID=builder-abc123",
		"BRAMBLE_CONTROL_SOCK=/run/user/1000/bramble-control-42.sock",
	}
	for _, kv := range want {
		if !slices.Contains(args, kv) {
			t.Errorf("envArgs() missing %q\ngot: %v", kv, args)
		}
	}

	// Every value must be preceded by its own -e flag.
	for i, a := range args {
		if strings.Contains(a, "=") && (i == 0 || args[i-1] != "-e") {
			t.Errorf("value %q at index %d is not preceded by -e\ngot: %v", a, i, args)
		}
	}
	if len(args) != 2*len(want) {
		t.Errorf("expected %d args (flag+value per var), got %d: %v", 2*len(want), len(args), args)
	}
}

// The helper tests above pass even if Start() stops splicing envArgs() into the
// tmux command — the window would launch without identity or sockets while every
// helper assertion still held. Pin the actual argv so that regression fails here.
func TestTmuxRunnerNewWindowArgs_SplicesEnvIntoInvocation(t *testing.T) {
	r := &tmuxRunner{
		windowName:  "happy-tiger",
		workDir:     "/work/repo",
		prompt:      "do the thing",
		model:       "claude-opus-4",
		sessionID:   "builder-abc123",
		brambleSock: "/run/user/1000/bramble-42.sock",
		controlSock: "/run/user/1000/bramble-control-42.sock",
	}

	args := r.newWindowArgs()

	for _, kv := range []string{
		IPCSockEnvVar + "=/run/user/1000/bramble-42.sock",
		SessionIDEnvVar + "=builder-abc123",
		ControlSockEnvVar + "=/run/user/1000/bramble-control-42.sock",
	} {
		i := slices.Index(args, kv)
		if i < 0 {
			t.Errorf("new-window argv missing %q\ngot: %v", kv, args)
			continue
		}
		if args[i-1] != "-e" {
			t.Errorf("%q is not preceded by -e in argv\ngot: %v", kv, args)
		}
	}

	// The env pairs must land before -n/-c and the trailing command, or tmux
	// treats them as part of the command rather than the window's environment.
	nameIdx := slices.Index(args, "-n")
	if nameIdx < 0 {
		t.Fatalf("argv has no -n window name: %v", args)
	}
	for i, a := range args {
		if strings.HasPrefix(a, "BRAMBLE_") && i > nameIdx {
			t.Errorf("env pair %q appears after -n at %d; tmux would not export it\ngot: %v", a, nameIdx, args)
		}
	}

	if args[0] != "new-window" {
		t.Errorf("argv[0] = %q, want new-window: %v", args[0], args)
	}
	if got := args[len(args)-2]; got != "/work/repo" {
		t.Errorf("working directory not passed via -c, got %q: %v", got, args)
	}
}

// A manager configured before the control server starts leaves controlSock
// empty; the window must still be launchable rather than exporting VAR=.
func TestTmuxRunnerEnvArgs_OmitsUnsetValues(t *testing.T) {
	r := &tmuxRunner{brambleSock: "/run/bramble.sock"}

	args := r.envArgs()

	if len(args) != 2 || args[0] != "-e" || args[1] != "BRAMBLE_SOCK=/run/bramble.sock" {
		t.Fatalf("expected only the IPC socket pair, got: %v", args)
	}
	for _, a := range args {
		if strings.HasSuffix(a, "=") {
			t.Errorf("emitted empty assignment %q", a)
		}
	}
}

// A codex window that never runs its notify program leaves bramble believing
// the session is still working after it has answered. Nothing then drains that
// session's queued messages, and its parent is only told it finished when the
// window dies — so both directions of subagent messaging go quiet. Assert the
// override is spliced in.
func TestTmuxRunnerCodexGetsNotifyHook(t *testing.T) {
	r := &tmuxRunner{
		provider:   ProviderCodex,
		model:      "gpt-5.5",
		sessionID:  "subagent-codetalk-9c89b7f5",
		brambleBin: "/home/ming/bin/bramble",
	}

	_, args := r.buildCommand()

	idx := slices.Index(args, "-c")
	if idx < 0 || idx+1 >= len(args) {
		t.Fatalf("buildCommand() has no -c override\ngot: %v", args)
	}
	got := args[idx+1]
	want := `notify=["/home/ming/bin/bramble","notify","--silent","--session-id","subagent-codetalk-9c89b7f5"]`
	if got != want {
		t.Errorf("notify override = %q, want %q", got, want)
	}
	// The prompt must stay last: codex treats the trailing argument as the
	// prompt, so an override appended after it would be swallowed.
	if args[len(args)-1] != r.prompt {
		t.Errorf("prompt is not the final argument: %v", args)
	}
}

// The notify program is only useful if bramble can be found and has an address
// to report. Without either, emit no override rather than a broken one that
// makes codex fail to start.
func TestTmuxRunnerCodexNotifyRequiresIdentityAndBinary(t *testing.T) {
	for name, r := range map[string]*tmuxRunner{
		"no session id":  {provider: ProviderCodex, brambleBin: "/bin/bramble"},
		"no bramble bin": {provider: ProviderCodex, sessionID: "s1"},
	} {
		_, args := r.buildCommand()
		if slices.Contains(args, "-c") {
			t.Errorf("%s: expected no notify override, got: %v", name, args)
		}
	}
}

// Only codex takes this override. Claude has its own Stop hook via --settings,
// and passing -c to it would be a startup error.
func TestTmuxRunnerNotifyOverrideIsCodexOnly(t *testing.T) {
	for _, provider := range []string{ProviderClaude, ProviderGemini, ProviderAgy} {
		r := &tmuxRunner{provider: provider, sessionID: "s1", brambleBin: "/bin/bramble"}
		_, args := r.buildCommand()
		for _, a := range args {
			if strings.HasPrefix(a, "notify=[") {
				t.Errorf("provider %s got a codex notify override: %v", provider, args)
			}
		}
	}
}

// The TOML value is built by string concatenation, so a path or ID containing
// a quote or backslash must not be able to break out of it.
func TestCodexNotifyConfigQuotesValues(t *testing.T) {
	got := codexNotifyConfig(`/opt/we"ird\path/bramble`, `id"1`)
	want := `notify=["/opt/we\"ird\\path/bramble","notify","--silent","--session-id","id\"1"]`
	if got != want {
		t.Errorf("codexNotifyConfig() = %q, want %q", got, want)
	}
}
