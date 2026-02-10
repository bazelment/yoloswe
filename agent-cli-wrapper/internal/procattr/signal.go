package procattr

import (
	"os"
	"syscall"
)

// SignalGroup sends a signal to the entire process group of the given process.
// Using the negative PID causes the kernel to deliver the signal to all
// processes in the group, not just the direct child.
func SignalGroup(p *os.Process, sig syscall.Signal) error {
	if p == nil {
		return nil
	}
	return syscall.Kill(-p.Pid, sig)
}

// KillGroup sends SIGKILL to the entire process group of the given process.
func KillGroup(p *os.Process) error {
	return SignalGroup(p, syscall.SIGKILL)
}
