//go:build linux

package shell

import "syscall"

// groupAttr starts the command as the leader of a new process group, so the
// command and everything it starts can be signalled together.
func groupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killGroup kills every process in the group that pid leads. A group with
// nobody left in it is not an error.
func killGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if err == syscall.ESRCH { //nolint:errorlint // Kill returns a bare Errno
		return nil
	}
	return err
}
