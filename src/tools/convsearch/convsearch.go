// Package convsearch reads a session's stored history: a keyword search, a
// range of entries, and totals. It reads every entry ever appended,
// including the ones before a compaction, and the sessions of subagents.
// Each function is one capability with typed input and a typed result. The
// tool that calls it turns a result or an *Error into the text the model
// reads.
package convsearch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"uuid"

	"github.com/gfx-labs/acpharness/src/store"
)

// Scope says which sessions a call may read.
type Scope string

// Scopes, from the narrowest to the widest.
const (
	// ScopeSelf is the calling session alone.
	ScopeSelf Scope = "self"
	// ScopeDescendants is the calling session and every subagent session
	// below it, found by parent id.
	ScopeDescendants Scope = "descendants"
	// ScopeStore is every session in the store. A shared store holds other
	// workers' sessions, so configuration has to allow it.
	ScopeStore Scope = "store"
)

// rank orders the scopes. An unknown scope has none.
func (s Scope) rank() (int, bool) {
	switch s {
	case ScopeSelf:
		return 1, true
	case ScopeDescendants:
		return 2, true
	case ScopeStore:
		return 3, true
	default:
		return 0, false
	}
}

// Access is who is asking and how far configuration lets them read.
type Access struct {
	// Session is the calling session.
	Session uuid.UUID
	// Widest is the widest scope configuration allows.
	Widest Scope
}

// Target is the part of an input that picks the sessions to read.
type Target struct {
	// Scope is the scope asked for. Empty means ScopeDescendants, or
	// ScopeSelf when that is all Access.Widest allows.
	Scope Scope
	// Session, when set, reads that one session, which must be in the
	// scope. It is how a parent reads one subagent's session.
	Session uuid.UUID
}

// Limits bound a result. They come from the caller's configuration: the
// package has no limit of its own.
type Limits struct {
	// Hits is the most hits a search returns and the most entries a range
	// returns. It must be positive.
	Hits int
	// SnippetContextBytes is the bytes of context on each side of a match.
	SnippetContextBytes int
	// MessageBytes and ToolResultBytes bound text in a range read.
	MessageBytes    int
	ToolResultBytes int
	// BlobBytes is the largest stored blob a search reads. A larger one is
	// not searched and is counted in the result. Zero reads no blob.
	BlobBytes int64
}

func (l Limits) check() error {
	switch {
	case l.Hits <= 0:
		return fmt.Errorf("convsearch: Limits.Hits %d is not positive", l.Hits)
	case l.SnippetContextBytes <= 0:
		return fmt.Errorf("convsearch: Limits.SnippetContextBytes %d is not positive", l.SnippetContextBytes)
	case l.MessageBytes <= 0 || l.ToolResultBytes <= 0:
		return fmt.Errorf("convsearch: message and tool result byte limits must be positive")
	case l.BlobBytes < 0:
		return fmt.Errorf("convsearch: Limits.BlobBytes %d is negative", l.BlobBytes)
	}
	return nil
}

// Kind says why a call was refused in a way the model can act on.
type Kind string

// Kinds of an Error.
const (
	// KindScopeNotAllowed: the scope asked for is wider than configuration
	// allows. Scope holds what was asked for and Widest what is allowed.
	KindScopeNotAllowed Kind = "scope_not_allowed"
	// KindUnknownScope: Scope is not one of the three scopes.
	KindUnknownScope Kind = "unknown_scope"
	// KindOutOfScope: Session is not among the sessions the scope covers.
	KindOutOfScope Kind = "out_of_scope"
	// KindNotFound: the store does not hold Session.
	KindNotFound Kind = "not_found"
	// KindEmptyQuery: a search has no query.
	KindEmptyQuery Kind = "empty_query"
	// KindInvalidRange: a range starts below zero or does not end after it
	// starts.
	KindInvalidRange Kind = "invalid_range"
)

// Error is a failure the model caused or can act on. Any other error from
// this package means the harness could not read the store.
type Error struct {
	Kind    Kind
	Session uuid.UUID
	Scope   Scope
	Widest  Scope
	// Start and End are the range of a KindInvalidRange error.
	Start, End int
}

func (e *Error) Error() string {
	switch e.Kind {
	case KindScopeNotAllowed:
		return fmt.Sprintf("convsearch: %s: scope %q is wider than the allowed %q", e.Kind, e.Scope, e.Widest)
	case KindUnknownScope:
		return fmt.Sprintf("convsearch: %s: %q", e.Kind, e.Scope)
	case KindOutOfScope, KindNotFound:
		return fmt.Sprintf("convsearch: %s: session %s", e.Kind, e.Session)
	case KindInvalidRange:
		return fmt.Sprintf("convsearch: %s: %d to %d", e.Kind, e.Start, e.End)
	default:
		return fmt.Sprintf("convsearch: %s", e.Kind)
	}
}

// sessions returns the sessions a call reads, the calling session first,
// then the others from the oldest to the newest.
func sessions(ctx context.Context, st store.Store, access Access, target Target) ([]store.Meta, error) {
	widest, ok := access.Widest.rank()
	if !ok {
		return nil, fmt.Errorf("convsearch: Access.Widest %q is not a scope", access.Widest)
	}
	scope := target.Scope
	if scope == "" {
		scope = ScopeDescendants
		if access.Widest == ScopeSelf {
			scope = ScopeSelf
		}
	}
	asked, ok := scope.rank()
	if !ok {
		return nil, &Error{Kind: KindUnknownScope, Scope: scope}
	}
	if asked > widest {
		return nil, &Error{Kind: KindScopeNotAllowed, Scope: scope, Widest: access.Widest}
	}

	// The store has no call for one session's Meta, so every scope starts
	// from the whole listing.
	metas, err := st.List(ctx, store.Filter{})
	if err != nil {
		return nil, fmt.Errorf("convsearch: list sessions: %w", err)
	}
	if scope != ScopeStore {
		metas = family(metas, access.Session, scope == ScopeDescendants)
	}
	if !slices.ContainsFunc(metas, func(m store.Meta) bool { return m.ID == access.Session }) {
		return nil, &Error{Kind: KindNotFound, Session: access.Session}
	}
	slices.SortStableFunc(metas, func(a, b store.Meta) int {
		switch {
		case a.ID == access.Session:
			return -1
		case b.ID == access.Session:
			return 1
		case !a.Created.Equal(b.Created):
			return a.Created.Compare(b.Created)
		default:
			return a.ID.Compare(b.ID)
		}
	})
	if target.Session == (uuid.UUID{}) {
		return metas, nil
	}
	for _, m := range metas {
		if m.ID == target.Session {
			return []store.Meta{m}, nil
		}
	}
	return nil, &Error{Kind: KindOutOfScope, Session: target.Session, Scope: scope}
}

// family picks root out of all, with every session below it when deep is
// set. One listing is read and walked in memory: a List call per session
// would read every session's metadata once for each of them.
func family(all []store.Meta, root uuid.UUID, deep bool) []store.Meta {
	children := map[uuid.UUID][]store.Meta{}
	var out []store.Meta
	for _, m := range all {
		if m.ID == root {
			out = append(out, m)
		}
		children[m.ParentID] = append(children[m.ParentID], m)
	}
	if !deep || len(out) == 0 {
		return out
	}
	// seen stops a loop of parent ids, which a damaged store could hold.
	seen := map[uuid.UUID]bool{root: true}
	for next := []uuid.UUID{root}; len(next) > 0; next = next[1:] {
		for _, child := range children[next[0]] {
			if !seen[child.ID] {
				seen[child.ID] = true
				out = append(out, child)
				next = append(next, child.ID)
			}
		}
	}
	return out
}

// storeError names the session a store error is about. A session deleted
// between the listing and the read is the model's to know about.
func storeError(session uuid.UUID, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return &Error{Kind: KindNotFound, Session: session}
	}
	return fmt.Errorf("convsearch: session %s: %w", session, err)
}
