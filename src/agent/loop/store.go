package loop

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/gfx-labs/towa/src/store"
	"github.com/gfx-labs/towa/src/transcript"
)

// load reads the session's conversation, creating the session when the
// store does not have it, and stores a result for every call left without
// one.
func (tn *turn) load(ctx context.Context) error {
	st := tn.l.o.Store
	if kept, cost, ok := tn.l.o.Kept.take(tn.id); ok {
		done, err := tn.resume(ctx, kept, cost)
		if err != nil || done {
			return err
		}
	}
	entries, err := st.History(ctx, tn.id, store.Range{})
	if errors.Is(err, store.ErrNotFound) {
		meta := store.Meta{ID: tn.id, Cwd: tn.t.Cwd, Dirs: tn.t.Dirs, Model: tn.t.Model}
		if tn.t.ParentID != "" {
			parent, err := uuid.Parse(string(tn.t.ParentID))
			if err != nil {
				return fmt.Errorf("loop: parent session id %q is not a UUID: %w", tn.t.ParentID, err)
			}
			meta.ParentID = parent
		}
		meta.Created = tn.l.o.Now()
		meta.Updated = meta.Created
		if err := st.Create(ctx, meta); err != nil {
			return err
		}
		tn.loaded = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("loop: loading session %s: %w", tn.id, err)
	}
	for entry, err := range entries {
		if err != nil {
			return fmt.Errorf("loop: loading session %s: %w", tn.id, err)
		}
		tn.entries = append(tn.entries, entry)
		if entry.Item.Usage != nil {
			tn.cost.Add(*entry.Item.Usage)
		}
		tn.parent = entry.ID
	}
	tn.loaded = true
	return tn.repairLost(ctx)
}

// resume starts from the entries the last turn kept. Another process may
// have appended to the log since, so it reads the log from the last kept
// entry on: that entry being there at its place says the kept ones are the
// log's start, and what follows it is added. It reports false when the log
// no longer starts with them, and the caller reads it whole.
func (tn *turn) resume(ctx context.Context, kept []transcript.Entry, cost Cost) (bool, error) {
	last := len(kept) - 1
	entries, err := tn.l.o.Store.History(ctx, tn.id, store.Range{From: last})
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("loop: loading session %s: %w", tn.id, err)
	}
	first := true
	for entry, err := range entries {
		if err != nil {
			return false, fmt.Errorf("loop: loading session %s: %w", tn.id, err)
		}
		if first {
			if entry.ID != kept[last].ID {
				return false, nil
			}
			first = false
			continue
		}
		kept = append(kept, entry)
		if entry.Item.Usage != nil {
			cost.Add(*entry.Item.Usage)
		}
	}
	if first {
		return false, nil
	}
	tn.entries, tn.cost, tn.parent, tn.loaded = kept, cost, kept[len(kept)-1].ID, true
	return true, tn.repairLost(ctx)
}

// repairLost stores a result for every call the loaded log left without one.
func (tn *turn) repairLost(ctx context.Context) error {
	repairs, err := lostResults(tn.entries, tn.model.Interruption)
	if err != nil {
		return err
	}
	return tn.store(ctx, repairs)
}

// append stores items in their original order.
func (tn *turn) append(ctx context.Context, items ...transcript.Item) error {
	return tn.store(ctx, items)
}

func (tn *turn) store(ctx context.Context, items []transcript.Item) error {
	if len(items) == 0 {
		return nil
	}
	now := tn.l.o.Now()
	entries := make([]transcript.Entry, len(items))
	for i, item := range items {
		entries[i] = transcript.NewEntry(tn.parent, now, item)
		tn.parent = entries[i].ID
	}
	// A cancelled turn still records how it ended.
	if err := tn.l.o.Store.Append(context.WithoutCancel(ctx), tn.id, entries); err != nil {
		return fmt.Errorf("loop: storing %d entries of session %s: %w", len(entries), tn.id, err)
	}
	tn.entries = append(tn.entries, entries...)
	return nil
}
