//go:build linux || darwin

package filelock

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Lock blocks until f holds the lock. Closing the last descriptor of the
// open file releases it.
func Lock(f *os.File, mode Mode) error {
	how, err := flockMode(mode)
	if err != nil {
		return err
	}
	return flock(f, how)
}

// TryLock takes the lock without waiting. It returns an error wrapping
// ErrHeld when another open file holds it.
func TryLock(f *os.File, mode Mode) error {
	how, err := flockMode(mode)
	if err != nil {
		return err
	}
	err = flock(f, how|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return fmt.Errorf("%w: %s", ErrHeld, f.Name())
	}
	return err
}

func flockMode(mode Mode) (int, error) {
	switch mode {
	case Shared:
		return unix.LOCK_SH, nil
	case Exclusive:
		return unix.LOCK_EX, nil
	default:
		return 0, fmt.Errorf("filelock: mode %d is not valid", mode)
	}
}

func flock(f *os.File, how int) error {
	for {
		err := unix.Flock(int(f.Fd()), how)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
