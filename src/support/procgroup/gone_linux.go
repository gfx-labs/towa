package procgroup

import "syscall"

func gone(err error) bool {
	return err == syscall.ESRCH //nolint:errorlint // Kill returns a bare Errno
}
