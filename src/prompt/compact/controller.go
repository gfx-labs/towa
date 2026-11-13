package compact

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/transcript"
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
	Origin        transcript.Origin
	ContextWindow int64
}

// Controller owns one pending call and bounded embedding history. Before,
// Finish and Close are serialized by the existing session owner. Close
// joins the pending call, but the controller can be reused on a later turn.
type Controller struct {
	opts                  ControllerOptions
	pending               *Task
	pendingNativeProvider string
	pendingNativeOrigin   transcript.Origin
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

// Close cancels and joins this controller's context-aware provider call
// before its owner releases provider resources. Embedding history is kept.
func (c *Controller) Close() {
	if c.pending != nil {
		c.pending.Cancel()
		<-c.pending.done
		c.pending = nil
	}
	c.pendingNativeProvider = ""
	c.pendingNativeOrigin = transcript.Origin{}
}

// Finish drains work already complete at turn end, then cancels and joins
// unfinished work. It never starts a request or waits for summary success.
// The owner appends returned effects before taking its final usage snapshot.
func (c *Controller) Finish(ctx context.Context, in BeforeInput) ([]transcript.Item, error) {
	if c.pending == nil {
		return nil, nil
	}
	result, ready, err := c.pending.Poll()
	compatible := c.pendingMatches(in)
	c.Close()
	if !ready || !compatible || ctx.Err() != nil {
		return nil, nil
	}
	effects, _, err := c.takeResult(result, err, in.Entries)
	return effects, err
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
	c.resetForCompaction(view)
	if !c.pendingMatches(in) {
		c.Close()
	}
	if effects, done, err := c.collectFinished(in.Entries); done {
		return effects, err
	}
	work := c.workFor(in)
	portable, err := EnsurePortable(ctx, in.Entries, in.ProviderName, work, in.Origin)
	if err != nil {
		return nil, err
	}
	if portable != nil {
		c.Close()
		return c.effects(portable, in.Entries)
	}
	usage, sizes, turns, latest := c.history(in.Entries)
	if err := c.embedLatestTurn(ctx, in.Entries, latest); err != nil {
		return nil, err
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
		return c.emergencyCut(ctx, in.Entries, decision)
	case Background:
		return nil, c.startBackground(ctx, in, view, work, native, decision)
	default:
		return nil, fmt.Errorf("compact: unknown decision %d", decision.Action)
	}
}

// resetForCompaction drops what was learned about the previous compaction
// when the transcript has a different latest one: a pending call and the
// embeddings describe a conversation that is no longer sent.
func (c *Controller) resetForCompaction(view View) {
	current := view.compactionID()
	if current != c.lastCompaction {
		c.Close()
		c.embeddings, c.lastEmbedded, c.lastCompaction = nil, uuid.Nil(), current
	}
}

// pendingMatches reports whether a pending native call, if there is one,
// was started for the provider and origin that in selects.
func (c *Controller) pendingMatches(in BeforeInput) bool {
	return c.pendingNativeProvider == "" ||
		(c.pendingNativeProvider == in.ProviderName && c.pendingNativeOrigin == in.Origin)
}

// collectFinished turns a pending call that has already finished into
// effects. done is false when there is nothing to return and Before goes on.
func (c *Controller) collectFinished(entries []transcript.Entry) (effects []transcript.Item, done bool, err error) {
	if c.pending == nil {
		return nil, false, nil
	}
	result, ready, err := c.pending.Poll()
	if !ready {
		return nil, false, nil
	}
	c.Close()
	return c.takeResult(result, err, entries)
}

// takeResult turns a finished call into effects. A result that another
// compaction superseded is dropped and done is false, so the caller goes on.
func (c *Controller) takeResult(result *Result, err error, entries []transcript.Entry) (effects []transcript.Item, done bool, outErr error) {
	if err != nil {
		return nil, true, err
	}
	effects, err = c.effects(result, entries)
	if errors.Is(err, ErrSuperseded) {
		return nil, false, nil
	}
	return effects, true, err
}

// workFor describes a text summary of in's history. Source and the native
// fields are filled in only when a background compaction starts.
func (c *Controller) workFor(in BeforeInput) Work {
	window := c.opts.SummaryContextWindow
	if window == 0 {
		window = in.ContextWindow
	}
	return Work{Model: c.opts.Summary, Input: c.opts.SummaryInput, ContextWindow: window, Mode: c.opts.Config.Mode}
}

// embedLatestTurn adds the embedding of the turn that ended at entries[latest]
// to the history, in semantic mode and once per turn.
func (c *Controller) embedLatestTurn(ctx context.Context, entries []transcript.Entry, latest int) error {
	if c.opts.Config.Mode != config.CompactionSemantic || latest < 0 || entries[latest].ID == c.lastEmbedded {
		return nil
	}
	vector, err := EmbedTurn(ctx, c.opts.Embedder, c.opts.Config, entries[turnStart(entries, latest):latest])
	if err != nil {
		return err
	}
	if len(vector) > 0 {
		if err := c.remember(vector); err != nil {
			return err
		}
	}
	c.lastEmbedded = entries[latest].ID
	return nil
}

// turnStart returns the index of the first entry of the turn whose usage
// entry is entries[latest]: just after the previous usage or compaction.
func turnStart(entries []transcript.Entry, latest int) int {
	start := latest
	for start > 0 {
		if prior := entries[start-1].Item; prior.Usage != nil || prior.Compaction != nil {
			break
		}
		start--
	}
	return start
}

// remember keeps a turn's embedding, dropping the oldest past the window.
func (c *Controller) remember(vector []float32) error {
	if len(c.embeddings) > 0 && len(vector) != len(c.embeddings[0]) {
		return errors.New("compact: embedding dimensions changed within this session")
	}
	c.embeddings = append(c.embeddings, vector)
	if len(c.embeddings) > c.opts.Config.EmbeddingHistoryWindow {
		c.embeddings = c.embeddings[1:]
	}
	return nil
}

// emergencyCut gives a pending summary one bounded wait, then cuts the
// context without a model summary.
func (c *Controller) emergencyCut(ctx context.Context, entries []transcript.Entry, decision Decision) ([]transcript.Item, error) {
	if c.pending != nil {
		if effects, done, err := c.waitPending(ctx, entries); done {
			return effects, err
		}
	}
	source, err := Select(entries, decision.Keep)
	if err != nil {
		return nil, err
	}
	if len(source.Replaced) == 0 {
		return nil, contextFull("a tool-safe emergency cut would replace no entries")
	}
	return c.effects(emergencyResult(source, decision), entries)
}

// waitPending waits the configured time for the pending call and always
// leaves no call pending. done is false when the wait gave nothing to
// return, so the caller cuts the context itself.
func (c *Controller) waitPending(ctx context.Context, entries []transcript.Entry) (effects []transcript.Item, done bool, err error) {
	result, err := c.pending.Wait(ctx, time.Duration(c.opts.Config.PendingWaitMillis)*time.Millisecond)
	c.Close()
	switch {
	case err == nil:
		return c.takeResult(result, nil, entries)
	case errors.Is(err, ErrPending):
		return nil, false, nil
	default:
		return nil, true, err
	}
}

// startBackground starts a compaction that runs beside the next request.
// It starts none when nothing can be replaced.
func (c *Controller) startBackground(ctx context.Context, in BeforeInput, view View, work Work, native Native, decision Decision) error {
	keep, err := c.backgroundKeep(ctx, view, decision)
	if err != nil {
		return err
	}
	work.Source, err = Select(in.Entries, keep)
	if err != nil {
		return err
	}
	if len(work.Source.Replaced) == 0 {
		return nil
	}
	if decision.Native {
		if len(in.Request.Items) == 0 {
			return errors.New("compact: native compaction needs the loop's normalized request items")
		}
		work.Native, work.NativeRequest, work.ProviderName = native, in.Request, in.ProviderName
		c.pendingNativeProvider = in.ProviderName
		c.pendingNativeOrigin = in.Origin
	}
	c.pending = Start(ctx, work)
	return nil
}

// backgroundKeep is how many of the latest messages a background compaction
// leaves. A native one covers the whole context.
func (c *Controller) backgroundKeep(ctx context.Context, view View, decision Decision) (int, error) {
	switch {
	case decision.Native:
		return 0, nil
	case c.opts.Config.Mode == config.CompactionSemantic:
		return SemanticKeep(ctx, c.opts.Embedder, c.opts.Config, view.Active)
	default:
		return decision.Keep, nil
	}
}

func contextFull(message string) error {
	return &provider.Error{Class: provider.Fail, Reason: provider.ReasonContextFull, Message: message}
}

// effects returns the items to append for a finished compaction: its usage
// when it has one, then the compaction item.
func (c *Controller) effects(result *Result, history []transcript.Entry) ([]transcript.Item, error) {
	item, err := result.ItemFor(history)
	if err != nil {
		return nil, err
	}
	if len(result.Report.Omissions) > 0 {
		item.Compaction.Summary += omissionNotice(result.Report.Omissions)
	}
	var effects []transcript.Item
	if result.Usage != nil {
		effects = append(effects, transcript.Item{Usage: result.Usage})
	}
	return append(effects, item), nil
}

// omissionNotice is the text appended to a summary whose input left text out.
func omissionNotice(omissions []Omission) string {
	var bytes, native int
	for _, omission := range omissions {
		bytes += omission.Bytes
		if omission.Reason == "unreadable native content" {
			native++
		}
	}
	return fmt.Sprintf("\n\n[towa: summary input omitted %d UTF-8 bytes of text and %d unreadable native items. Full entries remain in stored history.]", bytes, native)
}

// history reads the usage of the entries since the latest compaction: the
// last usage, the context size after each turn within the history window,
// the turn count, and the index of the last usage entry, or -1.
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
