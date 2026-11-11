// Package store keeps sessions on disk. The backends are in its
// subpackages.
//
// A session's state is flushed at the end of every turn and lives under one
// directory, so that the directory can be copied to another machine and the
// session loaded there by a new process.
package store

import (
	"context"
	"errors"
	"io"
	"iter"
	"time"
	"uuid"

	"github.com/gfx-labs/acpharness/src/transcript"
)

// ErrNotFound is returned for a session the store does not hold.
var ErrNotFound = errors.New("store: session not found")

// Meta describes a session.
type Meta struct {
	ID uuid.UUID `json:"id"`
	// ParentID is the session that spawned this one as a subagent.
	ParentID uuid.UUID `json:"parent_id,omitzero"`
	// Cwd is the session's working directory, an absolute path.
	Cwd string `json:"cwd"`
	// Dirs are the session's additional directories, absolute paths.
	Dirs    []string  `json:"dirs,omitzero"`
	Title   string    `json:"title,omitzero"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	// Model is the named model the session last used.
	Model string `json:"model,omitzero"`
	// Mode is the session's mode id.
	Mode string `json:"mode,omitzero"`
}

// Range selects entries of a session's full history by position, counting
// from zero. To is exclusive, and zero means the end.
type Range struct {
	From, To int
}

// Filter selects sessions for List. A zero field matches every session.
type Filter struct {
	Cwd      string
	ParentID uuid.UUID
}

// Store holds sessions.
type Store interface {
	Create(ctx context.Context, meta Meta) error
	// Append adds entries to a session and returns once they are durable.
	// It also sets the session's Updated time.
	Append(ctx context.Context, id uuid.UUID, entries []transcript.Entry) error
	// Load returns the session's entries that are in its context now: the
	// latest compaction entry and everything after it, or every entry when
	// there is no compaction.
	Load(ctx context.Context, id uuid.UUID) (Meta, iter.Seq2[transcript.Entry, error], error)
	// History reads a range of every entry ever appended, including the
	// ones before a compaction.
	History(ctx context.Context, id uuid.UUID, r Range) (iter.Seq2[transcript.Entry, error], error)
	// SetMeta replaces the fields of a session's Meta that can change:
	// Title, Model, Mode and Dirs.
	SetMeta(ctx context.Context, meta Meta) error
	List(ctx context.Context, filter Filter) ([]Meta, error)
	Delete(ctx context.Context, id uuid.UUID) error
	// PutBlob stores content for a session under its hash and returns once
	// it is durable. Storing the same content twice keeps one copy. The
	// content lives in the session's directory, so it moves with it.
	PutBlob(ctx context.Context, id uuid.UUID, mimeType string, content io.Reader) (transcript.Stored, error)
	// OpenBlob reads content stored by PutBlob. It returns ErrNotFound for
	// a hash the session does not hold.
	OpenBlob(ctx context.Context, id uuid.UUID, hash string) (io.ReadCloser, error)
}
