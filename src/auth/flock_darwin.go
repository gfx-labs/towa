package auth

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// flockExclusive blocks until f's open file holds the exclusive lock.
func flockExclusive(f *os.File) error {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
