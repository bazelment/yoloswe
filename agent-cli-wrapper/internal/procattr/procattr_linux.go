//go:build linux

// Package procattr provides platform-specific subprocess configuration
// for orphan prevention.
package procattr

import (
	"os/exec"
	"syscall"
)

// Set configures process group and parent-death signal for subprocess
// orphan prevention. On Linux, Pdeathsig causes the child to receive SIGTERM
// when the parent process dies (e.g. OOM kill, SIGKILL).
func Set(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGTERM,
	}
}
