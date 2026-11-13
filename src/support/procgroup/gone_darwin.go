package procgroup

import "syscall"

// gone also accepts EPERM: macOS answers it, not ESRCH, when the only members
// left are zombies.
func gone(err error) bool {
	return err == syscall.ESRCH || err == syscall.EPERM //nolint:errorlint // Kill returns a bare Errno
}
