//go:build darwin

package hooks

import "syscall"

// groupAttr starts the command as the leader of a new process group, so the
// command and everything it starts can be killed together.
func groupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killGroup kills every process in the group that pid leads. A group with
// nobody left in it is not an error. macOS answers EPERM, not ESRCH, when
// the only members left are zombies.
func killGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if err == syscall.ESRCH || err == syscall.EPERM { //nolint:errorlint // Kill returns a bare Errno
		return nil
	}
	return err
}
