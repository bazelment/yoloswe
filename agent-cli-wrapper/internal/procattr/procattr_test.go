package procattr

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

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

	cmd := startSignalHelper(t)

	// Signal the process group with SIGTERM.
	err := SignalGroup(cmd.Process, syscall.SIGTERM)
	assert.NoError(t, err)

	// Wait for process to exit (should be killed by SIGTERM).
	waitForHelperExit(t, cmd)
}

func TestKillGroup_RunningProcess(t *testing.T) {
	t.Parallel()

	cmd := startSignalHelper(t)

	// Kill the process group.
	err := KillGroup(cmd.Process)
	assert.NoError(t, err)

	// Wait for process to exit (should be killed by SIGKILL).
	waitForHelperExit(t, cmd)
}

func startSignalHelper(t *testing.T) *exec.Cmd {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=TestSignalHelperProcess")
	cmd.Env = append(os.Environ(), "PROCATTR_SIGNAL_HELPER=1")
	Set(cmd)
	require.NoError(t, cmd.Start())
	return cmd
}

func waitForHelperExit(t *testing.T, cmd *exec.Cmd) {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = KillGroup(cmd.Process)
		t.Fatal("helper process did not exit after process-group signal")
	}
}

func TestSignalHelperProcess(t *testing.T) {
	if os.Getenv("PROCATTR_SIGNAL_HELPER") != "1" {
		return
	}
	select {}
}
