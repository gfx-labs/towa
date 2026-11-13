// Package compact decides when a session's context is compacted, builds the
// request that asks a model for the summary, and reads a transcript that
// holds compactions.
//
// A compaction is one appended entry. It names the first entry that is
// still sent and holds the summary that stands for everything before it.
// Nothing here removes or rewrites an entry: the replaced entries stay in
// the store, where conversation search reads them.
package compact

import (
	"errors"
	"fmt"
	"strings"
	"uuid"

	"github.com/gfx-labs/towa/src/transcript"
)

// Values of transcript.Compaction.Mode that are not a configured mode.
const (
	// ModeManual: the user asked for the compaction.
	ModeManual = "manual"
	// ModeEmergency: the context was cut without a model summary, because
	// the next request would not have fit.
	ModeEmergency = "emergency"
	// ModeNative: the provider compacted the conversation itself.
	ModeNative = "native"
)

// ErrEmptySummary reports a compaction with no summary and no native items.
var ErrEmptySummary = errors.New("compact: the summary is empty")

// UnreadableError reports a native compaction that the provider about to be
// asked cannot read, and that has no summary text. The entries it replaced
// are still in the store, so the caller can summarise them again.
type UnreadableError struct {
	// Entry is the compaction entry.
	Entry uuid.UUID
	// Provider produced the compaction. Wanted is the one being asked.
	Provider, Wanted string
}

func (e *UnreadableError) Error() string {
	return fmt.Sprintf("compact: compaction %s was made by provider %q and has no summary that %q can read", e.Entry, e.Provider, e.Wanted)
}

// View is a transcript as the next request sees it.
type View struct {
	// Compaction is the latest compaction entry, or nil when there is none.
	Compaction *transcript.Entry
	// Active holds the entries still sent, in order: the ones from the
	// compaction's first kept entry on, without compaction entries.
	Active []transcript.Entry
}

// Summary is the text of the latest compaction, or empty.
func (v View) Summary() string {
	if v.Compaction == nil {
		return ""
	}
	return v.Compaction.Item.Compaction.Summary
}

// compactionID is the id of the latest compaction entry, or the nil id.
func (v View) compactionID() uuid.UUID {
	if v.Compaction == nil {
		return uuid.Nil()
	}
	return v.Compaction.ID
}

// Read finds the latest compaction in entries and the entries it kept. It
// returns slices of entries and changes nothing.
func Read(entries []transcript.Entry) (View, error) {
	last := -1
	for i := range entries {
		if entries[i].Item.Compaction != nil {
			last = i
		}
	}
	if last < 0 {
		return View{Active: entries}, nil
	}
	first, err := keptFrom(entries, last)
	if err != nil {
		return View{}, err
	}
	active := make([]transcript.Entry, 0, len(entries)-first)
	for _, e := range entries[first:] {
		if e.Item.Compaction == nil {
			active = append(active, e)
		}
	}
	return View{Compaction: &entries[last], Active: active}, nil
}

// keptFrom is the index of the first entry the compaction at entries[at]
// kept. A nil id means nothing before the compaction was kept.
func keptFrom(entries []transcript.Entry, at int) (int, error) {
	firstKept := entries[at].Item.Compaction.FirstKept
	if firstKept == uuid.Nil() {
		return at, nil
	}
	if i := indexOfID(entries[:at], firstKept); i >= 0 {
		return i, nil
	}
	return 0, fmt.Errorf("compact: compaction %s keeps from entry %s, which is not before it in the transcript",
		entries[at].ID, firstKept)
}

// indexOfID is the index of the entry with id, or -1.
func indexOfID(entries []transcript.Entry, id uuid.UUID) int {
	for i, e := range entries {
		if e.ID == id {
			return i
		}
	}
	return -1
}

// Items returns the items to send to providerName after any compaction: the
// summary as a user message, or the provider's own items for a native
// compaction it made, and then the kept entries' items. Without a
// compaction it returns every entry's item.
//
// The result still holds Usage, ModelSwitch and Interrupted items. The
// loop's Conversation deals with those, as it does without a compaction.
func Items(entries []transcript.Entry, providerName string, target ...transcript.Origin) ([]transcript.Item, error) {
	view, err := Read(entries)
	if err != nil {
		return nil, err
	}
	items := make([]transcript.Item, 0, len(view.Active)+1)
	if view.Compaction != nil {
		head, err := standIn(*view.Compaction, providerName, target...)
		if err != nil {
			return nil, err
		}
		items = append(items, head...)
	}
	for _, e := range view.Active {
		items = append(items, e.Item)
	}
	return items, nil
}

// standIn returns the items that stand for what a compaction replaced.
func standIn(entry transcript.Entry, providerName string, target ...transcript.Origin) ([]transcript.Item, error) {
	item := entry.Item
	if len(target) > 1 {
		return nil, errors.New("compact: more than one target origin")
	}
	if len(target) == 1 && item.OpaqueCompatible(providerName, target[0]) {
		return nativeWindow(entry, providerName, target[0])
	}
	summary := item.Compaction.Summary
	if strings.TrimSpace(summary) == "" {
		if len(item.Opaque) > 0 {
			return nil, &UnreadableError{Entry: entry.ID, Provider: item.Provider, Wanted: providerName}
		}
		return nil, fmt.Errorf("compaction %s: %w", entry.ID, ErrEmptySummary)
	}
	text := summaryPrefix + summary + summarySuffix
	return []transcript.Item{{User: &transcript.User{Parts: []transcript.Part{{Text: text}}}}}, nil
}
