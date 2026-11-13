// Package filelock takes advisory locks on open files. Each platform
// supplies the locking in its own file. A platform with no file locks
// returns errors.ErrUnsupported, so a caller that needs a lock fails rather
// than runs without one.
package filelock

import "errors"

// Mode is the kind of lock.
type Mode int

const (
	// Shared may be held by several open files at once.
	Shared Mode = iota
	// Exclusive is held by one open file, and no shared lock with it.
	Exclusive
)

// ErrHeld reports that TryLock found the lock held by another open file.
var ErrHeld = errors.New("filelock: lock is held")
