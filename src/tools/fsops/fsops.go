// Package fsops reads, writes and edits files inside a session's
// directories. Each function is one capability with typed input and a typed
// result. A dialect adapter turns a result or an *Error into the upstream
// tool's words.
package fsops

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/tools"
)

// Kind says why an operation was refused or failed in a way the model can
// act on.
type Kind string

// Kinds of an Error.
const (
	// KindOutside: confinement is on and the path is not in the working
	// directory or an additional directory, once symbolic links are
	// followed.
	KindOutside Kind = "outside"
	// KindNotFound: the file does not exist.
	KindNotFound Kind = "not_found"
	// KindPermission: the operating system refused access.
	KindPermission Kind = "permission"
	// KindIsDirectory: the path names a directory.
	KindIsDirectory Kind = "is_directory"
	// KindNotRegular: the path names a device, a pipe or a socket.
	KindNotRegular Kind = "not_regular"
	// KindBinary: the file is not text. MimeType holds what it looks like.
	KindBinary Kind = "binary"
	// KindTooLarge: Size is over Limit, both in bytes.
	KindTooLarge Kind = "too_large"
	// KindInvalidRange: the offset or limit of a read is negative.
	KindInvalidRange Kind = "invalid_range"
	// KindPastEnd: a read starts after the last line. Lines holds how many
	// lines the file has.
	KindPastEnd Kind = "past_end"
	// KindNoEdits: an edit call holds no edits.
	KindNoEdits Kind = "no_edits"
	// KindEmptyOld: the text to replace is empty. Edit holds its index.
	KindEmptyOld Kind = "empty_old"
	// KindSameText: the text to replace and its replacement are equal.
	KindSameText Kind = "same_text"
	// KindNoMatch: the text to replace is not in the file.
	KindNoMatch Kind = "no_match"
	// KindNotUnique: the text to replace is in the file Count times and the
	// edit does not replace every occurrence.
	KindNotUnique Kind = "not_unique"
	// KindNoChange: the edits leave the file as it was.
	KindNoChange Kind = "no_change"
)

// Error is a failure the model caused or can act on. Any other error from
// this package means the harness could not do the operation.
type Error struct {
	Kind Kind
	// Path is the path as the caller gave it.
	Path string
	// Edit is the index of the edit that failed.
	Edit int
	// Count is how many times the text of a KindNotUnique edit occurs.
	Count int
	// Lines is the file's line count, for KindPastEnd.
	Lines int
	// Size and Limit are in bytes, for KindTooLarge.
	Size  int64
	Limit int64
	// MimeType is what a KindBinary file looks like.
	MimeType string
	Err      error
}

func (e *Error) Error() string {
	s := fmt.Sprintf("fsops: %s: %s", e.Kind, e.Path)
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// Roots are a session's directories.
type Roots struct {
	// Cwd is the working directory. A relative path is taken from it.
	Cwd string
	// Dirs are the additional directories.
	Dirs []string
	// Confine refuses a path outside Cwd and Dirs. Without it any path is
	// accepted: isolation is the job of whatever runs the harness.
	Confine bool
}

// RootsOf returns the directories of the session a call runs in.
func RootsOf(env tools.Env, cfg config.ToolsFiles) Roots {
	return Roots{Cwd: env.Cwd, Dirs: env.Dirs, Confine: cfg.Confine}
}

// location is a path under a directory that exists, with every symbolic
// link that exists already followed.
type location struct {
	root string
	rel  string
}

func (l location) abs() string { return filepath.Join(l.root, l.rel) }

// Resolve returns the absolute path that path names once symbolic links are
// followed. The path may name a file that does not exist yet. With Confine,
// a path that ends up outside every root is an *Error of KindOutside.
func (r Roots) Resolve(path string) (string, error) {
	loc, err := r.locate(path)
	if err != nil {
		return "", err
	}
	return loc.abs(), nil
}

func (r Roots) locate(path string) (location, error) {
	if !filepath.IsAbs(r.Cwd) {
		return location{}, fmt.Errorf("fsops: working directory %q is not an absolute path", r.Cwd)
	}
	if path == "" {
		return location{}, &Error{Kind: KindNotFound, Path: path, Err: errors.New("the path is empty")}
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(r.Cwd, abs)
	}
	existing, rest, err := resolveExisting(filepath.Clean(abs))
	if err != nil {
		return location{}, classify(path, err)
	}
	resolved := filepath.Join(existing, rest)
	for _, dir := range append([]string{r.Cwd}, r.Dirs...) {
		root, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return location{}, fmt.Errorf("fsops: session directory: %w", err)
		}
		rel, err := filepath.Rel(root, resolved)
		if err == nil && filepath.IsLocal(rel) {
			return location{root: root, rel: rel}, nil
		}
	}
	if r.Confine {
		return location{}, &Error{Kind: KindOutside, Path: path}
	}
	// Outside the session directories the path is reached from the deepest
	// directory that holds it: its parent when it exists, and otherwise the
	// last directory of it that does.
	switch {
	case rest != "":
		return location{root: existing, rel: rest}, nil
	case filepath.Dir(existing) == existing:
		return location{root: existing, rel: "."}, nil
	default:
		return location{root: filepath.Dir(existing), rel: filepath.Base(existing)}, nil
	}
}

// resolveExisting follows the symbolic links in the part of path that
// exists and returns it with the rest, which does not exist. A link that
// points nowhere is an error: what it would create cannot be placed.
func resolveExisting(path string) (existing, rest string, err error) {
	for {
		_, err := os.Lstat(path)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return "", "", err
			}
			return resolved, rest, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", "", err
		}
		rest = filepath.Join(filepath.Base(path), rest)
		path = parent
	}
}

// open opens the directory of a location. Every later access goes through
// the returned os.Root, which refuses a path that leaves it, so a link
// swapped in after locate cannot lead out of a session directory.
func (l location) open() (*os.Root, error) {
	root, err := os.OpenRoot(l.root)
	if err != nil {
		return nil, fmt.Errorf("fsops: open directory: %w", err)
	}
	return root, nil
}

// classify turns an operating system error into an *Error when the model
// can act on it.
func classify(path string, err error) error {
	var known *Error
	switch {
	case errors.As(err, &known):
		return err
	case errors.Is(err, fs.ErrNotExist):
		return &Error{Kind: KindNotFound, Path: path, Err: err}
	case errors.Is(err, fs.ErrPermission):
		return &Error{Kind: KindPermission, Path: path, Err: err}
	default:
		return fmt.Errorf("fsops: %s: %w", path, err)
	}
}

// checkRegular reports a path that is not a regular file.
func checkRegular(path string, info fs.FileInfo) error {
	switch {
	case info.IsDir():
		return &Error{Kind: KindIsDirectory, Path: path}
	case !info.Mode().IsRegular():
		return &Error{Kind: KindNotRegular, Path: path}
	default:
		return nil
	}
}
