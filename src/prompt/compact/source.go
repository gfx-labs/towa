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
	source.FirstKept = view.compaction().FirstKept
	if source.FirstKept == uuid.Nil() && len(view.Active) > 0 {
		source.FirstKept = view.Active[0].ID
	}
	end := -1
	for i, e := range entries {
		if e.ID == view.Compaction.ID {
			end = i
			break
		}
	}
	if view.compaction().FirstKept != uuid.Nil() {
		for i, e := range entries[:end] {
			if e.ID == source.FirstKept {
				end = i
				break
			}
		}
	}
	start := 0
	source.Previous = ""
	for i := end - 1; i >= 0; i-- {
		c, ok := entries[i].Item.Body.(transcript.Compaction)
		if !ok || c.Summary == "" {
			continue
		}
		previous, err := Read(entries[:i+1])
		if err != nil {
			return Source{}, err
		}
		source.Previous = c.Summary
		if len(previous.Active) == 0 {
			start = i + 1
		} else {
			id := previous.Active[0].ID
			for j := range entries[:i] {
				if entries[j].ID == id {
					start = j
					break
				}
			}
		}
		break
	}
	for _, e := range entries[start:end] {
		if !transcript.Is[transcript.Compaction](e.Item) {
			source.Replaced = append(source.Replaced, e)
		}
	}
	if len(source.Replaced) == 0 {
		return Source{}, errors.New("compact: full stored history is required for a portable summary")
	}
	return source, nil
}

// ErrSuperseded means another appended compaction made a background result
// stale. The caller discards it rather than replacing a newer summary.
var ErrSuperseded = errors.New("compact: another compaction superseded this result")

func (source Source) firstKept(entries []transcript.Entry) (uuid.UUID, error) {
	view, err := Read(entries)
	if err != nil {
		return uuid.Nil(), err
	}
	current := uuid.Nil()
	if view.Compaction != nil {
		current = view.Compaction.ID
	}
	if current != source.BaseCompaction {
		return uuid.Nil(), ErrSuperseded
	}
	tail := -1
	for i, e := range entries {
		if e.ID == source.Tail {
			tail = i
			break
		}
	}
	if tail < 0 {
		return uuid.Nil(), fmt.Errorf("compact: captured tail %s is absent from history", source.Tail)
	}
	if source.FirstKept != uuid.Nil() {
		for _, e := range entries[:tail+1] {
			if e.ID == source.FirstKept {
				return source.FirstKept, nil
			}
		}
		return uuid.Nil(), errors.New("compact: first kept entry is absent from captured history")
	}
	if tail+1 < len(entries) {
		return entries[tail+1].ID, nil
	}
	return uuid.Nil(), nil
}
