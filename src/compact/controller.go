package compact

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// ControllerOptions wires compaction into an existing session owner. No
// option owns a store or a second session table.
type ControllerOptions struct {
	Config       config.Compaction
	Summary      provider.Provider
	SummaryInput SummaryInput
	// Zero uses the selected request model's window. A distinct summary
	// model needs its own metadata here.
	SummaryContextWindow int64
	Embedder             Embedder
	// Native resolves the actual chain entry, not just the first model's
	// provider. It returns nil for endpoints without native compaction.
	// The adapter uses the credential already in BeforeInput.Request.
	Native func(providerName string) Native
}

// BeforeInput is the loop's full history and selected chain entry. Request
// has that entry's credential and model. For explicit native compaction its
// Items must already have passed the loop's Conversation repair/rendering.
type BeforeInput struct {
	Entries       []transcript.Entry
	Request       provider.Request
	ProviderName  string
	ContextWindow int64
}

// Controller owns one pending call and bounded embedding history. Before
// and Close are serialized by the existing session owner. Close cancels the
// pending call, but the controller can be reused on a later turn.
type Controller struct {
	opts                  ControllerOptions
	pending               *Task
	pendingNativeProvider string
	embeddings            [][]float32
	lastEmbedded          uuid.UUID
	lastCompaction        uuid.UUID
}

// NewController validates the dependencies as well as standalone config.
// Native config must be wired even when the current model is not OpenAI:
// its resolver then explicitly returns nil for that provider.
func NewController(opts ControllerOptions) (*Controller, error) {
	if err := opts.Config.Validate(); err != nil {
		return nil, err
	}
	if opts.Summary == nil || opts.SummaryInput.MaxOutputTokens <= 0 {
		return nil, errors.New("compact: summary provider and positive output allowance are required")
	}
	if opts.SummaryContextWindow < 0 {
		return nil, errors.New("compact: negative summary context window")
	}
	if opts.Config.Mode == config.CompactionSemantic && opts.Embedder == nil {
		return nil, errors.New("compact: semantic mode needs the configured embeddings client")
	}
	if opts.Config.OpenAINative != config.NativeOff && opts.Native == nil {
		return nil, errors.New("compact: native compaction is enabled but no native resolver was supplied")
	}
	opts.SummaryInput.Prompt = opts.Config.SummaryPrompt
	opts.SummaryInput.Replaced, opts.SummaryInput.Previous = nil, ""
	return &Controller{opts: opts}, nil
}

// Close cancels only this controller's pending provider call. The existing
// session context owns the controller's lifetime.
func (c *Controller) Close() {
	if c.pending != nil {
		c.pending.Cancel()
		c.pending = nil
	}
	c.pendingNativeProvider = ""
}

// Before returns effects to append durably, in order, before the loop calls
// Items and Conversation. It never writes, deletes, dispatches, or replays a
// tool. On an error no new main model request may be sent. The router masks
// its prepare hook while this runs, preventing recursive summary requests.
func (c *Controller) Before(ctx context.Context, in BeforeInput) ([]transcript.Item, error) {
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}
	if in.ContextWindow <= 0 {
		return nil, errors.New("compact: selected model has no positive context window")
	}
	view, err := Read(in.Entries)
	if err != nil {
		return nil, err
	}
	current := uuid.Nil()
	if view.Compaction != nil {
		current = view.Compaction.ID
	}
	if current != c.lastCompaction {
		c.Close()
		c.embeddings, c.lastEmbedded, c.lastCompaction = nil, uuid.Nil(), current
	}
	if c.pendingNativeProvider != "" && c.pendingNativeProvider != in.ProviderName {
		c.Close()
	}
	if c.pending != nil {
		if result, ready, err := c.pending.Poll(); ready {
			c.Close()
			if err != nil {
				return nil, err
			}
			effects, err := c.effects(result, in.Entries)
			if !errors.Is(err, ErrSuperseded) {
				return effects, err
			}
		}
	}
	window := c.opts.SummaryContextWindow
	if window == 0 {
		window = in.ContextWindow
	}
	work := Work{Model: c.opts.Summary, Input: c.opts.SummaryInput, ContextWindow: window, Mode: c.opts.Config.Mode}
	portable, err := EnsurePortable(ctx, in.Entries, in.ProviderName, work)
	if err != nil {
		return nil, err
	}
	if portable != nil {
		c.Close()
		return c.effects(portable, in.Entries)
	}
	usage, sizes, turns, latest := c.history(in.Entries)
	if c.opts.Config.Mode == config.CompactionSemantic && latest >= 0 && in.Entries[latest].ID != c.lastEmbedded {
		start := latest
		for start > 0 {
			if prior := in.Entries[start-1].Item; prior.Usage != nil || prior.Compaction != nil {
				break
			}
			start--
		}
		vector, err := EmbedTurn(ctx, c.opts.Embedder, c.opts.Config, in.Entries[start:latest])
		if err != nil {
			return nil, err
		}
		if len(vector) > 0 {
			if len(c.embeddings) > 0 && len(vector) != len(c.embeddings[0]) {
				return nil, errors.New("compact: embedding dimensions changed within this session")
			}
			c.embeddings = append(c.embeddings, vector)
			if len(c.embeddings) > c.opts.Config.EmbeddingHistoryWindow {
				c.embeddings = c.embeddings[1:]
			}
		}
		c.lastEmbedded = in.Entries[latest].ID
	}
	var native Native
	if c.opts.Native != nil {
		native = c.opts.Native(in.ProviderName)
	}
	decision, err := Decide(Input{
		Config: c.opts.Config, ContextWindow: in.ContextWindow, Usage: usage,
		Messages: Messages(view.Active), Running: c.pending != nil, History: sizes,
		TurnsSinceCompaction: turns, Embeddings: c.embeddings, NativeAvailable: native != nil,
	})
	if err != nil {
		return nil, err
	}
	switch decision.Action {
	case None:
		return nil, nil
	case Stop:
		return nil, contextFull("the next request reaches the emergency threshold and no safe cut is allowed")
	case Emergency:
		if c.pending != nil {
			result, err := c.pending.Wait(ctx, time.Duration(c.opts.Config.PendingWaitMillis)*time.Millisecond)
			if err == nil {
				c.Close()
				effects, err := c.effects(result, in.Entries)
				if !errors.Is(err, ErrSuperseded) {
					return effects, err
				}
			} else if !errors.Is(err, ErrPending) {
				c.Close()
				return nil, err
			}
			c.Close()
		}
		source, err := Select(in.Entries, decision.Keep)
		if err != nil {
			return nil, err
		}
		if len(source.Replaced) == 0 {
			return nil, contextFull("a tool-safe emergency cut would replace no entries")
		}
		summary := EmergencySummary(source.Previous, source.Replaced, decision.ContextTokens, decision.Budget)
		result := &Result{Source: source, item: transcript.Item{Compaction: &transcript.Compaction{Summary: summary, Mode: ModeEmergency}}}
		return c.effects(result, in.Entries)
	case Background:
		keep := decision.Keep
		if decision.Native {
			keep = 0
		} else if c.opts.Config.Mode == config.CompactionSemantic {
			keep, err = SemanticKeep(ctx, c.opts.Embedder, c.opts.Config, view.Active)
			if err != nil {
				return nil, err
			}
		}
		work.Source, err = Select(in.Entries, keep)
		if err != nil {
			return nil, err
		}
		if len(work.Source.Replaced) == 0 {
			return nil, nil
		}
		if decision.Native {
			if len(in.Request.Items) == 0 {
				return nil, errors.New("compact: native compaction needs the loop's normalized request items")
			}
			work.Native, work.NativeRequest, work.ProviderName = native, in.Request, in.ProviderName
			c.pendingNativeProvider = in.ProviderName
		}
		c.pending = Start(ctx, work)
		return nil, nil
	default:
		return nil, fmt.Errorf("compact: unknown decision %d", decision.Action)
	}
}

func contextFull(message string) error {
	return &provider.Error{Class: provider.Fail, Reason: provider.ReasonContextFull, Message: message}
}

func (c *Controller) effects(result *Result, history []transcript.Entry) ([]transcript.Item, error) {
	item, err := result.ItemFor(history)
	if err != nil {
		return nil, err
	}
	if len(result.Report.Omissions) > 0 {
		var bytes, native int
		for _, omission := range result.Report.Omissions {
			bytes += omission.Bytes
			if omission.Reason == "unreadable native content" {
				native++
			}
		}
		item.Compaction.Summary += fmt.Sprintf("\n\n[acpharness: summary input omitted %d UTF-8 bytes of text and %d unreadable native items. Full entries remain in stored history.]", bytes, native)
	}
	var effects []transcript.Item
	if result.Usage != nil {
		effects = append(effects, transcript.Item{Usage: result.Usage})
	}
	return append(effects, item), nil
}

func (c *Controller) history(entries []transcript.Entry) (usage transcript.Usage, sizes []int64, turns, latest int) {
	latest = -1
	for i, e := range entries {
		if e.Item.Compaction != nil {
			usage, sizes, turns, latest = transcript.Usage{}, nil, 0, -1
		}
		if e.Item.Usage == nil {
			continue
		}
		usage, latest = *e.Item.Usage, i
		sizes = append(sizes, ContextTokens(usage))
		if len(sizes) > c.opts.Config.HistoryWindow {
			sizes = sizes[1:]
		}
		turns++
	}
	return usage, sizes, turns, latest
}
