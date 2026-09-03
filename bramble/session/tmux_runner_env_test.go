package session

import (
	"slices"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"
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

func TestTmuxRunnerNewWindowArgs_OpenRouterClaudeFullArgv(t *testing.T) {
	t.Setenv(llmendpoint.OpenRouterAPIKeyEnv, "openrouter-test-key")
	r := &tmuxRunner{
		windowName:  "openrouter-claude",
		workDir:     "/work/repo",
		prompt:      "return sentinel",
		model:       "stealth/ox-alpha",
		provider:    ProviderClaude,
		llmEndpoint: llmendpoint.OpenRouter(),
	}

	want := []string{
		"new-window", "-P", "-F", "#{window_id}",
		"-e", "ANTHROPIC_API_KEY=",
		"-e", "ANTHROPIC_AUTH_TOKEN=openrouter-test-key",
		"-e", "ANTHROPIC_BASE_URL=https://openrouter.ai/api",
		"-e", "ANTHROPIC_DEFAULT_HAIKU_MODEL=stealth/ox-alpha",
		"-e", "ANTHROPIC_DEFAULT_OPUS_MODEL=stealth/ox-alpha",
		"-e", "ANTHROPIC_DEFAULT_SONNET_MODEL=stealth/ox-alpha",
		"-e", "ANTHROPIC_MODEL=stealth/ox-alpha",
		"-e", "ANTHROPIC_SMALL_FAST_MODEL=stealth/ox-alpha",
		"-n", "openrouter-claude", "-c", "/work/repo",
		"claude '--model' 'stealth/ox-alpha' 'return sentinel'",
	}
	got := r.newWindowArgs()
	if !slices.Equal(got, want) {
		t.Fatalf("newWindowArgs() mismatch\n got: %#v\nwant: %#v", got, want)
	}
	// Stated separately from the argv equality above because this one is a
	// safety property, not a formatting detail: a non-empty ANTHROPIC_API_KEY
	// makes the interactive claude CLI block on a "Detected a custom API key in
	// your environment" modal whose default answer declines the endpoint's key,
	// and nothing outside the integration harness answers startup dialogs.
	//
	// The pair must be PRESENT and EMPTY, not absent. A tmux window inherits
	// the server's global environment, so an absent pair leaves whatever the
	// user exported before starting tmux — asserting absence here is what let
	// the earlier delete()-based version look correct while the window still
	// received the user's own key.
	idx := slices.Index(got, "ANTHROPIC_API_KEY=")
	if idx < 0 {
		t.Errorf("claude window does not shadow ANTHROPIC_API_KEY; an inherited value would survive\ngot: %#v", got)
	} else if got[idx-1] != "-e" {
		t.Errorf("ANTHROPIC_API_KEY= is not preceded by -e\ngot: %#v", got)
	}
	for _, a := range got {
		if strings.HasPrefix(a, "ANTHROPIC_API_KEY=") && a != "ANTHROPIC_API_KEY=" {
			t.Errorf("interactive claude window exports a non-empty %q; it would park on the custom-API-key modal\ngot: %#v", a, got)
		}
	}
}

func TestTmuxRunnerNewWindowArgs_OpenRouterCodexFullArgv(t *testing.T) {
	t.Setenv(llmendpoint.OpenRouterAPIKeyEnv, "openrouter-test-key")
	r := &tmuxRunner{
		windowName:  "openrouter-codex",
		workDir:     "/work/repo",
		prompt:      "return sentinel",
		model:       "stealth/ox-alpha",
		provider:    ProviderCodex,
		llmEndpoint: llmendpoint.OpenRouter(),
	}

	want := []string{
		"new-window", "-P", "-F", "#{window_id}",
		"-e", "OPENROUTER_API_KEY=openrouter-test-key",
		"-n", "openrouter-codex", "-c", "/work/repo",
		"codex '--disable' 'apps' '--disable' 'browser_use' " +
			"'--disable' 'browser_use_external' '--disable' 'computer_use' " +
			"'--disable' 'enable_request_compression' '--disable' 'fast_mode' " +
			"'--disable' 'guardian_approval' '--disable' 'hooks' " +
			"'--disable' 'image_generation' '--disable' 'in_app_browser' " +
			"'--disable' 'multi_agent' '--disable' 'personality' " +
			"'--disable' 'plugins' '--disable' 'shell_snapshot' " +
			"'--disable' 'skill_mcp_dependency_install' " +
			"'--disable' 'tool_call_mcp_elicitation' '--disable' 'tool_search' " +
			"'--disable' 'tool_suggest' '--disable' 'unavailable_dummy_tools' " +
			"'-c' 'model_providers.openrouter.name=\"openrouter\"' " +
			"'-c' 'model_providers.openrouter.base_url=\"https://openrouter.ai/api/v1\"' " +
			"'-c' 'model_providers.openrouter.wire_api=\"responses\"' " +
			"'-c' 'model_providers.openrouter.env_key=\"OPENROUTER_API_KEY\"' " +
			"'-c' 'model_provider=\"openrouter\"' '-m' 'stealth/ox-alpha' 'return sentinel'",
	}
	if got := r.newWindowArgs(); !slices.Equal(got, want) {
		t.Fatalf("newWindowArgs() mismatch\n got: %#v\nwant: %#v", got, want)
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
// the session is still working after it has answered — which a polling
// orchestrator reads as a lane still running — and its parent is only told it
// finished when the window dies. Assert the override is spliced in.
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
	for _, provider := range []string{ProviderClaude, ProviderCursor, ProviderAgy} {
		r := &tmuxRunner{provider: provider, sessionID: "s1", brambleBin: "/bin/bramble"}
		_, args := r.buildCommand()
		for _, a := range args {
			if strings.HasPrefix(a, "notify=[") {
				t.Errorf("provider %s got a codex notify override: %v", provider, args)
			}
		}
	}
}

// agy binds its tools (shell cwd, file writes) to a registered "project"
// resource, not to the process's actual working directory: a session
// launched with no --new-project sits on agy's built-in default-cli-project,
// whose projectResources is empty, so the shell tool falls back to
// ~/.gemini/antigravity-cli/scratch regardless of tmux's -c workDir. This was
// confirmed by driving a live bramble Build session (DRIVE-FINDINGS G-B) and
// by direct CLI checks: `agy --model ... --prompt-interactive 'run pwd'`
// printed the scratch dir until --new-project was added, at which point agy
// registered workDir as a project resource and pwd/writes bound correctly.
// A fresh agy session must therefore always get --new-project.
func TestTmuxRunnerAgyNewSessionBindsWorktree(t *testing.T) {
	r := &tmuxRunner{
		provider: ProviderAgy,
		model:    "gemini-3.8-flash-low",
		prompt:   "do it",
		workDir:  "/home/ubuntu/worktrees/yoloswe/scratch-build-1",
	}

	_, args := r.buildCommand()

	if !slices.Contains(args, "--new-project") {
		t.Fatalf("new agy session missing --new-project, which is what binds it to workDir as the writable workspace: %v", args)
	}
}

// --conversation already resumes into the previously-registered project for
// that worktree; pairing it with --new-project would create a second,
// mismatched project resource instead of reusing the bound one.
func TestTmuxRunnerAgyResumeDoesNotReCreateProject(t *testing.T) {
	r := &tmuxRunner{
		provider:        ProviderAgy,
		model:           "gemini-3.8-flash-low",
		prompt:          "keep going",
		workDir:         "/home/ubuntu/worktrees/yoloswe/scratch-build-1",
		resumeSessionID: "conv-abc123",
	}

	_, args := r.buildCommand()

	if slices.Contains(args, "--new-project") {
		t.Errorf("resumed agy session should not get --new-project: %v", args)
	}
	if !slices.Contains(args, "--conversation") {
		t.Errorf("resumed agy session missing --conversation: %v", args)
	}
}

// agy's -p/--print greedily consumes the next token as the prompt (see
// agent-cli-wrapper/agy/process.go and its ordering tests), so every flag —
// --new-project included — must land before the trailing prompt in
// --prompt-interactive mode, never after.
func TestTmuxRunnerAgyNewProjectPrecedesPrompt(t *testing.T) {
	r := &tmuxRunner{
		provider: ProviderAgy,
		model:    "gemini-3.8-flash-low",
		prompt:   "build it",
		workDir:  "/home/ubuntu/worktrees/yoloswe/scratch-build-1",
	}

	_, args := r.buildCommand()

	idx := slices.Index(args, "--new-project")
	if idx < 0 {
		t.Fatalf("--new-project not found: %v", args)
	}
	if args[len(args)-1] != r.prompt {
		t.Fatalf("prompt is not the final argument: %v", args)
	}
	if idx >= len(args)-1 {
		t.Errorf("--new-project must precede the prompt, got index %d in %v", idx, args)
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

// codexEndpointArgs forwards codex.WithLLMEndpoint's whole flag stream, not
// just the --config half. The denylist is the part that matters and the part
// that was missing: without it codex sends tool schemas that strict Responses
// providers reject with HTTP 400 "unknown variant `namespace`", so the tmux
// path 400s where the in-process path succeeds. OpenRouter is lenient enough
// that the live test cannot catch this, which is why it is pinned here.
func TestCodexEndpointArgs_ForwardsFeatureDenylist(t *testing.T) {
	t.Setenv(llmendpoint.OpenRouterAPIKeyEnv, "openrouter-test-key")
	r := &tmuxRunner{provider: ProviderCodex, llmEndpoint: llmendpoint.OpenRouter()}

	cfg := codex.ClientConfig{}
	codex.WithLLMEndpoint(llmendpoint.OpenRouter())(&cfg)
	got := r.codexEndpointArgs()

	if len(got) != len(cfg.AppServerArgs) {
		t.Fatalf("codexEndpointArgs dropped %d of %d app-server args\ngot: %#v\nfrom: %#v",
			len(cfg.AppServerArgs)-len(got), len(cfg.AppServerArgs), got, cfg.AppServerArgs)
	}

	// Element-wise and order-preserving: --config is the only token respelled,
	// everything else — the --disable flags and every value — survives
	// verbatim. Asserting per token rather than per pair is the point: the
	// translation must not depend on the stream being all two-token flags.
	for i, want := range cfg.AppServerArgs {
		if want == "--config" {
			want = "-c"
		}
		if got[i] != want {
			t.Errorf("arg %d: got %q, want %q", i, got[i], want)
		}
	}

	if !slices.Contains(got, "--disable") {
		t.Errorf("no --disable pairs reached the interactive CLI\ngot: %#v", got)
	}
}
