// Package claude adapts capabilities to Claude Code's tool behavior.
package claude

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// ReadTracker belongs to one session and is reused across its turns. Its zero
// value is ready to use. Sharing it between sessions would share edit authority.
// Generic uses this same boundary. Callbacks execute under the session lock.
type ReadTracker struct {
	mu    sync.Mutex
	files map[string]fingerprint
}

type fingerprint struct {
	digest [sha256.Size]byte
	info   os.FileInfo
}

// UnreadError refuses editing a file this session has not successfully read.
type UnreadError struct{ Path string }

func (e *UnreadError) Error() string {
	return fmt.Sprintf("claude: file %q has not been read yet", e.Path)
}

// ChangedError refuses editing a file changed since this session read it.
type ChangedError struct{ Path string }

func (e *ChangedError) Error() string {
	return fmt.Sprintf("claude: file %q changed since it was read", e.Path)
}

func snapshot(ctx context.Context, path string) (fingerprint, error) {
	if err := ctx.Err(); err != nil {
		return fingerprint{}, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fingerprint{}, err
	}
	defer f.Close() //nolint:errcheck // read only
	before, err := f.Stat()
	if err != nil {
		return fingerprint{}, err
	}
	if !before.Mode().IsRegular() {
		return fingerprint{}, fmt.Errorf("claude: %q is not a regular file", path)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fingerprint{}, err
	}
	after, err := f.Stat()
	if err != nil {
		return fingerprint{}, err
	}
	current, err := os.Stat(path)
	if err != nil {
		return fingerprint{}, err
	}
	if !os.SameFile(before, after) || !os.SameFile(after, current) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || !changeTime(before).Equal(changeTime(after)) {
		return fingerprint{}, &ChangedError{Path: path}
	}
	if err := ctx.Err(); err != nil {
		return fingerprint{}, err
	}
	var digest [sha256.Size]byte
	copy(digest[:], h.Sum(nil))
	return fingerprint{digest: digest, info: after}, nil
}

func same(a, b fingerprint) bool {
	return a.digest == b.digest && os.SameFile(a.info, b.info) && a.info.ModTime().Equal(b.info.ModTime()) && changeTime(a.info).Equal(changeTime(b.info))
}

// Read runs the capability and records a full-file fingerprint only after a
// successful, stable read, even when the capability returned just a line range.
// path must be the resolved absolute path used by the capability.
func (t *ReadTracker) Read(ctx context.Context, path string, read func() error) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	path = filepath.Clean(path)
	before, err := snapshot(ctx, path)
	if err != nil {
		return err
	}
	if err = read(); err != nil {
		return err
	}
	after, err := snapshot(ctx, path)
	if err != nil {
		delete(t.files, path)
		return err
	}
	if !same(before, after) {
		delete(t.files, path)
		return &ChangedError{Path: path}
	}
	t.remember(path, after)
	return nil
}

// Edit checks the session's fingerprint before invoking the capability, then
// remembers the successful edit's new content. A refusal never invokes edit.
// Like upstream this is a pre-write guard, not filesystem-wide atomic locking:
// an unrelated process can still change a file while the capability runs.
func (t *ReadTracker) Edit(ctx context.Context, path string, edit func() error) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	path = filepath.Clean(path)
	old, ok := t.files[path]
	if !ok {
		return &UnreadError{Path: path}
	}
	current, err := snapshot(ctx, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			delete(t.files, path)
			return &ChangedError{Path: path}
		}
		return err
	}
	if !same(old, current) {
		delete(t.files, path)
		return &ChangedError{Path: path}
	}
	return t.write(ctx, path, edit)
}

// Write remembers a successful whole-file write, as upstream updates
// readFileState after writing. The adapter checks overwrite permissions first.
func (t *ReadTracker) Write(ctx context.Context, path string, write func() error) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.write(ctx, filepath.Clean(path), write)
}

func (t *ReadTracker) write(ctx context.Context, path string, write func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := write(); err != nil {
		delete(t.files, path)
		return err
	}
	next, err := snapshot(ctx, path)
	if err != nil {
		delete(t.files, path)
		return err
	}
	t.remember(path, next)
	return nil
}

func (t *ReadTracker) remember(path string, f fingerprint) {
	if t.files == nil {
		t.files = make(map[string]fingerprint)
	}
	t.files[path] = f
}
