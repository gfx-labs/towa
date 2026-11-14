package loop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
	"uuid"

	acp "github.com/gfx-labs/go-acp"

	"github.com/gfx-labs/towa/src/agent/harness"
	"github.com/gfx-labs/towa/src/agent/hooks"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/prompt/compact"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/provider/router"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/repair"
	"github.com/gfx-labs/towa/src/tools/todo"
	"github.com/gfx-labs/towa/src/transcript"
)

// DeniedError is a prompt a hook denied. The turn did not start. It is the
// Err of a *StopError of kind harness.StopDenied, so a caller may match
// either.
type DeniedError struct {
	// Reason is the hook's reason, for the client.
	Reason string
}

func (e *DeniedError) Error() string { return "prompt denied by a hook: " + e.Reason }

// errWall is the cause of a turn's context ending at limits.max_wall.
var errWall = errors.New("loop: the turn reached " + config.KeyMaxWall)

// turn is the state of one running turn.
type turn struct {
	l       *Loop
	t       Turn
	id      uuid.UUID
	started time.Time
	limits  config.Limits
	// spentMu guards spent: a tool may add to it while others run.
	spentMu sync.Mutex
	spent   Spent
	// final is the text of the last response.
	final string
	// entries is the append-only log, including history hidden by compaction.
	entries []transcript.Entry
	// parent is the id of the last entry stored.
	parent uuid.UUID
	// stopDenials is how often a stop hook kept the turn going.
	stopDenials int64
	// model, mapper and schemas are the turn's resolved model.
	model           Resolved
	compactor       *compact.Controller
	compactionInput *compact.BeforeInput
	mapper          Mapper
	schemas         map[string]*repair.Schema

	// sendMu orders updates from tools that run at the same time.
	sendMu  sync.Mutex
	sendErr error
}

// Run runs one prompt turn. It has the shape of session.TurnFunc.
func (l *Loop) Run(ctx context.Context, t Turn) (Result, error) {
	id, err := uuid.Parse(string(t.SessionID))
	if err != nil {
		return Result{}, fmt.Errorf("loop: session id %q is not a UUID: %w", t.SessionID, err)
	}
	if t.Update == nil {
		return Result{}, errors.New("loop: the turn has no Update")
	}
	tn := &turn{l: l, t: t, id: id, started: l.o.Now(), limits: l.o.Limits}
	if t.Limits != nil {
		tn.limits = *t.Limits
	}
	result, err := tn.run(ctx)
	if l.o.Jobs != nil {
		// No job outlives its turn. The model learns how the ones it was
		// not told about ended at the start of the next turn, and the client
		// before the prompt returns.
		l.o.Jobs.StopAll()
		if err == nil {
			err = tn.reportJobs(ctx)
		}
	}
	result.Final = tn.final
	if result.Usage == nil {
		result.Usage = tn.snapshot().Usage()
	}
	return result, err
}

// run is the turn from its prompt to its end. The deferred compaction work
// reports through out and runErr, so an error from it joins the turn's own.
func (tn *turn) run(ctx context.Context) (out Result, runErr error) {
	if tn.limits.MaxWall > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, tn.limits.MaxWall, errWall)
		defer cancel()
	}
	user, err := UserItem(tn.t.Prompt)
	if err != nil {
		return Result{}, err
	}
	session := hooks.Session{ID: string(tn.t.SessionID), Cwd: tn.t.Cwd}
	if err := tn.admit(ctx, session, user); err != nil {
		return Result{}, err
	}
	if err := tn.resolve(ctx); err != nil {
		return Result{}, StopFor(err)
	}
	if err := tn.startCompactor(ctx); err != nil {
		return Result{}, err
	}
	if tn.compactor != nil {
		defer func() {
			defer tn.compactor.Close()
			if tn.compactionInput == nil {
				return
			}
			if err := tn.finishCompaction(ctx); err != nil {
				runErr = errors.Join(runErr, StopFor(err))
			}
			out.Usage = tn.snapshot().Usage()
		}()
	}
	system, err := tn.begin(ctx, user)
	if err != nil {
		return Result{}, err
	}
	for {
		if result, done, err := tn.step(ctx, session, system); done {
			return result, err
		}
	}
}

// admit runs the prompt hook. Nothing is stored for a denied prompt: the
// model never saw it.
func (tn *turn) admit(ctx context.Context, session hooks.Session, user transcript.Item) error {
	decision, hookErr := tn.l.o.Hooks.Prompt(ctx, session, user.User.Parts)
	logHook(ctx, tn.l.o.Logger, tn.t.SessionID, hookErr)
	if decision.Action != hooks.Allow {
		return &StopError{
			Stop: harness.Stop{Kind: harness.StopDenied, Message: decision.Reason},
			Err:  &DeniedError{Reason: decision.Reason},
		}
	}
	return nil
}

// startCompactor creates the turn's compaction controller. It leaves
// tn.compactor nil when compaction is off.
func (tn *turn) startCompactor(ctx context.Context) error {
	if tn.l.o.NewCompactor == nil {
		return nil
	}
	current := copyTurn(tn.t)
	limits := tn.limits
	current.Limits, current.Update = &limits, tn.update
	compactor, err := tn.l.o.NewCompactor(ctx, current, tn.model)
	if err != nil {
		return fmt.Errorf("loop: creating compaction controller: %w", err)
	}
	if compactor == nil {
		return errors.New("loop: NewCompactor returned no controller")
	}
	tn.compactor = compactor
	return nil
}

// finishCompaction lets the controller end the turn's pending work and
// stores what it produced.
func (tn *turn) finishCompaction(ctx context.Context) error {
	in := *tn.compactionInput
	in.Entries = tn.entries
	effects, err := tn.compactor.Finish(ctx, in)
	if err == nil && len(effects) > 0 {
		err = tn.applyCompaction(ctx, effects)
	}
	return err
}

// begin loads the session, stores the prompt and builds the system prompt.
func (tn *turn) begin(ctx context.Context, user transcript.Item) ([]provider.SystemBlock, error) {
	if err := tn.load(ctx); err != nil {
		return nil, err
	}
	if err := tn.append(ctx, user); err != nil {
		return nil, err
	}
	system, err := tn.l.o.System(ctx, tn.t)
	if err != nil {
		return nil, fmt.Errorf("loop: system prompt: %w", err)
	}
	return system, nil
}

// ended says how the turn ends when its context is over or its model
// request failed. It reports false when the turn goes on.
func (tn *turn) ended(ctx context.Context, streamErr error) (Result, bool, error) {
	switch {
	case ctx.Err() != nil && errors.Is(context.Cause(ctx), errWall):
		if err := tn.closeCalls(ctx); err != nil {
			return Result{}, true, err
		}
		return LimitResult(config.KeyMaxWall, tn.snapshot()), true, nil
	case ctx.Err() != nil:
		if err := tn.closeCalls(ctx); err != nil {
			return Result{}, true, err
		}
		// The model is told about the interruption when the next turn's
		// request is built.
		if err := tn.append(ctx, transcript.Item{Interrupted: &transcript.Interrupted{}}); err != nil {
			return Result{}, true, err
		}
		return tn.result(acp.StopReasonCancelled), true, nil
	case streamErr != nil:
		if limit, ok := errors.AsType[*turnLimitError](streamErr); ok {
			return LimitResult(limit.key, tn.snapshot()), true, nil
		}
		return Result{}, true, StopFor(streamErr)
	}
	return Result{}, false, nil
}

// closeCalls stores a cancelled result for every call the turn ends before
// dispatching. The turn can end between storing a response that asks for
// tools and running them, and a call with no result that is followed by
// anything else makes the session unreadable for the next turn.
func (tn *turn) closeCalls(ctx context.Context) error {
	answered := map[string]bool{}
	for _, entry := range tn.entries {
		if entry.Item.ToolResult != nil {
			answered[entry.Item.ToolResult.CallID] = true
		}
	}
	var unrun []transcript.Item
	for _, entry := range tn.entries {
		if call := entry.Item.ToolCall; call != nil && !answered[call.ID] {
			cancelled := result(*call, transcript.OutcomeCancelled, textInterrupted)
			unrun = append(unrun, transcript.Item{ToolResult: &cancelled})
			tn.toolUpdate(ctx, cancelled, nil)
		}
	}
	return tn.append(ctx, unrun...)
}

func (tn *turn) result(reason acp.StopReason) Result {
	return Result{StopReason: reason, Usage: tn.snapshot().Usage()}
}

// resolve asks for the turn's model and checks what came back.
func (tn *turn) resolve(ctx context.Context) error {
	model, err := tn.l.o.Resolve(ctx, tn.t.Model)
	if err != nil {
		return fmt.Errorf("loop: resolving model %q: %w", tn.t.Model, err)
	}
	switch {
	case model.Provider == nil:
		return fmt.Errorf("loop: model %q resolved without a provider", tn.t.Model)
	case model.Interruption == "":
		return fmt.Errorf("loop: model %q resolved without an interruption text", tn.t.Model)
	case model.ContextSize < 0 || model.MaxOutputTokens < 0:
		return fmt.Errorf("loop: model %q resolved with a negative context size or output limit", tn.t.Model)
	}
	schemas := model.Schemas
	if schemas == nil {
		if schemas, err = Schemas(model.Tools); err != nil {
			return err
		}
	}
	tn.model, tn.schemas = model, schemas
	tn.mapper = Mapper{Tools: model.Tools}
	return nil
}

// step runs one pass of the turn: a model request and what follows from its
// response. It reports false when the turn goes on.
func (tn *turn) step(ctx context.Context, session hooks.Session, system []provider.SystemBlock) (Result, bool, error) {
	if result, done, err := tn.ended(ctx, nil); done {
		return result, true, err
	}
	tn.spent.Wall = tn.l.o.Now().Sub(tn.started)
	if key, over := Exceeded(tn.limits, tn.snapshot()); over {
		return LimitResult(key, tn.snapshot()), true, nil
	}
	if err := tn.reportJobs(ctx); err != nil {
		return Result{}, true, err
	}

	resp, streamErr := tn.stream(ctx, system)
	for retries := 0; ; retries++ {
		move, again := tn.resend(ctx, resp, streamErr, retries)
		if !again {
			break
		}
		if err := tn.discard(ctx, resp); err != nil {
			return Result{}, true, err
		}
		tn.moved(ctx, move)
		if err := sleep(ctx, move.Wait); err != nil {
			resp, streamErr = &Response{}, err
			break
		}
		resp, streamErr = tn.stream(ctx, system)
	}
	tn.final = resp.Text()
	if err := tn.record(ctx, resp, streamErr); err != nil {
		return Result{}, true, err
	}
	if result, done, err := tn.ended(ctx, streamErr); done {
		return result, true, err
	}
	switch resp.Stop {
	case provider.StopPaused:
		// The provider carries on from what it is sent back, so the
		// conversation goes as it is. The next request is counted
		// against the limits at the top like any other.
		return Result{}, false, nil
	case provider.StopToolUse:
		return tn.dispatch(ctx, resp.Calls())
	}
	return tn.stopped(ctx, session, resp.Stop)
}

// resend says whether a response the provider cut off after it began is
// sent again, and the move to report. The router retries only until an event
// arrives, so past that the turn applies the same policy, as to the last
// entry of a chain.
func (tn *turn) resend(ctx context.Context, resp *Response, streamErr error, retries int) (router.Move, bool) {
	perr, ok := errors.AsType[*provider.Error](streamErr)
	if !ok || !resp.arrived || ctx.Err() != nil {
		return router.Move{}, false
	}
	decision := router.Decide(tn.limits.Retry, perr, router.Attempt{
		Retries: retries, Entry: 0, Entries: 1, Now: tn.l.o.Now(), Rand: rand.Float64(),
	})
	if decision.Action != router.Again {
		return router.Move{}, false
	}
	var entry router.Entry
	if failed, ok := errors.AsType[*router.Failed](streamErr); ok {
		entry = failed.Entry
	}
	return router.Move{Kind: router.MoveRetry, From: entry, To: entry, Wait: decision.Wait, Err: perr}, true
}

// discard drops a cut-off response before the request is sent again. Its
// items are not stored, so the model never sees them. The client is told
// that each call it was shown did not run, and the usage still counts.
func (tn *turn) discard(ctx context.Context, resp *Response) error {
	for _, call := range resp.Calls() {
		tn.notRun(ctx, call.ID)
	}
	for _, id := range resp.Writing() {
		tn.notRun(ctx, id)
	}
	if resp.Usage != nil {
		tn.add(*resp.Usage)
		if update, ok := tn.mapper.Usage(*resp.Usage); ok {
			tn.send(ctx, acp.SessionUpdate{UsageUpdate: &update})
		}
		if err := tn.append(ctx, transcript.Item{Usage: resp.Usage}); err != nil {
			return err
		}
	}
	return tn.sendErr
}

// notRun tells the client that a call it was shown did not run.
func (tn *turn) notRun(ctx context.Context, id string) {
	failed := acp.ToolCallStatusFailed
	tn.send(ctx, acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
		ToolCallId: acp.ToolCallId(id), Status: &failed,
		Content: []acp.ToolCallContent{tools.TextContent(textNotRun)},
	}})
}

// sleep waits for d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// record tells the client how a response ended and stores it.
func (tn *turn) record(ctx context.Context, resp *Response, streamErr error) error {
	stored := resp.Items
	if streamErr != nil || resp.Stop != provider.StopToolUse {
		// The calls of a response that failed, was cut off or ended
		// for another reason are not run. They still get a result.
		for _, call := range resp.Calls() {
			unrun := result(call, transcript.OutcomeCancelled, textNotRun)
			stored = append(stored, transcript.Item{ToolResult: &unrun})
			tn.toolUpdate(ctx, unrun, nil)
		}
	}
	// A call the response announced and never finished is not an item
	// and is not stored. The client is still told that it ended.
	for _, id := range resp.Writing() {
		tn.notRun(ctx, id)
	}
	if resp.Usage != nil {
		tn.add(*resp.Usage)
		stored = append(stored, transcript.Item{Usage: resp.Usage})
		if update, ok := tn.mapper.Usage(*resp.Usage); ok {
			tn.send(ctx, acp.SessionUpdate{UsageUpdate: &update})
		}
	}
	// The calls are durable before any of them is dispatched, so a
	// process that dies here leaves a call without a result, which the
	// next turn records as unknown and does not run.
	if err := tn.append(ctx, stored...); err != nil {
		return err
	}
	return tn.sendErr
}

// stopped handles a response the model ended: the stop hook may send the
// turn back to work.
func (tn *turn) stopped(ctx context.Context, session hooks.Session, stop provider.StopReason) (Result, bool, error) {
	reason, ok := StopReason(stop)
	if !ok {
		return Result{}, true, fmt.Errorf("loop: the response ended with the stop reason %q", stop)
	}
	decision, hookErr := tn.l.o.Hooks.Stop(ctx, session, string(reason))
	if result, done, err := tn.ended(ctx, nil); done {
		return result, true, err
	}
	logHook(ctx, tn.l.o.Logger, tn.t.SessionID, hookErr)
	if decision.Action == hooks.Allow {
		if reason == acp.StopReasonEndTurn && tn.l.o.Jobs != nil {
			return tn.awaitJobs(ctx, reason)
		}
		return tn.pokeTodos(ctx, reason)
	}
	// A hook that always denies must not run the turn forever.
	if tn.stopDenials >= tn.limits.MaxStopDenials {
		return LimitResult(config.KeyMaxStopDenials, tn.snapshot()), true, nil
	}
	tn.stopDenials++
	return tn.continueWith(ctx, decision.Reason)
}

// awaitJobs keeps a turn the model ended going while its background
// commands run: it waits for the next to end, and the next request tells
// the model how it ended.
func (tn *turn) awaitJobs(ctx context.Context, reason acp.StopReason) (Result, bool, error) {
	if err := tn.l.o.Jobs.Next(ctx); err != nil {
		if result, done, endErr := tn.ended(ctx, nil); done {
			return result, true, endErr
		}
		return Result{}, true, err
	}
	if !tn.l.o.Jobs.Unreported() {
		return tn.pokeTodos(ctx, reason)
	}
	return Result{}, false, nil
}

// reportJobs tells the model how the background commands it was not told
// about ended.
func (tn *turn) reportJobs(ctx context.Context) error {
	if tn.l.o.Jobs == nil {
		return nil
	}
	ended := tn.l.o.Jobs.Take()
	if len(ended) == 0 {
		return nil
	}
	reports := make([]string, len(ended))
	for i, job := range ended {
		outcome, report := job.Report()
		reports[i] = report
		// The call that started the job ended when it started; the client
		// is shown how the job ended under it.
		status, err := Status(outcome)
		if err != nil {
			return err
		}
		tn.send(ctx, acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
			ToolCallId: acp.ToolCallId(job.CallID), Status: &status,
			Content: []acp.ToolCallContent{tools.TextContent(report)},
		}})
	}
	return tn.append(ctx, userText(jobsPrefix+strings.Join(reports, "\n\n")))
}

// pokeTodos ends the turn, unless it ended by itself with todo items
// unfinished: then it is sent back to them, a bounded number of times.
func (tn *turn) pokeTodos(ctx context.Context, reason acp.StopReason) (Result, bool, error) {
	if reason != acp.StopReasonEndTurn || tn.limits.MaxTodoPokes == 0 {
		return tn.result(reason), true, nil
	}
	// Without the tool the model cannot finish or drop an item.
	if _, ok := tn.model.Tools.Lookup(todo.Name); !ok {
		return tn.result(reason), true, nil
	}
	open, sent, err := todo.Open(tn.entries)
	if err != nil {
		return Result{}, true, err
	}
	if len(open) == 0 || int64(sent) >= tn.limits.MaxTodoPokes {
		return tn.result(reason), true, nil
	}
	return tn.continueWith(ctx, todo.Poke(open))
}

// continueWith stores text as a user message and keeps the turn going.
func (tn *turn) continueWith(ctx context.Context, text string) (Result, bool, error) {
	if err := tn.append(ctx, userText(text)); err != nil {
		return Result{}, true, err
	}
	return Result{}, false, nil
}

// dispatch runs the calls of a response that stopped for tool use and
// stores their results.
func (tn *turn) dispatch(ctx context.Context, calls []transcript.ToolCall) (Result, bool, error) {
	if len(calls) == 0 {
		return Result{}, true, errors.New("loop: the model stopped for tool use without a tool call")
	}
	left, limited := ToolCallsLeft(tn.limits, tn.snapshot())
	// A tool that runs a model of its own, a subagent, reports what it
	// used here, so the turn's limits count it.
	toolCtx := tn.toolContext(ctx)
	done := Dispatcher{
		Tools: tn.model.Tools,
		Env:   tools.Env{SessionID: tn.t.SessionID, Cwd: tn.t.Cwd, Dirs: tn.t.Dirs},
		Left:  left, Limited: limited, LimitKey: config.KeyMaxToolCalls,
		Permit: tn.t.Permit,
		Update: func(update acp.SessionToolCallUpdate) {
			tn.send(ctx, acp.SessionUpdate{ToolCallUpdate: &update})
		},
		PutBlob: func(ctx context.Context, mimeType string, content io.Reader) (transcript.Stored, error) {
			return tn.l.o.Store.PutBlob(ctx, tn.id, mimeType, content)
		},
		Schemas: tn.schemas, Repair: tn.l.o.Repair, Gate: tn.l.o.Gate, Hooks: tn.l.o.Hooks, Logger: tn.l.o.Logger,
	}.Run(toolCtx, calls)
	tn.spentMu.Lock()
	tn.spent.ToolCalls += done.Ran
	tn.spentMu.Unlock()
	results := make([]transcript.Item, len(done.Results))
	for i := range done.Results {
		results[i] = transcript.Item{ToolResult: &done.Results[i]}
	}
	if err := tn.append(ctx, results...); err != nil {
		return Result{}, true, err
	}
	switch {
	case tn.sendErr != nil:
		return Result{}, true, tn.sendErr
	case done.Err != nil:
		return Result{}, true, done.Err
	case done.OverLimit:
		return LimitResult(config.KeyMaxToolCalls, tn.snapshot()), true, nil
	}
	return Result{}, false, nil
}
