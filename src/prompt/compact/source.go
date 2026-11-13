package compact

import (
	"errors"
	"fmt"
	"uuid"

	"github.com/gfx-labs/towa/src/transcript"
)

// Source is an immutable snapshot to summarize. It holds references to the
// caller's immutable entries, not a second conversation log. The caller
// serializes appends and calls Result.ItemFor before appending its result.
type Source struct {
	Replaced  []transcript.Entry
	Previous  string
	FirstKept uuid.UUID
	// BaseCompaction guards against applying a result after another
	// compaction superseded it. Tail is the last entry at capture time.
	BaseCompaction uuid.UUID
	Tail           uuid.UUID
}

// Select captures the prefix replaced by a model summary. No entry is
// changed. A zero-length Replaced means nothing can be compacted safely.
func Select(entries []transcript.Entry, keep int) (Source, error) {
	view, err := Read(entries)
	if err != nil {
		return Source{}, err
	}
	n, first := Cut(view.Active, keep)
	source := sourceFor(entries, view)
	source.Replaced, source.FirstKept = view.Active[:n], first
	return source, nil
}

// sourceFor starts a Source with the guards that detect a stale result.
func sourceFor(entries []transcript.Entry, view View) Source {
	source := Source{Previous: view.Summary()}
	if len(entries) > 0 {
		source.Tail = entries[len(entries)-1].ID
	}
	if view.Compaction != nil {
		source.BaseCompaction = view.Compaction.ID
	}
	return source
}

// PortableSource reconstructs the readable history hidden by the latest
// native compaction. Its ciphertext is never sent to the summary model.
// The newest earlier text summary is reused when it covers that prefix.
// entries must come from Store.History, not a compacted Load snapshot.
func PortableSource(entries []transcript.Entry) (Source, error) {
	view, err := Read(entries)
	if err != nil {
		return Source{}, err
	}
	if view.Compaction == nil {
		return Source{}, errors.New("compact: no compaction needs a portable summary")
	}
	source := sourceFor(entries, view)
	source.FirstKept = view.Compaction.Item.Compaction.FirstKept
	if source.FirstKept == uuid.Nil() && len(view.Active) > 0 {
		source.FirstKept = view.Active[0].ID
	}
	end := indexOfID(entries, view.Compaction.ID)
	if view.Compaction.Item.Compaction.FirstKept != uuid.Nil() {
		if i := indexOfID(entries[:end], source.FirstKept); i >= 0 {
			end = i
		}
	}
	previous, start, err := earlierSummary(entries, end)
	if err != nil {
		return Source{}, err
	}
	source.Previous = previous
	for _, e := range entries[start:end] {
		if e.Item.Compaction == nil {
			source.Replaced = append(source.Replaced, e)
		}
	}
	if len(source.Replaced) == 0 {
		return Source{}, errors.New("compact: full stored history is required for a portable summary")
	}
	return source, nil
}

// earlierSummary finds the newest text summary before entries[end]. It
// returns that summary and the index where the entries it did not cover
// begin. Without one it returns no summary and index 0.
func earlierSummary(entries []transcript.Entry, end int) (summary string, start int, err error) {
	for i := end - 1; i >= 0; i-- {
		c := entries[i].Item.Compaction
		if c == nil || c.Summary == "" {
			continue
		}
		covered, err := Read(entries[:i+1])
		if err != nil {
			return "", 0, err
		}
		if len(covered.Active) == 0 {
			return c.Summary, i + 1, nil
		}
		return c.Summary, max(indexOfID(entries[:i], covered.Active[0].ID), 0), nil
	}
	return "", 0, nil
}

// ErrSuperseded means another appended compaction made a background result
// stale. The caller discards it rather than replacing a newer summary.
var ErrSuperseded = errors.New("compact: another compaction superseded this result")

// firstKept is the id of the first entry that stays after this source's
// compaction is appended to entries, which may have grown since the capture.
func (source Source) firstKept(entries []transcript.Entry) (uuid.UUID, error) {
	view, err := Read(entries)
	if err != nil {
		return uuid.Nil(), err
	}
	if view.compactionID() != source.BaseCompaction {
		return uuid.Nil(), ErrSuperseded
	}
	tail := indexOfID(entries, source.Tail)
	if tail < 0 {
		return uuid.Nil(), fmt.Errorf("compact: captured tail %s is absent from history", source.Tail)
	}
	if source.FirstKept != uuid.Nil() {
		if indexOfID(entries[:tail+1], source.FirstKept) < 0 {
			return uuid.Nil(), errors.New("compact: first kept entry is absent from captured history")
		}
		return source.FirstKept, nil
	}
	if tail+1 < len(entries) {
		return entries[tail+1].ID, nil
	}
	return uuid.Nil(), nil
}
