// Package procgroup starts a command as the leader of its own process group
// and signals the whole group, so that a command and everything it started
// can be stopped together.
package procgroup

import (
	"os/exec"
	"syscall"
)

// Set makes cmd the leader of a new process group when it starts.
func Set(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// Kill kills every process in the group that pid leads.
func Kill(pid int) error { return Signal(pid, syscall.SIGKILL) }

// Terminate asks every process in the group that pid leads to end.
func Terminate(pid int) error { return Signal(pid, syscall.SIGTERM) }

// Signal sends sig to every process in the group that pid leads. A group
// with nobody left in it is not an error.
func Signal(pid int, sig syscall.Signal) error {
	err := syscall.Kill(-pid, sig)
	if gone(err) {
		return nil
	}
	return err
}
