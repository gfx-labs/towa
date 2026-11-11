package loop

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"sync"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// Settle decides the result of a call that was dispatched: tool.Run was
// called and returned res and runErr. cancelled says whether the turn was
// cancelled by then.
//
//   - A tool that returned a result knows best what happened, also after a
//     cancel: its outcome stands.
//   - A tool that returned an error after a cancel did not say whether it
//     took effect. That is the unknown outcome, except for a tool that only
//     reads, which cannot have had one.
//   - Any other error means the harness could not run the tool. The model
//     is told the call failed.
func Settle(effect tools.Effect, res tools.Result, runErr error, cancelled bool) tools.Result {
	text := func(outcome transcript.Outcome, s string) tools.Result {
		return tools.Result{Outcome: outcome, Parts: []transcript.Part{{Text: s}}}
	}
	switch {
	case runErr == nil:
		if _, err := Status(res.Outcome); err != nil {
			return text(transcript.OutcomeFailed, textNoOutcome)
		}
		return res
	case cancelled && effect == tools.ReadOnly:
		return text(transcript.OutcomeCancelled, textReadStopped)
	case cancelled:
		return text(transcript.OutcomeUnknown, textUnknown)
	default:
		return text(transcript.OutcomeFailed, fmt.Sprintf(textHarnessError, runErr))
	}
}

// JobFor is what the scheduler needs to know about a call. A call that
// only reads is taken to read the paths it shows the client as locations.
func JobFor(effect tools.Effect, d tools.Description) Job {
	job := Job{Effect: effect}
	for _, p := range d.WritePaths {
		job.WritePaths = append(job.WritePaths, filepath.Clean(p))
	}
	for _, l := range d.Locations {
		job.ReadPaths = append(job.ReadPaths, filepath.Clean(l.Path))
	}
	return job
}

// Dispatcher runs the tool calls of one model response.
type Dispatcher struct {
	Tools tools.Set
	Env   tools.Env
	// Left is how many calls may still be dispatched, when Limited is set.
	// LimitKey is the config key named to the model for a call over it.
	Left     int64
	Limited  bool
	LimitKey string
	// Permit is Turn.Permit. Nil allows every call.
	Permit func(ctx context.Context, tool string, effect tools.Effect, call acp.ToolCallUpdate) (bool, error)
	// Update sends an update for a call. It may be called from several
	// goroutines at once. An error is reported through Failed.
	Update func(update acp.SessionToolCallUpdate)
	// PutBlob is tools.Env.PutBlob for this session.
	PutBlob func(ctx context.Context, mimeType string, content io.Reader) (transcript.Stored, error)
	// Failed is told about a tool the harness could not run.
	Failed func(call transcript.ToolCall, err error)
}

// Dispatched is what running a batch of calls came to.
type Dispatched struct {
	// Results has one result per call, in the calls' order.
	Results []transcript.ToolResult
	// Ran is how many calls were dispatched: handed to their tool.
	Ran int64
	// OverLimit says a call was not run because the limit was reached.
	OverLimit bool
	// Err is why the turn must stop, when asking for permission failed.
	Err error
}

type planned struct {
	tool tools.Tool
	job  Job
	// settled is set when the call has its result without being run.
	settled *transcript.ToolResult
}

// Run runs calls and returns a result for every one of them, whatever
// happens: an unknown tool, a refused argument, a rejected permission, a
// reached limit and a cancelled turn each give a result that says so.
//
// Calls that conflict (see Conflicts) run in the order the model wrote
// them, and the others run at the same time. A call is handed to its tool
// at most once. Run returns when every tool has returned, also after ctx
// ends: a tool is trusted to stop when its context does.
func (d Dispatcher) Run(ctx context.Context, calls []transcript.ToolCall) Dispatched {
	out := Dispatched{Results: make([]transcript.ToolResult, len(calls))}
	plan := make([]planned, len(calls))
	settle := func(i int, outcome transcript.Outcome, text string) {
		r := result(calls[i], outcome, text)
		plan[i].settled = &r
	}

	// Decide in the model's order which calls run. Permission is asked one
	// call at a time, so the user answers them in that order.
	for i, call := range calls {
		tool, ok := d.Tools.Lookup(call.Name)
		if !ok {
			settle(i, transcript.OutcomeFailed, fmt.Sprintf(textUnknownTool, call.Name))
			continue
		}
		desc, err := tool.Describe(call.Input)
		if err != nil {
			settle(i, transcript.OutcomeFailed, fmt.Sprintf(textBadInput, err))
			continue
		}
		if d.Limited && out.Ran >= d.Left {
			out.OverLimit = true
			settle(i, transcript.OutcomeCancelled, fmt.Sprintf(textLimit, d.LimitKey))
			continue
		}
		if out.Err != nil || ctx.Err() != nil {
			settle(i, transcript.OutcomeCancelled, textInterrupted)
			continue
		}
		if d.Permit != nil {
			kind := desc.Kind
			allowed, err := d.Permit(ctx, call.Name, tool.Effect, acp.ToolCallUpdate{
				ToolCallId: acp.ToolCallId(call.ID), Title: &desc.Title, Kind: &kind,
				Locations: desc.Locations, RawInput: call.Input,
			})
			switch {
			case err != nil && ctx.Err() == nil:
				out.Err = fmt.Errorf("loop: asking permission for %s: %w", call.Name, err)
				settle(i, transcript.OutcomeCancelled, textInterrupted)
				continue
			case err != nil:
				settle(i, transcript.OutcomeCancelled, textInterrupted)
				continue
			case !allowed:
				settle(i, transcript.OutcomeCancelled, textRejected)
				continue
			}
		}
		plan[i].tool, plan[i].job = tool, JobFor(tool.Effect, desc)
		out.Ran++
	}
	if out.Err != nil {
		// The turn is stopping, so the calls allowed before the failure
		// do not start.
		out.Ran = 0
		for i := range plan {
			if plan[i].settled == nil {
				settle(i, transcript.OutcomeCancelled, textInterrupted)
			}
		}
	}

	// A settled call takes no part in scheduling: it conflicts with nothing.
	jobs := make([]Job, len(calls))
	for i := range plan {
		if plan[i].settled == nil {
			jobs[i] = plan[i].job
		}
	}
	runAll(Waits(jobs), func(i int) {
		if plan[i].settled != nil {
			out.Results[i] = *plan[i].settled
			d.finish(out.Results[i], nil)
			return
		}
		res := d.runOne(ctx, calls[i], plan[i].tool)
		out.Results[i] = transcript.ToolResult{CallID: calls[i].ID, Name: calls[i].Name, Parts: res.Parts, Outcome: res.Outcome}
		d.finish(out.Results[i], res.Content)
	})
	return out
}

// runOne hands one call to its tool, unless the turn has ended while the
// call waited for its turn.
func (d Dispatcher) runOne(ctx context.Context, call transcript.ToolCall, tool tools.Tool) tools.Result {
	if ctx.Err() != nil {
		return tools.Result{Outcome: transcript.OutcomeCancelled, Parts: []transcript.Part{{Text: textInterrupted}}}
	}
	id := acp.ToolCallId(call.ID)
	running := acp.ToolCallStatusInProgress
	d.Update(acp.SessionToolCallUpdate{ToolCallId: id, Status: &running})

	env := d.Env
	env.PutBlob = d.PutBlob
	// A tool may report progress from more than one goroutine.
	var mu sync.Mutex
	var shown []acp.ToolCallContent
	env.Progress = func(_ context.Context, content []acp.ToolCallContent) error {
		mu.Lock()
		defer mu.Unlock()
		shown = append(shown, content...)
		d.Update(acp.SessionToolCallUpdate{ToolCallId: id, Content: slices.Clone(shown)})
		return nil
	}
	res, err := tool.Run(ctx, tools.Call{ID: call.ID, Input: call.Input, Env: env})
	cancelled := ctx.Err() != nil
	if err != nil && !cancelled && d.Failed != nil {
		d.Failed(call, err)
	}
	return Settle(tool.Effect, res, err, cancelled)
}

func (d Dispatcher) finish(result transcript.ToolResult, content []acp.ToolCallContent) {
	update, err := ToolResult(result, content)
	if err != nil {
		// Every result made here has one of the four outcomes.
		panic(err)
	}
	d.Update(update)
}
