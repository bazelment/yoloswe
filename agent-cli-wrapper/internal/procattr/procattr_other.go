//go:build !linux

// Package procattr provides platform-specific subprocess configuration
// for orphan prevention.
package procattr

import (
	"os/exec"
	"syscall"
)

// Set configures process group for subprocess orphan prevention.
// On non-Linux platforms, Pdeathsig is not available. Setpgid creates a
// process group, enabling kill -<signal> -<pgid> cleanup by the parent.
func Set(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}
