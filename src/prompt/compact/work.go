package compact

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/transcript"
)

// Work is one compaction captured by the session owner. A text summary uses
// Model, Input and ContextWindow. A native compaction uses Native and
// NativeRequest, which must describe the whole captured context. Source
// must then come from Select(history, 0), not a recent-message cut.
type Work struct {
	Source        Source
	Mode          string
	Model         provider.Provider
	Input         SummaryInput
	ContextWindow int64
	Native        Native
	NativeRequest provider.Request
	ProviderName  string
}

// Result is a completed compaction, not yet appended. Only the session
// owner appends, after ItemFor checks that no newer compaction replaced its
// source. Usage and Report let that owner report cost and omitted input.
type Result struct {
	Source Source
	Usage  *transcript.Usage
	Report SummaryReport
	item   transcript.Item
}

// ItemFor returns the one item to append now. Entries added while the
// summary was running are kept, even when the captured cut kept nothing.
// The caller serializes this check and its append with other session writes.
func (r *Result) ItemFor(history []transcript.Entry) (transcript.Item, error) {
	if r == nil || r.item.Compaction == nil {
		return transcript.Item{}, errors.New("compact: no completed result to append")
	}
	first, err := r.Source.firstKept(history)
	if err != nil {
		return transcript.Item{}, err
	}
	item := r.item
	c := *item.Compaction
	c.FirstKept = first
	item.Compaction = &c
	return item, nil
}

// Run performs one provider call and returns no usable result on a cut-off,
// refused, failed or empty summary. It never calls a tool or writes a store.
func Run(ctx context.Context, work Work) (*Result, error) {
	if len(work.Source.Replaced) == 0 {
		return nil, errors.New("compact: nothing was selected for compaction")
	}
	if work.Native != nil {
		return runNative(ctx, work)
	}
	return runSummary(ctx, work)
}

// runNative asks the provider to compact the whole captured context.
func runNative(ctx context.Context, work Work) (*Result, error) {
	if work.Source.FirstKept != uuid.Nil() {
		return nil, errors.New("compact: a native request must cover the whole captured context")
	}
	got, err := work.Native.Compact(ctx, work.NativeRequest)
	if err != nil {
		return nil, err
	}
	item, err := NativeItem(got, work.ProviderName, work.Source.FirstKept)
	if err != nil {
		return nil, err
	}
	return &Result{Source: work.Source, Usage: got.Usage, item: item}, nil
}

// runSummary asks the summary model for a text summary of the source.
func runSummary(ctx context.Context, work Work) (*Result, error) {
	if work.Model == nil || work.Mode == "" {
		return nil, errors.New("compact: summary model and mode are required")
	}
	in := work.Input
	in.Replaced, in.Previous = work.Source.Replaced, work.Source.Previous
	req, report, err := BoundedSummaryRequest(in, work.ContextWindow)
	if err != nil {
		return nil, err
	}
	result := &Result{Source: work.Source, Report: report}
	items, stop, usage, err := collectStream(ctx, work.Model, req)
	if err != nil {
		return nil, err
	}
	result.Usage = usage
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}
	summary, err := SummaryText(items, stop)
	if err != nil {
		return nil, err
	}
	result.item = transcript.Item{Compaction: &transcript.Compaction{
		Summary: summary, FirstKept: work.Source.FirstKept, Mode: work.Mode,
	}}
	return result, nil
}

// collectStream reads a whole response: its finished items, stop reason
// and the last usage it reported.
func collectStream(ctx context.Context, model provider.Provider, req provider.Request) (items []transcript.Item, stop provider.StopReason, usage *transcript.Usage, err error) {
	for event, err := range model.Stream(ctx, req) {
		if err != nil {
			return nil, "", nil, err
		}
		if event.Item != nil {
			items = append(items, *event.Item)
		}
		if event.Usage != nil {
			u := *event.Usage
			usage = &u
		}
		if event.Stop != "" {
			stop = event.Stop
		}
	}
	return items, stop, usage, nil
}

// EnsurePortable writes a real model summary when a different provider
// cannot read the latest native compaction. A nil result means the context
// already has a readable representation. The caller appends the result and
// renders Items again before sending anything to that next provider.
func EnsurePortable(ctx context.Context, history []transcript.Entry, providerName string, work Work, target ...transcript.Origin) (*Result, error) {
	_, err := Items(history, providerName, target...)
	if err == nil {
		return nil, nil
	}
	if _, ok := errors.AsType[*UnreadableError](err); !ok {
		return nil, err
	}
	source, err := PortableSource(history)
	if err != nil {
		return nil, err
	}
	work.Source, work.Native, work.Mode = source, nil, ModePortable
	return Run(ctx, work)
}

// ModePortable records a text summary written to cross a provider boundary.
const ModePortable = "portable"

// ErrPending says a bounded wait ended without a result. The task is still
// running until the owner cancels it or its parent context ends.
var ErrPending = errors.New("compact: background summary is still pending")

// Task is just one background call, owned by the existing session or turn
// context. It has no store, registry, timers that poll, or session lifetime.
// Cancel is required when its result is discarded or the owner closes.
type Task struct {
	done   chan struct{}
	cancel context.CancelFunc
	result *Result
	err    error
}

// Start runs work under ctx. The caller must keep Source entries immutable.
func Start(ctx context.Context, work Work) *Task {
	ctx, cancel := context.WithCancel(ctx)
	task := &Task{done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(task.done)
		defer cancel()
		task.result, task.err = Run(ctx, work)
	}()
	return task
}

// Cancel cancels the underlying provider call. It does not wait for it.
func (task *Task) Cancel() { task.cancel() }

// Poll returns ready false without waiting. When ready, the same immutable
// result or error remains available to Wait and subsequent Poll calls.
func (task *Task) Poll() (result *Result, ready bool, err error) {
	select {
	case <-task.done:
		return task.result, true, task.err
	default:
		return nil, false, nil
	}
}

// Wait waits at most limit, and never beyond ctx. An emergency caller uses
// compaction.pending_wait_ms, checks the result, then cancels and appends an
// explicit emergency omission if still pending. It does not poll or sleep.
func (task *Task) Wait(ctx context.Context, limit time.Duration) (*Result, error) {
	if limit < 0 {
		return nil, fmt.Errorf("compact: negative pending wait %s", limit)
	}
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}
	if result, ready, err := task.Poll(); ready {
		return result, err
	}
	if limit == 0 {
		return nil, ErrPending
	}
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	case <-timer.C:
		return nil, ErrPending
	case <-task.done:
		return task.result, task.err
	}
}
