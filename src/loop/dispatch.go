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
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/hooks"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/batch"
	"github.com/gfx-labs/acpharness/src/tools/gate"
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

// NotRunError is a call that was refused before it reached its tool: an
// unknown tool, an argument that was not accepted, a rejected permission, a
// denying hook, a reached limit, a cancelled turn. A batch gets it from its
// dispatcher's Plan.
type NotRunError struct {
	Outcome transcript.Outcome
	// Text is what the model is told.
	Text string
}

func (e *NotRunError) Error() string {
	return fmt.Sprintf("tool call not run (%s): %s", e.Outcome, e.Text)
}

// Dispatcher runs the tool calls of one model response. Tools, Env, Update
// and Logger are required.
type Dispatcher struct {
	Tools tools.Set
	// Schemas is what Schemas returns for Tools.
	Schemas map[string]*repair.Schema
	// Repair is tools.repair.enabled.
	Repair bool
	// Gate is tools.gate: a result over its budget is withheld from the
	// model unless the call accepted a large output.
	Gate config.ToolsGate
	Env  tools.Env
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
	// Ran is how many calls were dispatched: handed to their tool. A call
	// inside a batch counts, and so does the batch.
	Ran int64
	// OverLimit says a call was not run because the limit was reached.
	OverLimit bool
	// Err is why the turn must stop, when asking for permission failed.
	Err error
}

// budget is what the calls of one response share: how many may still be
// dispatched and whether the turn has to stop. Calls inside a batch are
// decided on the batch's goroutine, so it is locked.
type budget struct {
	mu        sync.Mutex
	left      int64
	limited   bool
	ran       int64
	overLimit bool
	err       error
}

// take counts one dispatched call. It reports false when the limit is used
// up.
func (b *budget) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limited && b.ran >= b.left {
		b.overLimit = true
		return false
	}
	b.ran++
	return true
}

func (b *budget) full() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limited && b.ran >= b.left {
		b.overLimit = true
		return true
	}
	return false
}

func (b *budget) stop(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err == nil {
		b.err = err
	}
}

func (b *budget) stopped() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

// planned is a call that was allowed, ready for its tool.
type planned struct {
	tool tools.Tool
	// input is the argument the tool is run with.
	input []byte
	// accept says the call asked for its output whatever its size.
	accept bool
	job    Job
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
	b := &budget{left: d.Left, limited: d.Limited}
	plan := make([]planned, len(calls))
	refused := make([]*NotRunError, len(calls))
	// Which calls run is decided in the model's order. Permission is asked
	// one call at a time, so the user answers in that order.
	for i, call := range calls {
		plan[i], refused[i] = d.decide(ctx, b, call.ID, call.Name, call.Input)
	}
	if b.stopped() != nil {
		// The turn is stopping, so the calls allowed before the failure
		// do not start.
		b.ran = 0
		for i := range refused {
			if refused[i] == nil {
				refused[i] = &NotRunError{Outcome: transcript.OutcomeCancelled, Text: textInterrupted}
			}
		}
	}

	// A refused call takes no part in scheduling: it conflicts with nothing.
	jobs := make([]Job, len(calls))
	for i := range plan {
		if refused[i] == nil {
			jobs[i] = plan[i].job
		}
	}
	runAll(Waits(jobs), func(i int) {
		var res tools.Result
		if refused[i] != nil {
			res = textResult(refused[i].Outcome, refused[i].Text)
		} else {
			res = d.runOne(ctx, b, calls[i], plan[i])
		}
		out.Results[i] = transcript.ToolResult{CallID: calls[i].ID, Name: calls[i].Name, Parts: res.Parts, Outcome: res.Outcome}
		update, err := ToolResult(out.Results[i], res.Content)
		if err != nil {
			// Settle and textResult only give the four outcomes.
			panic(err)
		}
		d.Update(update)
	})
	out.Ran, out.OverLimit, out.Err = b.ran, b.overLimit, b.err
	return out
}

// decide says whether one call runs, and with which input. id is the id
// the client knows the call by.
func (d Dispatcher) decide(ctx context.Context, b *budget, id, name string, raw []byte) (planned, *NotRunError) {
	refuse := func(outcome transcript.Outcome, text string) (planned, *NotRunError) {
		return planned{}, &NotRunError{Outcome: outcome, Text: text}
	}
	tool, ok := d.Tools.Lookup(name)
	if !ok {
		return refuse(transcript.OutcomeFailed, fmt.Sprintf(textUnknownTool, name))
	}
	input, accept, desc, err := d.accept(ctx, tool, id, raw)
	if err != nil {
		return refuse(transcript.OutcomeFailed, fmt.Sprintf(textBadInput, err))
	}
	if b.full() {
		return refuse(transcript.OutcomeCancelled, fmt.Sprintf(textLimit, d.LimitKey))
	}
	if b.stopped() != nil || ctx.Err() != nil {
		return refuse(transcript.OutcomeCancelled, textInterrupted)
	}
	if d.Permit != nil {
		kind := desc.Kind
		allowed, err := d.Permit(ctx, name, tool.Effect, acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId(id), Title: &desc.Title, Kind: &kind,
			Locations: desc.Locations, RawInput: input,
		})
		switch {
		case err != nil:
			if ctx.Err() == nil {
				b.stop(fmt.Errorf("loop: asking permission for %s: %w", name, err))
			}
			return refuse(transcript.OutcomeCancelled, textInterrupted)
		case !allowed:
			return refuse(transcript.OutcomeCancelled, textRejected)
		}
	}
	if d.Hooks != nil {
		decision, err := d.Hooks.PreTool(ctx, d.session(), id, name, input)
		logHook(ctx, d.Logger, d.Env.SessionID, err)
		switch {
		case ctx.Err() != nil:
			return refuse(transcript.OutcomeCancelled, textInterrupted)
		case decision.Action != hooks.Allow:
			return refuse(transcript.OutcomeFailed, decision.Reason)
		case !slices.Equal(decision.Input, input):
			// A hook's input is checked as the model's is. One that is
			// refused fails the call: the old input is not run instead.
			var hookAccept bool
			input, hookAccept, desc, err = d.accept(ctx, tool, id, decision.Input)
			if err != nil {
				return refuse(transcript.OutcomeFailed, fmt.Sprintf(textBadHookInput, err))
			}
			// The hook saw the input without the member, so it cannot
			// have taken the model's choice away.
			accept = accept || hookAccept
		}
	}
	// Two calls of one batch can pass the check above together, so the
	// count is taken here and can still say no.
	if !b.take() {
		return refuse(transcript.OutcomeCancelled, fmt.Sprintf(textLimit, d.LimitKey))
	}
	return planned{tool: tool, input: input, accept: accept, job: JobFor(tool.Effect, desc)}, nil
}

// accept repairs and validates an input, takes the gate's member out of it
// and describes the call it makes.
func (d Dispatcher) accept(ctx context.Context, tool tools.Tool, id string, input []byte) ([]byte, bool, tools.Description, error) {
	if schema := d.Schemas[tool.Spec.Name]; schema != nil {
		fixed, repairs, err := schema.Apply(input, d.Repair)
		if err != nil {
			return nil, false, tools.Description{}, err
		}
		if len(repairs) > 0 {
			d.Logger.InfoContext(ctx, "tool input repaired", "session", d.Env.SessionID, "tool", tool.Spec.Name, "call", id, "repairs", repairs)
		}
		input = fixed
	}
	// After repair, so that "true" sent as a string was repaired there.
	rest, accept, err := gate.Split(input)
	if err != nil {
		return nil, false, tools.Description{}, err
	}
	desc, err := tool.Describe(rest)
	return rest, accept, desc, err
}

func (d Dispatcher) session() hooks.Session {
	return hooks.Session{ID: string(d.Env.SessionID), Cwd: d.Env.Cwd}
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
func (d Dispatcher) runOne(ctx context.Context, b *budget, call transcript.ToolCall, p planned) tools.Result {
	if ctx.Err() != nil {
		return textResult(transcript.OutcomeCancelled, textInterrupted)
	}
	id := acp.ToolCallId(call.ID)
	running := acp.ToolCallStatusInProgress
	d.Update(acp.SessionToolCallUpdate{ToolCallId: id, Status: &running})

	// A tool may report progress from more than one goroutine.
	var mu sync.Mutex
	var shown []acp.ToolCallContent
	progress := func(_ context.Context, content []acp.ToolCallContent) error {
		mu.Lock()
		defer mu.Unlock()
		shown = append(shown, content...)
		d.Update(acp.SessionToolCallUpdate{ToolCallId: id, Content: slices.Clone(shown)})
		return nil
	}
	// A tool that runs other calls, the batch, finds its dispatcher here.
	ctx = context.WithValue(ctx, batchKey{}, d.batch(ctx, b, call))
	return d.execute(ctx, call.ID, call.Name, p, progress)
}

// execute runs an allowed call and gives its result as the model gets it:
// settled, gated and passed by the post_tool hooks.
func (d Dispatcher) execute(
	ctx context.Context, id, name string, p planned,
	progress func(context.Context, []acp.ToolCallContent) error,
) tools.Result {
	env := d.Env
	env.PutBlob = d.PutBlob
	env.Progress = progress
	res, err := p.tool.Run(ctx, tools.Call{ID: id, Input: p.input, Env: env})
	cancelled := ctx.Err() != nil
	if err != nil && !cancelled {
		d.Logger.ErrorContext(ctx, "tool could not be run", "session", d.Env.SessionID, "tool", name, "call", id, "err", err)
	}
	res = Settle(p.tool.Effect, res, err, cancelled)
	// The store write of a gated result must not be lost to a cancel.
	res, summary := gate.Apply(context.WithoutCancel(ctx), env, d.Gate, res, p.accept)
	if summary != nil && summary.StoreErr != nil {
		d.Logger.ErrorContext(ctx, "gated tool output could not be stored", "session", d.Env.SessionID, "tool", name, "call", id, "err", summary.StoreErr)
	}
	// After a cancel the hook could not run to its end, and its failure
	// would replace an outcome that must stay as it is.
	if d.Hooks == nil || cancelled {
		return res
	}
	decision, err := d.Hooks.PostTool(ctx, d.session(), id, name, p.input, res.Outcome, res.Parts)
	if ctx.Err() != nil {
		return res
	}
	logHook(ctx, d.Logger, d.Env.SessionID, err)
	if decision.Action != hooks.Allow {
		return textResult(transcript.OutcomeFailed, decision.Reason)
	}
	return res
}

type batchKey struct{}

// BatchDispatcher returns the dispatcher for the calls of a batch. A tool
// that runs other calls takes it from the context of its Run, so that each
// of them goes through the same repair, validation, permission, hooks,
// limit and gating as a call on its own. It reports false outside a call
// the loop dispatched.
func BatchDispatcher(ctx context.Context) (batch.Dispatcher, bool) {
	d, ok := ctx.Value(batchKey{}).(batch.Dispatcher)
	return d, ok
}

// batch returns the dispatcher for the calls inside parent.
//
// The client is not sent a tool call for a call inside a batch: it sees the
// batch. A permission request for one carries the id "<batch id>/<index>".
func (d Dispatcher) batch(ctx context.Context, b *budget, parent transcript.ToolCall) batch.Dispatcher {
	var mu sync.Mutex
	plans := map[int]planned{}
	id := func(index int) string { return fmt.Sprintf("%s/%d", parent.ID, index) }
	return batch.Dispatcher{
		Plan: func(index int, call batch.Call) (batch.Plan, error) {
			// A batch inside a batch would hide its calls from the
			// scheduler of the outer one.
			if call.Name == parent.Name {
				return batch.Plan{}, &NotRunError{Outcome: transcript.OutcomeFailed, Text: fmt.Sprintf(textNested, parent.Name)}
			}
			p, refused := d.decide(ctx, b, id(index), call.Name, call.Input)
			if refused != nil {
				return batch.Plan{}, refused
			}
			mu.Lock()
			plans[index] = p
			mu.Unlock()
			return batch.Plan{Effect: p.job.Effect, WritePaths: p.job.WritePaths, ReadPaths: p.job.ReadPaths}, nil
		},
		Run: func(ctx context.Context, index int, call batch.Call) (tools.Result, error) {
			mu.Lock()
			p, ok := plans[index]
			// Taken out, so that a second Run of one index cannot hand
			// the call to its tool again.
			delete(plans, index)
			mu.Unlock()
			if !ok {
				return tools.Result{}, fmt.Errorf("loop: call %d of batch %s was not planned, or was already run", index, parent.ID)
			}
			// The inner context has no dispatcher: nothing inside a
			// batch starts another.
			ctx = context.WithValue(ctx, batchKey{}, nil)
			return d.execute(ctx, id(index), call.Name, p, nil), nil
		},
	}
}
