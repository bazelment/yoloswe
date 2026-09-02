package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/agy"
)

// writeFakeAgy writes an executable shell script standing in for the agy
// binary, mirroring the fake used in agent-cli-wrapper/agy's own tests. It
// prints the given --output-format json result payload verbatim to stdout.
func writeFakeAgy(t *testing.T, resultJSON string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agy")
	script := "#!/bin/sh\ncat <<'EOF'\n" + resultJSON + "\nEOF\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

func TestAgyProvider_Name(t *testing.T) {
	t.Parallel()

	p := NewAgyProvider()
	defer p.Close()

	assert.Equal(t, ProviderAgy, p.Name())
}

func TestAgyProvider_EventsChannel(t *testing.T) {
	t.Parallel()

	p := NewAgyProvider()
	defer p.Close()

	ch := p.Events()
	require.NotNil(t, ch)
	assert.Equal(t, (<-chan AgentEvent)(p.events), ch)
}

func TestAgyEffortLevel_MapsAllLevels(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   EffortLevel
		want string
	}{
		{EffortAuto, ""},
		{EffortLow, "low"},
		{EffortMedium, "medium"},
		{EffortHigh, "high"},
		{EffortMax, "high"}, // EffortMax clamps to agy's highest level.
		{EffortLevel("unexpected"), ""},
	} {
		t.Run(string(tc.in), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, agyEffortLevel(tc.in))
		})
	}
}

func TestAgyProvider_AcceptsExplicitEffort(t *testing.T) {
	t.Parallel()

	p := NewAgyProvider()
	defer p.Close()

	// No CLI binary in the test environment: Execute still fails, but it
	// must fail on subprocess startup, not on the effort guard that used
	// to reject any explicit non-auto level.
	_, err := p.Execute(context.Background(), "ignored", nil, WithProviderEffort(EffortHigh))
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrEffortUnsupported),
		"agy now supports effort; must not reject with ErrEffortUnsupported, got %v", err)
}

// TestAgyProvider_PopulatesSessionIDAndUsage is the reachability control for
// the id/usage round-trip: it fails if AgentResult.SessionID or AgentResult.Usage
// stop being populated from agy's parsed conversation_id/usage. See
// L7.notes.md for the verbatim revert-and-watch-it-fail output.
func TestAgyProvider_PopulatesSessionIDAndUsage(t *testing.T) {
	// Intentionally NOT t.Parallel(): fork/exec of the freshly written fake
	// agy script races ETXTBSY against other tests' cmd.Start() calls under
	// load (same known race documented in jiradozer/orchestrator_test.go).
	// TestAgyProvider_ResumeSessionIDReachesConversationFlag is serial for
	// the same reason.

	resultJSON := `{"conversation_id":"conv-provider-1","status":"SUCCESS","response":"PLATYPUS",` +
		`"duration_seconds":0.4,"num_turns":1,` +
		`"usage":{"input_tokens":200,"output_tokens":7,"thinking_tokens":0,"cache_read_tokens":30,"total_tokens":237}}`
	cliPath := writeFakeAgy(t, resultJSON)

	p := NewAgyProvider(agy.WithCLIPath(cliPath))
	defer p.Close()

	result, err := p.Execute(context.Background(), "remember PLATYPUS", nil)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, "conv-provider-1", result.SessionID, "AgentResult.SessionID must carry agy's conversation_id")
	assert.Equal(t, "PLATYPUS", result.Text)
	assert.Equal(t, 200, result.Usage.InputTokens)
	assert.Equal(t, 7, result.Usage.OutputTokens)
	assert.Equal(t, 30, result.Usage.CacheReadTokens)
}

// TestAgyProvider_ResumeSessionIDReachesConversationFlag proves the read side
// (cfg.ResumeSessionID -> agy.WithConversation) still works end to end
// through a real argv, not just through the Go option struct.
func TestAgyProvider_ResumeSessionIDReachesConversationFlag(t *testing.T) {
	// Not t.Parallel(): see the comment on TestAgyProvider_PopulatesSessionIDAndUsage.

	dir := t.TempDir()
	path := filepath.Join(dir, "agy")
	script := "#!/bin/sh\n" +
		"case \" $* \" in\n" +
		"  *' --conversation conv-provider-1 '*) ;;\n" +
		"  *) echo 'fake agy: missing --conversation conv-provider-1' >&2; exit 2 ;;\n" +
		"esac\n" +
		"cat <<'EOF'\n" +
		`{"conversation_id":"conv-provider-1","status":"SUCCESS","response":"PLATYPUS",` +
		`"duration_seconds":0.2,"num_turns":2,` +
		`"usage":{"input_tokens":50,"output_tokens":3,"thinking_tokens":0,"cache_read_tokens":10,"total_tokens":63}}` +
		"\nEOF\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))

	p := NewAgyProvider(agy.WithCLIPath(path))
	defer p.Close()

	result, err := p.Execute(context.Background(), "what was the word?", nil,
		WithProviderResumeSessionID("conv-provider-1"))
	require.NoError(t, err)
	assert.Equal(t, "PLATYPUS", result.Text)
	assert.Equal(t, "conv-provider-1", result.SessionID)
}
