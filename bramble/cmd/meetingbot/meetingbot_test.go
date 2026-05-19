package meetingbot

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	botpkg "github.com/bazelment/yoloswe/bramble/meetingbot"
)

func TestBuildClientFailsClosedToLocal(t *testing.T) {
	// An absent or empty mode must resolve to the local client (the --agent
	// flag default), never the real provider, so a blank value cannot silently
	// trigger network calls or require credentials.
	for _, mode := range []string{"", "  ", "local", "LOCAL", "offline"} {
		c, err := buildClient(mode)
		require.NoError(t, err, "mode=%q", mode)
		require.IsType(t, botpkg.LocalAgentClient{}, c, "mode=%q", mode)
	}

	c, err := buildClient("real")
	require.NoError(t, err)
	require.IsType(t, botpkg.ProviderAgentClient{}, c)

	_, err = buildClient("bogus")
	require.Error(t, err)
}

const cmdSampleTranscript = `[00:02-00:05] Speaker A: You can start.
[00:54-01:13] Speaker B: There were deployment-related issues in staging.
[11:45-11:56] Speaker A: There is a preview problem; one app is weird.
[12:11-12:26] Speaker C: Resolve why the deployment failed and the production flag.`

// TestRunEndToEndWithCustomQuestionsAndQualityGate drives the real cobra `run`
// path (transcript loading, --evaluate behavior, --quality-gate, and eval
// report writing) with the deterministic local client. It uses two custom
// --question flags (fewer than the default set) so it also pins the
// MinInteractions fix: the gate must derive its minimum from the interactions
// actually run, not the hard-coded default count, and therefore not FAIL
// spuriously on the interaction-count check.
func TestRunEndToEndWithCustomQuestionsAndQualityGate(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "meeting.txt")
	require.NoError(t, os.WriteFile(transcript, []byte(cmdSampleTranscript), 0o644))
	reportPath := filepath.Join(dir, "report.json")

	Cmd.SetArgs([]string{
		"--note", transcript,
		"--agent", "local",
		"--question", "What is the root cause of the preview problem?",
		"--question", "What should we do about staging deployments?",
		"--quality-gate",
		"--eval-report", reportPath,
	})
	t.Cleanup(func() { Cmd.SetArgs(nil) })

	require.NoError(t, Cmd.ExecuteContext(context.Background()))

	data, err := os.ReadFile(reportPath)
	require.NoError(t, err)
	var report reportFile
	require.NoError(t, json.Unmarshal(data, &report))
	require.Len(t, report.Results, 1)
	// Two custom questions => exactly two interactions ran.
	require.Len(t, report.Results[0].Interactions, 2)
	// The gate must pass: with MinInteractions derived from the actual count,
	// running fewer than the default four questions is no longer a FAIL.
	require.True(t, report.Gate.Passed, report.Gate.Checks)
}
