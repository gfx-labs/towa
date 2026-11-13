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
		return st.Create(ctx, meta)
	}
	if err != nil {
		return fmt.Errorf("loop: loading session %s: %w", tn.id, err)
	}
	var history []transcript.Item
	for entry, err := range entries {
		if err != nil {
			return fmt.Errorf("loop: loading session %s: %w", tn.id, err)
		}
		tn.entries = append(tn.entries, entry)
		if entry.Item.Compaction == nil {
			history = append(history, entry.Item)
		}
		tn.parent = entry.ID
	}
	_, repairs, err := Conversation(history, tn.model.Interruption)
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
