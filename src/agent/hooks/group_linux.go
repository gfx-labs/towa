//go:build linux

package hooks

import "syscall"

// killGroup kills every process in the group that pid leads. A group with
// nobody left in it is not an error.
func killGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if err == syscall.ESRCH { //nolint:errorlint // Kill returns a bare Errno
		return nil
	}
	return err
}
