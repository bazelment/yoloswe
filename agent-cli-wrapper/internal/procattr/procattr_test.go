package procattr

import (
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSet_ConfiguresSysProcAttr(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("echo", "test")
	require.Nil(t, cmd.SysProcAttr)

	Set(cmd)

	require.NotNil(t, cmd.SysProcAttr)
	assert.True(t, cmd.SysProcAttr.Setpgid, "Setpgid should be true for process group creation")
}

func TestSet_Idempotent(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("echo", "test")

	Set(cmd)
	first := cmd.SysProcAttr

	Set(cmd)
	second := cmd.SysProcAttr

	// Both calls should configure the same fields.
	assert.Equal(t, first.Setpgid, second.Setpgid)
}

func TestSignalGroup_NilProcess(t *testing.T) {
	t.Parallel()
	err := SignalGroup(nil, syscall.SIGTERM)
	assert.NoError(t, err, "SignalGroup with nil process should be a no-op")
}

func TestKillGroup_NilProcess(t *testing.T) {
	t.Parallel()
	err := KillGroup(nil)
	assert.NoError(t, err, "KillGroup with nil process should be a no-op")
}

func TestSignalGroup_RunningProcess(t *testing.T) {
	t.Parallel()

	// Start a sleep process with a process group.
	cmd := exec.Command("sleep", "60")
	Set(cmd)
	require.NoError(t, cmd.Start())

	// Signal the process group with SIGTERM.
	err := SignalGroup(cmd.Process, syscall.SIGTERM)
	assert.NoError(t, err)

	// Wait for process to exit (should be killed by SIGTERM).
	_ = cmd.Wait()
}

func TestKillGroup_RunningProcess(t *testing.T) {
	t.Parallel()

	// Start a sleep process with a process group.
	cmd := exec.Command("sleep", "60")
	Set(cmd)
	require.NoError(t, cmd.Start())

	// Kill the process group.
	err := KillGroup(cmd.Process)
	assert.NoError(t, err)

	// Wait for process to exit (should be killed by SIGKILL).
	_ = cmd.Wait()
}
