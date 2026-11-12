// Package dir is the store backend that keeps each session in one
// directory:
//
//	<root>/<session id>/meta.json       the session's Meta
//	<root>/<session id>/log.jsonl       every entry, one JSON value per line
//	<root>/<session id>/log.jsonl.zst   or the same lines, one zstd frame per Append
//	<root>/<session id>/blobs/<sha256>  content stored by PutBlob
//
// Nothing outside that directory belongs to the session, so copying it
// under another root moves the session. The store keeps no state in memory:
// every call opens what it needs, and a lock on the log (flock) orders the
// calls of every process that uses the root.
package dir

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"uuid"

	"github.com/gfx-labs/acpharness/src/filelock"
	"github.com/gfx-labs/acpharness/src/store"
)

// File names in a session's directory.
const (
	metaName  = "meta.json"
	logName   = "log.jsonl"
	blobsName = "blobs"

	// The names a session's directory has while it is built or removed.
	creatingSuffix = ".creating"
	deletingSuffix = ".deleting"
)

// Sessions hold conversations, so only the owner can read them.
const (
	dirMode  = 0o700
	fileMode = 0o600
)

// CorruptError reports a session directory that does not hold what this
// package wrote.
type CorruptError struct {
	Path string
	// Offset is the position in the file, when the fault has one.
	Offset int64
	Err    error
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("store/dir: %s is corrupt at offset %d: %v", e.Path, e.Offset, e.Err)
}

func (e *CorruptError) Unwrap() error { return e.Err }

// Options configures a Store.
type Options struct {
	// Root is the directory that holds one directory per session. It must
	// be an absolute path. It is created when the first session is.
	Root string
	// Compression is how the log of a session made by Create is stored. A
	// session that exists is read and appended to in the format it has.
	Compression Compression
}

// Compression names a log format.
type Compression string

// Log formats.
const (
	// CompressionNone is one JSON value per line in log.jsonl.
	CompressionNone Compression = "none"
	// CompressionZstd is one zstd frame per Append in log.jsonl.zst.
	CompressionZstd Compression = "zstd"
)

// Stats counts what a Store did since it was made.
type Stats struct {
	// Syncs is the number of fsync calls, on files and on directories.
	Syncs int64
}

// Store keeps sessions under a root directory. It is safe for concurrent
// use, and several processes may use one root.
type Store struct {
	root string
	// formats are the log formats a session may be in, and create1 is the
	// one a new session gets.
	formats []logFormat
	create1 logFormat
	codec   codec

	syncs atomic.Int64
}

var _ store.Store = (*Store)(nil)

// New returns a store over the sessions under opts.Root.
func New(opts Options) (*Store, error) {
	if !filepath.IsAbs(opts.Root) {
		return nil, fmt.Errorf("store/dir: root %q is not an absolute path", opts.Root)
	}
	s := &Store{root: filepath.Clean(opts.Root)}
	plain, compressed := plainFormat{}, zstdFormat{codec: &s.codec}
	s.formats = []logFormat{plain, compressed}
	switch opts.Compression {
	case CompressionNone:
		s.create1 = plain
	case CompressionZstd:
		s.create1 = compressed
	default:
		return nil, fmt.Errorf("store/dir: compression %q is not %q or %q", opts.Compression, CompressionNone, CompressionZstd)
	}
	return s, nil
}

// Stats returns the counters.
func (s *Store) Stats() Stats {
	return Stats{Syncs: s.syncs.Load()}
}

// SessionDir returns the directory that holds everything of a session.
func (s *Store) SessionDir(id uuid.UUID) string {
	return filepath.Join(s.root, id.String())
}

// Create adds a session. Creating an id the store already holds is an error
// that wraps fs.ErrExist.
func (s *Store) Create(ctx context.Context, meta store.Meta) error {
	return s.build(ctx, meta, func(building string) error {
		log, err := os.OpenFile(logPath(building, s.create1), os.O_CREATE|os.O_EXCL|os.O_WRONLY, fileMode)
		if err != nil {
			return err
		}
		return log.Close()
	})
}

// build makes a session's directory under a hidden name, has fill put the
// log in it, and renames it into place, so that a crash leaves a whole
// session or none.
func (s *Store) build(ctx context.Context, meta store.Meta, fill func(building string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if meta.ID == uuid.Nil() {
		return errors.New("store/dir: the session has no id")
	}
	if !filepath.IsAbs(meta.Cwd) {
		return fmt.Errorf("store/dir: session %s: cwd %q is not an absolute path", meta.ID, meta.Cwd)
	}
	if err := s.mkdir(s.root); err != nil {
		return err
	}
	root, err := s.lockRoot(filelock.Exclusive)
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // Closing drops the directory lock.
	final := s.SessionDir(meta.ID)
	if _, err := os.Lstat(final); err == nil {
		return fmt.Errorf("store/dir: session %s: %w", meta.ID, fs.ErrExist)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// What a crashed Create of the same id left behind was never reported
	// as created.
	building := filepath.Join(s.root, "."+meta.ID.String()+creatingSuffix)
	if err := os.RemoveAll(building); err != nil {
		return err
	}
	if err := os.Mkdir(building, dirMode); err != nil {
		return err
	}
	// The lock tells Sweep that the directory is in use.
	held, err := lockDir(building)
	if err != nil {
		return errors.Join(err, os.RemoveAll(building))
	}
	defer held.Close() //nolint:errcheck // Nothing is written through it: closing drops the lock.
	err = func() error {
		data, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		if err := s.writeFile(filepath.Join(building, metaName), data); err != nil {
			return err
		}
		if err := fill(building); err != nil {
			return err
		}
		if err := s.syncDir(building); err != nil {
			return err
		}
		if err := os.Rename(building, final); err != nil {
			return err
		}
		return s.syncDir(s.root)
	}()
	if err != nil {
		return errors.Join(err, os.RemoveAll(building))
	}
	return nil
}

// lockDir takes an exclusive lock on a directory without waiting.
func lockDir(path string) (*os.File, error) {
	d, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if err := filelock.TryLock(d, filelock.Exclusive); err != nil {
		return nil, errors.Join(err, d.Close())
	}
	return d, nil
}

// Sweep removes what a crash during Create, Fork or Delete left under the
// root: a directory that never became a session, or one that had stopped
// being one. It returns how many it removed. A directory that a running
// call holds is left alone. Nothing calls Sweep on its own: New does no
// I/O, so the program runs it when it starts.
func (s *Store) Sweep(ctx context.Context) (int, error) {
	root, err := s.lockRoot(filelock.Exclusive)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer root.Close() //nolint:errcheck // Closing drops the directory lock.
	names, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if !leftover(name.Name()) {
			continue
		}
		path := filepath.Join(s.root, name.Name())
		held, err := lockDir(path)
		if errors.Is(err, filelock.ErrHeld) || errors.Is(err, fs.ErrNotExist) {
			// In use, or finished since the root was read.
			continue
		}
		if err != nil {
			return removed, err
		}
		err = os.RemoveAll(path)
		if err = errors.Join(err, held.Close()); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// leftover reports whether name is one build or Delete gives a directory
// while it works on it.
func leftover(name string) bool {
	rest, hidden := strings.CutPrefix(name, ".")
	if !hidden {
		return false
	}
	for _, suffix := range []string{creatingSuffix, deletingSuffix} {
		if id, ok := strings.CutSuffix(rest, suffix); ok {
			parsed, err := uuid.Parse(id)
			return err == nil && parsed.String() == id
		}
	}
	return false
}

// SetMeta replaces the Title, Model, Mode, Dirs, Archived and State of a
// session.
func (s *Store) SetMeta(ctx context.Context, meta store.Meta) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	log, _, err := s.openLog(meta.ID, os.O_RDONLY, filelock.Exclusive)
	if err != nil {
		return err
	}
	defer log.Close() //nolint:errcheck // Read only: closing drops the lock and cannot lose data.
	stored, err := s.readMeta(meta.ID)
	if err != nil {
		return err
	}
	stored.Title, stored.Model, stored.Mode, stored.Dirs = meta.Title, meta.Model, meta.Mode, meta.Dirs
	stored.Archived, stored.State = meta.Archived, meta.State
	data, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	return s.replaceFile(s.SessionDir(meta.ID), metaName, data)
}

// Delete removes a session and everything in its directory.
func (s *Store) Delete(ctx context.Context, id uuid.UUID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := s.lockRoot(filelock.Exclusive)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s", store.ErrNotFound, id)
	}
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // Closing drops the directory lock.
	// The lock is held until the files are gone, so that a call waiting
	// for it finds a log with no name and reports the session as not found.
	log, _, err := s.openLog(id, os.O_RDONLY, filelock.Exclusive)
	if err != nil {
		return err
	}
	defer log.Close() //nolint:errcheck // Read only: closing drops the lock and cannot lose data.
	// Renamed first, so that a crash cannot leave half a session under its
	// id.
	doomed := filepath.Join(s.root, "."+id.String()+deletingSuffix)
	if err := os.RemoveAll(doomed); err != nil {
		return err
	}
	// Locked before it has its hidden name, so that Sweep never finds it
	// without its lock.
	held, err := lockDir(s.SessionDir(id))
	if err != nil {
		return err
	}
	defer held.Close() //nolint:errcheck // Nothing is written through it: closing drops the lock.
	if err := os.Rename(s.SessionDir(id), doomed); err != nil {
		return err
	}
	if err := s.syncDir(s.root); err != nil {
		return err
	}
	return os.RemoveAll(doomed)
}

// readMeta reads meta.json. The caller holds the session's lock.
func (s *Store) readMeta(id uuid.UUID) (store.Meta, error) {
	path := filepath.Join(s.SessionDir(id), metaName)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return store.Meta{}, &CorruptError{Path: path, Err: err}
		}
		return store.Meta{}, err
	}
	var meta store.Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		return store.Meta{}, &CorruptError{Path: path, Err: err}
	}
	if meta.ID != id {
		return store.Meta{}, &CorruptError{Path: path, Err: fmt.Errorf("it describes session %s", meta.ID)}
	}
	return meta, nil
}

// openLog opens a session's log, whichever format it is in, and takes a
// lock on it, which closing the file drops. The log is the lock for the
// whole session directory.
func (s *Store) openLog(id uuid.UUID, flag int, lock filelock.Mode) (*os.File, logFormat, error) {
	dir := s.SessionDir(id)
	var f *os.File
	var format logFormat
	for _, candidate := range s.formats {
		opened, err := os.OpenFile(logPath(dir, candidate), flag, 0)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, closeAfter(err, f)
		}
		if f != nil {
			err := &CorruptError{Path: dir, Err: errors.New("the session has a log in two formats")}
			return nil, nil, closeAfter(closeAfter(err, f), opened)
		}
		f, format = opened, candidate
	}
	if f == nil {
		if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
			return nil, nil, fmt.Errorf("%w: %s", store.ErrNotFound, id)
		}
		return nil, nil, &CorruptError{Path: dir, Err: errors.New("the session has no log")}
	}
	if err := filelock.Lock(f, lock); err != nil {
		return nil, nil, closeAfter(fmt.Errorf("store/dir: lock %s: %w", f.Name(), err), f)
	}
	// Delete holds the lock until the files are gone, so a log with no name
	// left belongs to a session that was deleted while this call waited.
	info, err := f.Stat()
	if err != nil {
		return nil, nil, closeAfter(err, f)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink == 0 {
		return nil, nil, closeAfter(fmt.Errorf("%w: %s", store.ErrNotFound, id), f)
	}
	return f, format, nil
}

// closeAfter closes f, which may be nil, on the way out with err.
func closeAfter(err error, f *os.File) error {
	if f == nil {
		return err
	}
	return errors.Join(err, f.Close())
}

// writeFile creates a file with data and makes its content durable. The
// caller makes the name durable with syncDir.
func (s *Store) writeFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fileMode)
	if err != nil {
		return err
	}
	if err := s.writeSync(f, data); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

func (s *Store) writeSync(f *os.File, data []byte) error {
	_, err := f.Write(data)
	if err != nil {
		return err
	}
	s.syncs.Add(1)
	return f.Sync()
}

// replaceFile puts data under name in dir, so that a reader or a crash sees
// the old content or the new, never a part.
func (s *Store) replaceFile(dir, name string, data []byte) error {
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := s.writeFile(tmp, data); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	return s.syncDir(dir)
}

// syncDir makes the names in a directory durable.
func (s *Store) syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	s.syncs.Add(1)
	if err := d.Sync(); err != nil {
		return errors.Join(err, d.Close())
	}
	return d.Close()
}

// mkdir makes every missing directory in path and syncs its parent's new
// name. MkdirAll alone would leave a newly made store root nondurable even
// after Create had synced the session directory below it.
func (s *Store) mkdir(path string) error {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("store/dir: %s is not a directory", path)
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(path)
	if err := s.mkdir(parent); err != nil {
		return err
	}
	if err := os.Mkdir(path, dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return s.syncDir(parent)
}
