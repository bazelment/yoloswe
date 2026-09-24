package session

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"
	"github.com/bazelment/yoloswe/multiagent/agent"
)

// ControlSockPath follows the same late-binding contract as IPCSockPath: the
// control server picks its path after the manager is built, so the setter must
// be reachable post-construction and the config field must feed the getter.
func TestManagerControlSockPath(t *testing.T) {
	t.Parallel()

	// ControlSockPath should be empty by default.
	m := NewManager()
	assert.Equal(t, "", m.ControlSockPath())

	// SetControlSockPath should update the path; ControlSockPath() reflects it.
	const sockPath = "/tmp/bramble-control-test.sock"
	m.SetControlSockPath(sockPath)
	assert.Equal(t, sockPath, m.ControlSockPath())

	// ManagerConfig.ControlSockPath is wired through to the getter.
	m2 := NewManagerWithConfig(ManagerConfig{ControlSockPath: sockPath})
	assert.Equal(t, sockPath, m2.ControlSockPath())
}

// envArgs() proves the runner exports whatever sockets it holds, but not that
// the manager ever hands them over. This pins the seam between them: a session
// launched by a manager that knows both sockets must end up with both in its
// tmux environment. Dropping either assignment in newTmuxRunner fails here.
func TestManagerNewTmuxRunnerCarriesSocketsAndIdentity(t *testing.T) {
	t.Parallel()

	const (
		ipcSock     = "/run/user/1000/bramble-42.sock"
		controlSock = "/run/user/1000/bramble-control-42.sock"
	)
	m := NewManagerWithConfig(ManagerConfig{
		IPCSockPath:     ipcSock,
		ControlSockPath: controlSock,
	})

	sess := &Session{
		ID:           "builder-abc123",
		Type:         SessionTypeBuilder,
		WorktreePath: "/tmp/worktree",
	}
	runner := m.newTmuxRunner(sess, "do the thing", "happy-tiger", agent.AgentModel{
		ID:       "opus",
		Provider: ProviderClaude,
	})
	require.NotNil(t, runner)

	// The manager's sockets and the session's own ID reached the runner...
	assert.Equal(t, ipcSock, runner.brambleSock)
	assert.Equal(t, controlSock, runner.controlSock)
	assert.Equal(t, string(sess.ID), runner.sessionID)

	// ...and survive all the way into the tmux window's environment.
	args := runner.envArgs()
	for _, kv := range []string{
		IPCSockEnvVar + "=" + ipcSock,
		SessionIDEnvVar + "=" + string(sess.ID),
		ControlSockEnvVar + "=" + controlSock,
	} {
		assert.Contains(t, args, kv, "envArgs() lost %q\ngot: %v", kv, args)
	}
	assert.True(t, slices.Contains(args, "-e"), "expected -e flags in %v", args)
}

// A manager whose control server has not started yet must still produce a
// launchable window rather than exporting BRAMBLE_CONTROL_SOCK= with no value.
func TestManagerNewTmuxRunnerOmitsUnstartedControlSocket(t *testing.T) {
	t.Parallel()

	m := NewManagerWithConfig(ManagerConfig{IPCSockPath: "/run/bramble.sock"})
	runner := m.newTmuxRunner(&Session{ID: "builder-xyz"}, "prompt", "window", agent.AgentModel{ID: "opus"})

	assert.Equal(t, "", runner.controlSock)
	for _, a := range runner.envArgs() {
		assert.NotContains(t, a, ControlSockEnvVar, "unset control socket should be omitted, got %q", a)
	}
}

// The endpoint reaches the tmux window only if newTmuxRunner copies it off the
// session. Wiring every earlier layer and dropping it here would leave the
// window pointed at the default provider with no error anywhere.
func TestManagerNewTmuxRunnerCarriesSessionLLMEndpoint(t *testing.T) {
	t.Parallel()

	endpoint := llmendpoint.OpenRouter()
	m := NewManagerWithConfig(ManagerConfig{})
	runner := m.newTmuxRunner(
		&Session{ID: "builder-openrouter", Model: "stealth/ox-alpha", LLMEndpoint: endpoint},
		"prompt",
		"window",
		agent.AgentModel{ID: "stealth/ox-alpha", Provider: ProviderCodex},
	)

	assert.Equal(t, endpoint, runner.llmEndpoint)
}

// Effort reaches the tmux command line only if newTmuxRunner copies it off the
// session; buildCommand's own tests start from a runner that already has it.
func TestManagerNewTmuxRunnerCarriesSessionEffort(t *testing.T) {
	t.Parallel()

	m := NewManagerWithConfig(ManagerConfig{})
	runner := m.newTmuxRunner(
		&Session{ID: "builder-effort", Model: "gpt-5.5", Effort: "xhigh"},
		"prompt",
		"window",
		agent.AgentModel{ID: "gpt-5.5", Provider: ProviderCodex},
	)

	assert.Equal(t, "xhigh", runner.effort)
}
