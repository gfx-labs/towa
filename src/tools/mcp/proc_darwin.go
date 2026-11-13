package mcp

import (
	"os/exec"
	"syscall"
)

// setProcessGroup makes the command the leader of a new process group, so
// that a signal to the group reaches every process the server starts.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup sends sig to every process in the group that pid leads.
func signalGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}
