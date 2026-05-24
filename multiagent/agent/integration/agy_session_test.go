//go:build integration
// +build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/agy"
	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newAgyIntegrationProvider(t *testing.T) *agent.AgyProvider {
	t.Helper()
	return agent.NewAgyProvider(
		agy.WithPrintTimeout(2*time.Minute),
		agy.WithStderrHandler(func(data []byte) {
			t.Logf("[agy stderr] %s", string(data))
		}),
	)
}

func TestAgy_BasicPrompt(t *testing.T) {
	skipIfBinaryMissing(t, "agy")
	provider := newAgyIntegrationProvider(t)
	defer provider.Close()

	result, err := provider.Execute(t.Context(), "Reply with exactly: HELLO_FROM_AGY", nil)
	require.NoError(t, err)
	require.True(t, result.Success)
	assert.Contains(t, strings.ToUpper(result.Text), "HELLO_FROM_AGY")
}

func TestAgy_PlannerReadOnly(t *testing.T) {
	skipIfBinaryMissing(t, "agy")
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "should_not_exist.txt")
	provider := newAgyIntegrationProvider(t)
	defer provider.Close()

	result, err := provider.Execute(t.Context(),
		"Try to create should_not_exist.txt containing blocked, then report whether you were allowed.",
		nil,
		agent.WithProviderWorkDir(tmpDir),
		agent.WithProviderPermissionMode("plan"),
	)
	require.NoError(t, err)
	require.True(t, result.Success)
	_, statErr := os.Stat(target)
	assert.True(t, os.IsNotExist(statErr), "plan mode should not create files")
}

func TestAgy_FileWrite(t *testing.T) {
	skipIfBinaryMissing(t, "agy")
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "agy_test_output.txt")
	provider := newAgyIntegrationProvider(t)
	defer provider.Close()

	result, err := provider.Execute(t.Context(),
		"Create the file "+target+" containing exactly AGY_FILE_WRITE_OK. Do not include any other text in the file.",
		nil,
		agent.WithProviderWorkDir(tmpDir),
		agent.WithProviderPermissionMode("bypass"),
	)
	require.NoError(t, err)
	require.True(t, result.Success)
	t.Logf("agy response: %s", result.Text)
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "AGY_FILE_WRITE_OK", strings.TrimSpace(string(data)))
}
