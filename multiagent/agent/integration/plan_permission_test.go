//go:build integration
// +build integration

// Manual integration tests for PlanOnlyPermissionHandler with real Gemini CLI.
// These tests verify that the permission handler correctly allows read operations
// and rejects write operations in a real ACP session.
//
// Run manually with:
//   bazel build //multiagent/agent/integration:integration_test
//   ./bazel-bin/multiagent/agent/integration/integration_test_/integration_test -test.v -test.run TestPlanOnlyPermissionHandler
//
// Requirements:
// - Gemini CLI installed and in PATH
// - Valid API key configured
// - Trusted folder setup complete

package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/acp"
	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingPlanOnlyHandler wraps PlanOnlyPermissionHandler and records requests.
type recordingPlanOnlyHandler struct {
	planOnly *acp.PlanOnlyPermissionHandler
	mu       sync.Mutex
	allowed  []string // tool names that were allowed
	rejected []string // tool names that were rejected
}

func (h *recordingPlanOnlyHandler) RequestPermission(ctx context.Context, req acp.RequestPermissionRequest) (*acp.RequestPermissionResponse, error) {
	resp, err := h.planOnly.RequestPermission(ctx, req)
	if err != nil {
		return resp, err
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	toolName := req.ToolCall.ToolName

	// Determine if this was allowed or rejected based on the selected option
	if resp.Outcome.Type == "selected" {
		for _, opt := range req.Options {
			if opt.ID == resp.Outcome.OptionID {
				if strings.HasPrefix(opt.Kind, "allow") {
					h.allowed = append(h.allowed, toolName)
				} else if strings.HasPrefix(opt.Kind, "reject") {
					h.rejected = append(h.rejected, toolName)
				}
				break
			}
		}
	} else if resp.Outcome.Type == "cancelled" {
		h.rejected = append(h.rejected, toolName)
	}

	return resp, nil
}

func (h *recordingPlanOnlyHandler) getAllowed() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.allowed...)
}

func (h *recordingPlanOnlyHandler) getRejected() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.rejected...)
}

func TestPlanOnlyPermissionHandler_ReadOperationsAllowed(t *testing.T) {
	if !isBinaryAvailable("gemini") {
		t.Skip("gemini binary not available")
	}

	// Create temp directory for test
	tmpDir, err := os.MkdirTemp("", "plan-perm-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Create a test file to read
	testFile := filepath.Join(tmpDir, "test.txt")
	err = os.WriteFile(testFile, []byte("Hello, this is a test file."), 0644)
	require.NoError(t, err)

	// Create provider with recording PlanOnlyPermissionHandler
	recorder := &recordingPlanOnlyHandler{
		planOnly: &acp.PlanOnlyPermissionHandler{},
	}

	provider := agent.NewGeminiProvider(
		acp.WithBinaryArgs("--experimental-acp", "--model", "gemini-2.5-flash"),
		acp.WithPermissionHandler(recorder),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Ask Gemini to read the file
	prompt := "Please read the file at " + testFile + " and tell me what it contains. Just read it, don't modify anything."

	result, err := provider.Execute(ctx, prompt, nil)
	require.NoError(t, err, "Execute should succeed")
	require.NotNil(t, result)

	err = provider.Close()
	require.NoError(t, err)

	// Verify that read operations were allowed
	allowed := recorder.getAllowed()
	rejected := recorder.getRejected()

	t.Logf("Allowed tools: %v", allowed)
	t.Logf("Rejected tools: %v", rejected)

	// We expect at least one read operation to have been allowed
	hasReadOperation := false
	readTools := []string{"read_file", "read_text_file"}
	for _, toolName := range allowed {
		for _, readTool := range readTools {
			if strings.Contains(strings.ToLower(toolName), readTool) {
				hasReadOperation = true
				break
			}
		}
	}

	assert.True(t, hasReadOperation, "at least one read operation should have been allowed")

	// The response should contain the file content
	assert.Contains(t, result.Text, "test file", "response should mention the test file content")
}

func TestPlanOnlyPermissionHandler_WriteOperationsRejected(t *testing.T) {
	if !isBinaryAvailable("gemini") {
		t.Skip("gemini binary not available")
	}

	// Create temp directory for test
	tmpDir, err := os.MkdirTemp("", "plan-perm-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Create provider with recording PlanOnlyPermissionHandler
	recorder := &recordingPlanOnlyHandler{
		planOnly: &acp.PlanOnlyPermissionHandler{},
	}

	provider := agent.NewGeminiProvider(
		acp.WithBinaryArgs("--experimental-acp", "--model", "gemini-2.5-flash"),
		acp.WithPermissionHandler(recorder),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Ask Gemini to write a file - this should be rejected
	testFile := filepath.Join(tmpDir, "output.txt")
	prompt := "Please write the text 'Hello World' to the file at " + testFile

	result, err := provider.Execute(ctx, prompt, nil)
	// The execute might succeed but the tool call should be rejected
	require.NoError(t, err, "Execute should not error out")
	require.NotNil(t, result)

	err = provider.Close()
	require.NoError(t, err)

	// Verify that write operations were rejected
	rejected := recorder.getRejected()

	t.Logf("Rejected tools: %v", rejected)

	// We expect write operations to have been rejected
	hasWriteRejection := false
	writeTools := []string{"write_file", "write_text_file"}
	for _, toolName := range rejected {
		for _, writeTool := range writeTools {
			if strings.Contains(strings.ToLower(toolName), writeTool) {
				hasWriteRejection = true
				break
			}
		}
	}

	assert.True(t, hasWriteRejection, "write operations should have been rejected")

	// Verify the file was NOT created
	_, err = os.Stat(testFile)
	assert.True(t, os.IsNotExist(err), "file should not have been created")
}

func TestPlanOnlyPermissionHandler_LongRunningSession(t *testing.T) {
	if !isBinaryAvailable("gemini") {
		t.Skip("gemini binary not available")
	}

	// Create temp directory for test
	tmpDir, err := os.MkdirTemp("", "plan-perm-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Create a test file to read
	testFile := filepath.Join(tmpDir, "test.txt")
	err = os.WriteFile(testFile, []byte("Original content"), 0644)
	require.NoError(t, err)

	// Create long-running provider with PlanOnlyPermissionHandler
	recorder := &recordingPlanOnlyHandler{
		planOnly: &acp.PlanOnlyPermissionHandler{},
	}

	provider := agent.NewGeminiLongRunningProvider(
		[]acp.ClientOption{
			acp.WithBinaryArgs("--experimental-acp", "--model", "gemini-2.5-flash"),
			acp.WithPermissionHandler(recorder),
		},
		acp.WithSessionCWD(tmpDir),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Start the long-running session
	err = provider.Start(ctx)
	require.NoError(t, err, "Start should succeed")

	// Turn 1: Read the file (should be allowed)
	result1, err := provider.SendMessage(ctx, "Read the file test.txt and tell me what it says")
	require.NoError(t, err, "Turn 1 should succeed")
	require.NotNil(t, result1)

	// Turn 2: Try to write to the file (should be rejected)
	outputFile := filepath.Join(tmpDir, "output.txt")
	result2, err := provider.SendMessage(ctx, "Write 'Hello World' to output.txt")
	require.NoError(t, err, "Turn 2 should not error out")
	require.NotNil(t, result2)

	// Turn 3: Try to list directory (should be allowed)
	result3, err := provider.SendMessage(ctx, "List the files in the current directory")
	require.NoError(t, err, "Turn 3 should succeed")
	require.NotNil(t, result3)

	err = provider.Stop()
	require.NoError(t, err)

	err = provider.Close()
	require.NoError(t, err)

	// Verify permissions were enforced across all turns
	allowed := recorder.getAllowed()
	rejected := recorder.getRejected()

	t.Logf("Allowed tools: %v", allowed)
	t.Logf("Rejected tools: %v", rejected)

	// Should have allowed read operations
	assert.NotEmpty(t, allowed, "some read operations should have been allowed")

	// Should have rejected write operations
	assert.NotEmpty(t, rejected, "write operations should have been rejected")

	// Verify the output file was NOT created
	_, err = os.Stat(outputFile)
	assert.True(t, os.IsNotExist(err), "output file should not have been created")
}

// isBinaryAvailable checks if a binary is available in PATH.
func isBinaryAvailable(name string) bool {
	_, err := os.LookupPath(name)
	return err == nil
}
