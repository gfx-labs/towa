package loop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/hooks"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/repair"
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
	switch {
	case runErr == nil:
		if _, err := Status(res.Outcome); err != nil {
			return textResult(transcript.OutcomeFailed, textNoOutcome)
		}
		return res
	case cancelled && effect == tools.ReadOnly:
		return textResult(transcript.OutcomeCancelled, textReadStopped)
	case cancelled:
		return textResult(transcript.OutcomeUnknown, textUnknown)
	default:
		return textResult(transcript.OutcomeFailed, fmt.Sprintf(textHarnessError, runErr))
	}
}

func textResult(outcome transcript.Outcome, text string) tools.Result {
	return tools.Result{Outcome: outcome, Parts: []transcript.Part{{Text: text}}}
}

// JobFor is what the scheduler needs to know about a call. A call is taken
// to read the paths it shows the client as locations.
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

// Schemas compiles the input schema of every tool of a set that has one,
// by tool name. A tool with only a native definition has none, and its
// input is not validated.
func Schemas(set tools.Set) (map[string]*repair.Schema, error) {
	out := map[string]*repair.Schema{}
	for _, tool := range set.Tools {
		if len(tool.Spec.InputSchema) == 0 {
			continue
		}
		schema, err := repair.Compile(tool.Spec.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("loop: input schema of tool %s: %w", tool.Spec.Name, err)
		}
		out[tool.Spec.Name] = schema
	}
	return out, nil
}

// Dispatcher runs the tool calls of one model response. Tools, Env, Update
// and Logger are required.
type Dispatcher struct {
	Tools tools.Set
	// Schemas is what Schemas returns for Tools.
	Schemas map[string]*repair.Schema
	// Repair is tools.repair.enabled.
	Repair bool
	Env    tools.Env
	// Left is how many calls may still be dispatched, when Limited is set.
	// LimitKey is the config key named to the model for a call over it.
	Left     int64
	Limited  bool
	LimitKey string
	// Permit is Turn.Permit. Nil allows every call.
	Permit func(ctx context.Context, tool string, effect tools.Effect, call acp.ToolCallUpdate) (bool, error)
	// Hooks runs the pre_tool and post_tool hooks. Nil runs none.
	Hooks *hooks.Runner
	// Update sends an update for a call. It may be called from several
	// goroutines at once.
	Update func(update acp.SessionToolCallUpdate)
	// PutBlob is tools.Env.PutBlob for this session.
	PutBlob func(ctx context.Context, mimeType string, content io.Reader) (transcript.Stored, error)
	Logger  *slog.Logger
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
	// input is the argument the tool is run with.
	input []byte
	job   Job
	// settled is set when the call has its result without being run.
	settled *tools.Result
}

// Run runs calls and returns a result for every one of them, whatever
// happens: an unknown tool, a refused argument, a rejected permission, a
// denying hook, a reached limit and a cancelled turn each give a result
// that says so.
//
// Calls that conflict (see Conflicts) run in the order the model wrote
// them, and the others run at the same time. A call is handed to its tool
// at most once. Run returns when every tool has returned, also after ctx
// ends: a tool is trusted to stop when its context does.
func (d Dispatcher) Run(ctx context.Context, calls []transcript.ToolCall) Dispatched {
	out := Dispatched{Results: make([]transcript.ToolResult, len(calls))}
	plan := make([]planned, len(calls))
	// Which calls run is decided in the model's order. Permission is asked
	// one call at a time, so the user answers in that order.
	for i, call := range calls {
		plan[i] = d.prepare(ctx, call, &out)
	}
	if out.Err != nil {
		// The turn is stopping, so the calls allowed before the failure
		// do not start.
		out.Ran = 0
		for i := range plan {
			if plan[i].settled == nil {
				stopped := textResult(transcript.OutcomeCancelled, textInterrupted)
				plan[i].settled = &stopped
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
		var res tools.Result
		if plan[i].settled != nil {
			res = *plan[i].settled
		} else {
			res = d.runOne(ctx, calls[i], plan[i])
		}
		out.Results[i] = transcript.ToolResult{CallID: calls[i].ID, Name: calls[i].Name, Parts: res.Parts, Outcome: res.Outcome}
		update, err := ToolResult(out.Results[i], res.Content)
		if err != nil {
			// Settle and textResult only give the four outcomes.
			panic(err)
		}
		d.Update(update)
	})
	return out
}

// prepare decides whether one call runs, and with which input.
func (d Dispatcher) prepare(ctx context.Context, call transcript.ToolCall, out *Dispatched) planned {
	settle := func(outcome transcript.Outcome, text string) planned {
		res := textResult(outcome, text)
		return planned{settled: &res}
	}
	tool, ok := d.Tools.Lookup(call.Name)
	if !ok {
		return settle(transcript.OutcomeFailed, fmt.Sprintf(textUnknownTool, call.Name))
	}
	input, desc, err := d.accept(ctx, tool, call, call.Input)
	if err != nil {
		return settle(transcript.OutcomeFailed, fmt.Sprintf(textBadInput, err))
	}
	if d.Limited && out.Ran >= d.Left {
		out.OverLimit = true
		return settle(transcript.OutcomeCancelled, fmt.Sprintf(textLimit, d.LimitKey))
	}
	if out.Err != nil || ctx.Err() != nil {
		return settle(transcript.OutcomeCancelled, textInterrupted)
	}
	if d.Permit != nil {
		kind := desc.Kind
		allowed, err := d.Permit(ctx, call.Name, tool.Effect, acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId(call.ID), Title: &desc.Title, Kind: &kind,
			Locations: desc.Locations, RawInput: input,
		})
		switch {
		case err != nil:
			if ctx.Err() == nil {
				out.Err = fmt.Errorf("loop: asking permission for %s: %w", call.Name, err)
			}
			return settle(transcript.OutcomeCancelled, textInterrupted)
		case !allowed:
			return settle(transcript.OutcomeCancelled, textRejected)
		}
	}
	if d.Hooks != nil {
		decision, err := d.Hooks.PreTool(ctx, d.session(), call.ID, call.Name, input)
		d.hookFailed(ctx, err)
		switch {
		case ctx.Err() != nil:
			return settle(transcript.OutcomeCancelled, textInterrupted)
		case decision.Action != hooks.Allow:
			return settle(transcript.OutcomeFailed, decision.Reason)
		case !slices.Equal(decision.Input, input):
			// A hook's input is checked as the model's is. One that is
			// refused fails the call: the old input is not run instead.
			input, desc, err = d.accept(ctx, tool, call, decision.Input)
			if err != nil {
				return settle(transcript.OutcomeFailed, fmt.Sprintf(textBadHookInput, err))
			}
		}
	}
	out.Ran++
	return planned{tool: tool, input: input, job: JobFor(tool.Effect, desc)}
}

// accept repairs and validates an input and describes the call it makes.
func (d Dispatcher) accept(ctx context.Context, tool tools.Tool, call transcript.ToolCall, input []byte) ([]byte, tools.Description, error) {
	if schema := d.Schemas[call.Name]; schema != nil {
		fixed, repairs, err := schema.Apply(input, d.Repair)
		if err != nil {
			return nil, tools.Description{}, err
		}
		if len(repairs) > 0 {
			d.Logger.InfoContext(ctx, "tool input repaired", "session", d.Env.SessionID, "tool", call.Name, "call", call.ID, "repairs", repairs)
		}
		input = fixed
	}
	desc, err := tool.Describe(input)
	return input, desc, err
}

func (d Dispatcher) session() hooks.Session {
	return hooks.Session{ID: string(d.Env.SessionID), Cwd: d.Env.Cwd}
}

// hookFailed logs a hook that failed. Its decision already denies.
func (d Dispatcher) hookFailed(ctx context.Context, err error) {
	logHook(ctx, d.Logger, d.Env.SessionID, err)
}

func logHook(ctx context.Context, logger *slog.Logger, session acp.SessionId, err error) {
	if err == nil {
		return
	}
	var herr *hooks.Error
	if errors.As(err, &herr) {
		logger.ErrorContext(ctx, "hook failed", "session", session, "kind", herr.Kind, "event", herr.Event, "err", err)
		return
	}
	logger.ErrorContext(ctx, "hook failed", "session", session, "err", err)
}

// runOne hands one call to its tool, unless the turn has ended while the
// call waited for its turn.
func (d Dispatcher) runOne(ctx context.Context, call transcript.ToolCall, p planned) tools.Result {
	if ctx.Err() != nil {
		return textResult(transcript.OutcomeCancelled, textInterrupted)
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
	res, err := p.tool.Run(ctx, tools.Call{ID: call.ID, Input: p.input, Env: env})
	cancelled := ctx.Err() != nil
	if err != nil && !cancelled {
		d.Logger.ErrorContext(ctx, "tool could not be run", "session", d.Env.SessionID, "tool", call.Name, "call", call.ID, "err", err)
	}
	res = Settle(p.tool.Effect, res, err, cancelled)
	// After a cancel the hook could not run to its end, and its failure
	// would replace an outcome that must stay as it is.
	if d.Hooks == nil || cancelled {
		return res
	}
	decision, err := d.Hooks.PostTool(ctx, d.session(), call.ID, call.Name, p.input, res.Outcome, res.Parts)
	if ctx.Err() != nil {
		return res
	}
	d.hookFailed(ctx, err)
	if decision.Action != hooks.Allow {
		return textResult(transcript.OutcomeFailed, decision.Reason)
	}
	return res
}
