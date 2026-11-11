package loop

import (
	"context"
	"errors"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/compact"
	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/provider/router"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// turnLimitError carries a limit through a router's preparation callback.
// It is answered as a prompt result, not a provider failure.
type turnLimitError struct{ key string }

func (e *turnLimitError) Error() string { return "loop: the turn reached " + e.key }

func (tn *turn) contextItems(ctx context.Context, name, interruption string, origin transcript.Origin) ([]transcript.Item, error) {
	items, err := compact.Items(tn.entries, name, origin)
	if err != nil {
		return nil, err
	}
	items, repairs, err := Conversation(items, interruption)
	if err != nil {
		return nil, err
	}
	if err := tn.store(ctx, repairs); err != nil {
		return nil, err
	}
	return items, nil
}

func (tn *turn) prepareRequest(ctx context.Context, entry router.Entry, req provider.Request) (provider.Request, error) {
	window, interruption := tn.model.ContextSize, tn.model.Interruption
	if selected := entry.Preparation; selected != nil {
		if selected.ContextWindow <= 0 || selected.MaxOutputTokens <= 0 {
			return provider.Request{}, errors.New("loop: selected entry needs positive context and output allowances")
		}
		window, interruption = selected.ContextWindow, selected.Interruption
		req.System, req.MaxOutputTokens = selected.System, selected.MaxOutputTokens
	}
	if window > 0 {
		tn.mapper.ContextSize = func(string) (int, bool) { return int(window), true }
	}
	name := tn.model.ProviderName
	if entry.Provider != nil {
		name = ""
		if namer, ok := entry.Provider.(provider.ContextNamer); ok {
			var err error
			name, err = namer.ContextName(req.Credential)
			if err != nil {
				return provider.Request{}, err
			}
		}
	}
	origin := transcript.Origin{Model: req.Model, Account: req.Credential.Account}
	items, err := tn.contextItems(ctx, name, interruption, origin)
	if err != nil {
		var unreadable *compact.UnreadableError
		if tn.compactor == nil || !errors.As(err, &unreadable) {
			return provider.Request{}, err
		}
	}
	req.Items = items
	if tn.compactor != nil {
		in := compact.BeforeInput{
			Entries: tn.entries, Request: req, ProviderName: name, ContextWindow: window, Origin: origin,
		}
		tn.compactionInput = &in
		effects, err := tn.compactor.Before(ctx, in)
		if err != nil {
			return provider.Request{}, StopFor(err)
		}
		if err := tn.applyCompaction(ctx, effects); err != nil {
			return provider.Request{}, err
		}
		req.Items, err = tn.contextItems(ctx, name, interruption, origin)
		if err != nil {
			return provider.Request{}, err
		}
	}
	tn.spentMu.Lock()
	tn.spent.Wall = tn.l.o.Now().Sub(tn.started)
	tn.spentMu.Unlock()
	if key, over := Exceeded(tn.limits, tn.snapshot()); over {
		return provider.Request{}, &turnLimitError{key: key}
	}
	return req, nil
}

func (tn *turn) applyCompaction(ctx context.Context, effects []transcript.Item) error {
	for _, effect := range effects {
		if effect.Usage != nil {
			tn.add(*effect.Usage)
		}
	}
	if err := tn.store(ctx, effects); err != nil {
		return err
	}
	for i, effect := range effects {
		if effect.Usage != nil {
			if update, ok := tn.mapper.Usage(*effect.Usage); ok {
				tn.send(ctx, acp.SessionUpdate{UsageUpdate: &update})
			}
		}
		if effect.Compaction != nil {
			entry := tn.entries[len(tn.entries)-len(effects)+i]
			update := acp.SessionUpdate{CompactionUpdate: &acp.SessionCompactionUpdate{
				CompactionId: acp.CompactionId(entry.ID.String()), Status: acp.CompactionStatusCompleted,
			}}
			if effect.Compaction.Summary != "" {
				update.CompactionUpdate.Summary = []acp.ContentBlock{textBlock(effect.Compaction.Summary)}
			}
			if tn.t.Sendable != nil && tn.t.Sendable(update) {
				tn.send(ctx, update)
			}
		}
	}
	return tn.sendErr
}
