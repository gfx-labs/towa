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

// ErrTorn matches, with errors.Is, the error for a session whose log ends in
// an append that a crash cut short. The entries before it are intact, and
// Repair makes the session appendable again.
var ErrTorn = errors.New("store: the session's log ends in a torn append")

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
	// Archived hides the session from a List that excludes archived
	// sessions. It changes nothing else: the session loads and appends.
	Archived bool `json:"archived,omitzero"`
	// State belongs to the caller. The store keeps it and returns the same
	// bytes, and never reads it.
	State []byte `json:"state,omitzero"`
}

// Archived selects sessions by their archived state.
type Archived int

// Values of Filter.Archived.
const (
	// ArchivedAny matches every session.
	ArchivedAny Archived = iota
	// ArchivedExclude matches the sessions that are not archived.
	ArchivedExclude
	// ArchivedOnly matches the archived sessions.
	ArchivedOnly
)

// Range selects entries of a session's full history by position, counting
// from zero. To is exclusive, and zero means the end.
type Range struct {
	From, To int
}

// Filter selects sessions for List. A zero field matches every session.
type Filter struct {
	Cwd      string
	ParentID uuid.UUID
	Archived Archived
	// After selects ids after this id, exclusively. List is ordered by id,
	// oldest first. A zero id means the first page.
	After uuid.UUID
	// Limit is the most matches to return, after the filters. Zero means
	// no limit. A negative value is an error.
	Limit int
}

// Store holds sessions.
type Store interface {
	Create(ctx context.Context, meta Meta) error
	// Append adds entries to a session and returns once they are durable.
	// The entries of one call are stored together: after a crash a session
	// holds all of them or none. The session's Updated time becomes the
	// time of the last entry. A session whose log is torn gets an error
	// that matches ErrTorn, and nothing is added.
	Append(ctx context.Context, id uuid.UUID, entries []transcript.Entry) error
	// Load returns the session's entries that are in its context now: the
	// latest compaction entry and everything after it, or every entry when
	// there is no compaction. When the log is torn, the sequence yields
	// every whole entry and then an error that matches ErrTorn.
	Load(ctx context.Context, id uuid.UUID) (Meta, iter.Seq2[transcript.Entry, error], error)
	// History reads a range of every entry ever appended, including the
	// ones before a compaction. A range that reaches the end of a torn log
	// ends as Load does.
	History(ctx context.Context, id uuid.UUID, r Range) (iter.Seq2[transcript.Entry, error], error)
	// Repair sets the torn end of a session's log aside, where it stays in
	// the session's directory, and returns the number of bytes set aside.
	// For a log that is not torn it does nothing and returns zero. Nothing
	// that an Append reported as stored is ever set aside.
	Repair(ctx context.Context, id uuid.UUID) (setAside int64, err error)
	// Fork creates the session meta describes with a copy of source's full
	// history and its stored content, as they are when it is called. The
	// two sessions share nothing afterwards. Source is not changed. A
	// source whose log is torn is not forked: the error matches ErrTorn.
	Fork(ctx context.Context, source uuid.UUID, meta Meta) error
	// SetMeta replaces the fields of a session's Meta that can change:
	// Title, Model, Mode, Dirs, Archived and State.
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
