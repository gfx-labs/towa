package dir

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"uuid"

	"github.com/gfx-labs/towa/src/store"
	"github.com/gfx-labs/towa/src/support/filelock"
)

// leaseName is the file in a session's directory whose lock is the lease.
// It holds the pid of the process that took it, for the error another
// process gets. The kernel drops the lock when that process ends however
// it ends, so a lease is never left behind.
const leaseName = "lease"

// lease is a held lease: closing the file drops the lock.
type lease struct{ f *os.File }

func (l lease) Close() error { return l.f.Close() }

// Lease implements store.Store.
func (s *Store) Lease(ctx context.Context, id uuid.UUID) (io.Closer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := s.takeLease(id)
	if err != nil {
		return nil, err
	}
	return lease{f}, nil
}

func (s *Store) takeLease(id uuid.UUID) (*os.File, error) {
	dir := s.SessionDir(id)
	if _, err := os.Lstat(filepath.Join(dir, metaName)); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", store.ErrNotFound, id)
	}
	f, err := os.OpenFile(filepath.Join(dir, leaseName), os.O_RDWR|os.O_CREATE, fileMode)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", store.ErrNotFound, id)
		}
		return nil, err
	}
	if err := filelock.TryLock(f, filelock.Exclusive); err != nil {
		if errors.Is(err, filelock.ErrHeld) {
			return nil, errors.Join(&store.LeasedError{ID: id, PID: leaseHolder(f)}, f.Close())
		}
		return nil, errors.Join(err, f.Close())
	}
	pid := []byte(strconv.Itoa(os.Getpid()) + "\n")
	if err := f.Truncate(0); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if _, err := f.WriteAt(pid, 0); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// leaseHolder reads the pid a lease's holder wrote, or zero.
func leaseHolder(f *os.File) int {
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		return 0
	}
	return pid
}
