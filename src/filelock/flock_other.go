//go:build !linux && !darwin

package filelock

import (
	"errors"
	"os"
)

// Lock is not supported on this platform.
func Lock(*os.File, Mode) error { return errors.ErrUnsupported }

// TryLock is not supported on this platform.
func TryLock(*os.File, Mode) error { return errors.ErrUnsupported }
