package loop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/pkg/acp/harness"
	"github.com/gfx-labs/acpharness/src/compact"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/hooks"
	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/provider/router"
	"github.com/gfx-labs/acpharness/src/store"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/repair"
	"github.com/gfx-labs/acpharness/src/tools/todo"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// Options is what a turn is built from. Every field is required unless it
// says otherwise.
type Options struct {
	// Resolve returns what a turn needs from the model the session chose.
	// It is called once per turn, before the first request, with
	// Turn.Model as it is: an empty name means the session chose none, and
	// the resolver owns the default.
	Resolve func(ctx context.Context, model string) (Resolved, error)
	// NewCompactor returns the controller for this turn's session. Nil
	// disables automatic compaction. Close cancels pending work at turn end.
	NewCompactor func(context.Context, Turn, Resolved) (*compact.Controller, error)
	Store        store.Store
	// System returns the system prompt's sections for a turn.
	System func(ctx context.Context, t Turn) ([]provider.SystemBlock, error)
	Limits config.Limits
	// Now is the clock: entry times and the turn's wall time come from it.
	Now func() time.Time
	// Repair is tools.repair.enabled: fix the known ways a model gets a
	// tool's argument wrong before validating it.
	Repair bool
	// Gate is tools.gate: when a tool's result is withheld from the model.
	Gate config.ToolsGate
	// Hooks runs the configured hooks. It may have none.
	Hooks  *hooks.Runner
	Logger *slog.Logger
}

// Resolved is a model as one turn uses it.
type Resolved struct {
	// Provider answers the turn's model requests. It is the router over the
	// model's chain, which sets each request's model and credential.
	Provider provider.Provider
	// Tools is the session's tool set.
	Tools tools.Set
	// Model is the provider's own name for the model, of the chain's first
	// entry. It is for logs: the router names the model of each request.
	Model string
	// ProviderName identifies the first entry's provider for native context.
	// A router supplies the actual entry's ProviderID before each request.
	ProviderName string
	// ContextSize is the model's context window in tokens, for usage
	// updates. Zero means it is not known, and no usage update is sent.
	ContextSize int64
	// MaxOutputTokens and Effort are sent with every request, and not sent
	// when zero.
	MaxOutputTokens int64
	Effort          string
	// Interruption is the text that tells the model the turn before was
	// interrupted. It is required.
	Interruption string
}

// Loop runs prompt turns. It keeps nothing between turns: a session's
// conversation is read from the store when a turn starts.
type Loop struct {
	o Options
}

// New returns a loop over its dependencies.
func New(o Options) (*Loop, error) {
	switch {
	case o.Resolve == nil:
		return nil, errors.New("loop: Resolve is required")
	case o.Store == nil:
		return nil, errors.New("loop: Store is required")
	case o.System == nil:
		return nil, errors.New("loop: System is required")
	case o.Now == nil:
		return nil, errors.New("loop: Now is required")
	case o.Hooks == nil:
		return nil, errors.New("loop: Hooks is required")
	case o.Logger == nil:
		return nil, errors.New("loop: Logger is required")
	}
	return &Loop{o: o}, nil
}

// DeniedError is a prompt a hook denied. The turn did not start. It is the
// Err of a *StopError of kind harness.StopDenied, so a caller may match
// either.
type DeniedError struct {
	// Reason is the hook's reason, for the client.
	Reason string
}

func (e *DeniedError) Error() string { return "prompt denied by a hook: " + e.Reason }

// StopReason is the ACP stop reason for a response the model ended. It
// reports false for tool use, which does not end a turn, and for a reason
// outside provider's list.
func StopReason(stop provider.StopReason) (acp.StopReason, bool) {
	switch stop {
	case provider.StopEndTurn:
		return acp.StopReasonEndTurn, true
	case provider.StopMaxTokens:
		return acp.StopReasonMaxTokens, true
	case provider.StopRefusal:
		return acp.StopReasonRefusal, true
	}
	return "", false
}

// errWall is the cause of a turn's context ending at limits.max_wall.
var errWall = errors.New("loop: the turn reached " + config.KeyMaxWall)

// Response collects one model response from its events.
type Response struct {
	// Items are the finished items, in order.
	Items []transcript.Item
	// Usage is the last usage reported, nil when none was.
	Usage *transcript.Usage
	// Stop is empty until the last event.
	Stop provider.StopReason

	// textShown and thoughtShown say a delta was sent since the last
	// finished item of that kind.
	textShown, thoughtShown bool
}

// Add takes one event and returns the updates that show it to the client.
// A finished text or thinking item is shown only when no delta came for it,
// since its deltas already showed it.
func (r *Response) Add(m Mapper, e provider.Event) []acp.SessionUpdate {
	switch {
	case e.TextDelta != "":
		r.textShown = true
		return []acp.SessionUpdate{TextChunk(e.TextDelta)}
	case e.ThinkingDelta != "":
		r.thoughtShown = true
		return []acp.SessionUpdate{ThoughtChunk(e.ThinkingDelta)}
	case e.Item != nil:
		item := *e.Item
		r.Items = append(r.Items, item)
		switch {
		case item.Text != nil:
			shown := r.textShown
			r.textShown = false
			if !shown && item.Text.Text != "" {
				return []acp.SessionUpdate{TextChunk(item.Text.Text)}
			}
		case item.Thinking != nil:
			shown := r.thoughtShown
			r.thoughtShown = false
			if !shown && item.Thinking.Text != "" {
				return []acp.SessionUpdate{ThoughtChunk(item.Thinking.Text)}
			}
		case item.ToolCall != nil:
			call := m.ToolCall(*item.ToolCall)
			return []acp.SessionUpdate{{ToolCall: &call}}
		case item.Native != nil:
			return HostedSearch(item)
		}
	case e.Usage != nil:
		r.Usage = e.Usage
	case e.Stop != "":
		r.Stop = e.Stop
	}
	return nil
}

// Text returns the response's text items joined.
func (r *Response) Text() string {
	var text strings.Builder
	for _, item := range r.Items {
		if item.Text != nil {
			text.WriteString(item.Text.Text)
		}
	}
	return text.String()
}

// Calls returns the response's tool calls in order.
func (r *Response) Calls() []transcript.ToolCall {
	var calls []transcript.ToolCall
	for _, item := range r.Items {
		if item.ToolCall != nil {
			calls = append(calls, *item.ToolCall)
		}
	}
	return calls
}

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
	result.Final = tn.final
	if result.Usage == nil {
		result.Usage = tn.snapshot().Usage()
	}
	return result, err
}

func (tn *turn) run(ctx context.Context) (out Result, runErr error) {
	l, t, id := tn.l, tn.t, tn.id
	if tn.limits.MaxWall > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, tn.limits.MaxWall, errWall)
		defer cancel()
	}
	user, err := UserItem(t.Prompt)
	if err != nil {
		return Result{}, err
	}
	hookSession := hooks.Session{ID: string(t.SessionID), Cwd: t.Cwd}
	decision, hookErr := l.o.Hooks.Prompt(ctx, hookSession, user.User.Parts)
	logHook(ctx, l.o.Logger, t.SessionID, hookErr)
	if decision.Action != hooks.Allow {
		// Nothing is stored: the model never saw the prompt.
		return Result{}, &StopError{
			Stop: harness.Stop{Kind: harness.StopDenied, Message: decision.Reason},
			Err:  &DeniedError{Reason: decision.Reason},
		}
	}
	if err := tn.resolve(ctx); err != nil {
		return Result{}, StopFor(err)
	}
	if l.o.NewCompactor != nil {
		current := copyTurn(t)
		limits := tn.limits
		current.Limits, current.Update = &limits, tn.update
		tn.compactor, err = l.o.NewCompactor(ctx, current, tn.model)
		if err != nil {
			return Result{}, fmt.Errorf("loop: creating compaction controller: %w", err)
		}
		if tn.compactor == nil {
			return Result{}, errors.New("loop: NewCompactor returned no controller")
		}
		defer func() {
			defer tn.compactor.Close()
			if tn.compactionInput == nil {
				return
			}
			in := *tn.compactionInput
			in.Entries = tn.entries
			effects, finishErr := tn.compactor.Finish(ctx, in)
			if finishErr == nil && len(effects) > 0 {
				finishErr = tn.applyCompaction(ctx, effects)
			}
			if finishErr != nil {
				runErr = errors.Join(runErr, StopFor(finishErr))
			}
			out.Usage = tn.snapshot().Usage()
		}()
	}
	if err := tn.load(ctx); err != nil {
		return Result{}, err
	}
	if err := tn.append(ctx, user); err != nil {
		return Result{}, err
	}
	system, err := l.o.System(ctx, t)
	if err != nil {
		return Result{}, fmt.Errorf("loop: system prompt: %w", err)
	}

	for {
		if result, done, err := tn.ended(ctx, nil); done {
			return result, err
		}
		tn.spent.Wall = l.o.Now().Sub(tn.started)
		if key, over := Exceeded(tn.limits, tn.snapshot()); over {
			return LimitResult(key, tn.snapshot()), nil
		}

		resp, streamErr := tn.stream(ctx, system)
		tn.final = resp.Text()
		calls := resp.Calls()
		runs := streamErr == nil && resp.Stop == provider.StopToolUse
		stored := resp.Items
		if !runs {
			// The calls of a response that failed, was cut off or ended
			// for another reason are not run. They still get a result.
			for _, call := range calls {
				unrun := result(call, transcript.OutcomeCancelled, textNotRun)
				stored = append(stored, transcript.Item{ToolResult: &unrun})
				tn.toolUpdate(ctx, unrun, nil)
			}
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
			return Result{}, err
		}
		if tn.sendErr != nil {
			return Result{}, tn.sendErr
		}
		if result, done, err := tn.ended(ctx, streamErr); done {
			return result, err
		}

		if resp.Stop == provider.StopPaused {
			// The provider carries on from what it is sent back, so the
			// conversation goes as it is. The next request is counted
			// against the limits at the top like any other.
			continue
		}
		if resp.Stop != provider.StopToolUse {
			reason, ok := StopReason(resp.Stop)
			if !ok {
				return Result{}, fmt.Errorf("loop: the response ended with the stop reason %q", resp.Stop)
			}
			decision, hookErr := l.o.Hooks.Stop(ctx, hookSession, string(reason))
			if result, done, err := tn.ended(ctx, nil); done {
				return result, err
			}
			logHook(ctx, l.o.Logger, t.SessionID, hookErr)
			if decision.Action == hooks.Allow {
				// A turn that ends by itself with todo items unfinished
				// is sent back to them, a bounded number of times.
				if reason != acp.StopReasonEndTurn || tn.limits.MaxTodoPokes == 0 {
					return tn.result(reason), nil
				}
				open, sent, err := todo.Open(tn.entries)
				if err != nil {
					return Result{}, err
				}
				if len(open) == 0 || int64(sent) >= tn.limits.MaxTodoPokes {
					return tn.result(reason), nil
				}
				poke := transcript.Item{User: &transcript.User{Parts: []transcript.Part{{Text: todo.Poke(open)}}}}
				if err := tn.append(ctx, poke); err != nil {
					return Result{}, err
				}
				continue
			}
			// A hook that always denies must not run the turn forever.
			if tn.stopDenials >= tn.limits.MaxStopDenials {
				return LimitResult(config.KeyMaxStopDenials, tn.snapshot()), nil
			}
			tn.stopDenials++
			denied := transcript.Item{User: &transcript.User{Parts: []transcript.Part{{Text: decision.Reason}}}}
			if err := tn.append(ctx, denied); err != nil {
				return Result{}, err
			}
			continue
		}
		if len(calls) == 0 {
			return Result{}, errors.New("loop: the model stopped for tool use without a tool call")
		}

		left, limited := ToolCallsLeft(tn.limits, tn.snapshot())
		// A tool that runs a model of its own, a subagent, reports what it
		// used here, so the turn's limits count it.
		toolCtx := tn.toolContext(ctx)
		done := Dispatcher{
			Tools: tn.model.Tools,
			Env:   tools.Env{SessionID: t.SessionID, Cwd: t.Cwd, Dirs: t.Dirs},
			Left:  left, Limited: limited, LimitKey: config.KeyMaxToolCalls,
			Permit: t.Permit,
			Update: func(update acp.SessionToolCallUpdate) {
				tn.send(ctx, acp.SessionUpdate{ToolCallUpdate: &update})
			},
			PutBlob: func(ctx context.Context, mimeType string, content io.Reader) (transcript.Stored, error) {
				return l.o.Store.PutBlob(ctx, id, mimeType, content)
			},
			Schemas: tn.schemas, Repair: l.o.Repair, Gate: l.o.Gate, Hooks: l.o.Hooks, Logger: l.o.Logger,
		}.Run(toolCtx, calls)
		tn.spentMu.Lock()
		tn.spent.ToolCalls += done.Ran
		tn.spentMu.Unlock()
		results := make([]transcript.Item, len(done.Results))
		for i := range done.Results {
			results[i] = transcript.Item{ToolResult: &done.Results[i]}
		}
		if err := tn.append(ctx, results...); err != nil {
			return Result{}, err
		}
		switch {
		case tn.sendErr != nil:
			return Result{}, tn.sendErr
		case done.Err != nil:
			return Result{}, done.Err
		case done.OverLimit:
			return LimitResult(config.KeyMaxToolCalls, tn.snapshot()), nil
		}
	}
}

// ended says how the turn ends when its context is over or its model
// request failed. It reports false when the turn goes on.
func (tn *turn) ended(ctx context.Context, streamErr error) (Result, bool, error) {
	switch {
	case ctx.Err() != nil && errors.Is(context.Cause(ctx), errWall):
		return LimitResult(config.KeyMaxWall, tn.snapshot()), true, nil
	case ctx.Err() != nil:
		// The model is told about the interruption when the next turn's
		// request is built.
		if err := tn.append(ctx, transcript.Item{Interrupted: &transcript.Interrupted{}}); err != nil {
			return Result{}, true, err
		}
		return tn.result(acp.StopReasonCancelled), true, nil
	case streamErr != nil:
		var limit *turnLimitError
		if errors.As(streamErr, &limit) {
			return LimitResult(limit.key, tn.snapshot()), true, nil
		}
		return Result{}, true, StopFor(streamErr)
	}
	return Result{}, false, nil
}

func (tn *turn) result(reason acp.StopReason) Result {
	return Result{StopReason: reason, Usage: tn.snapshot().Usage()}
}

type spendKey struct{}

// Spend adds usage to the turn that dispatched the tool whose context ctx
// is. A tool that makes model requests of its own, such as one that runs a
// subagent, calls it with each request's usage, and the turn's token limits
// then count it before the next request. It may be called from tools that
// run at the same time. It reports false outside a call the loop
// dispatched, where there is no turn to charge.
func Spend(ctx context.Context, u transcript.Usage) bool {
	add, ok := ctx.Value(spendKey{}).(func(transcript.Usage))
	if ok {
		add(u)
	}
	return ok
}

func (tn *turn) add(u transcript.Usage) {
	tn.spentMu.Lock()
	defer tn.spentMu.Unlock()
	tn.spent.Add(u)
}

func (tn *turn) snapshot() Spent {
	tn.spentMu.Lock()
	defer tn.spentMu.Unlock()
	return tn.spent
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
	schemas, err := Schemas(model.Tools)
	if err != nil {
		return err
	}
	tn.model, tn.schemas = model, schemas
	tn.mapper = Mapper{Tools: model.Tools}
	if model.ContextSize > 0 {
		// Legacy routers have no selected-entry metadata; preparation replaces this when available.
		tn.mapper.ContextSize = func(string) (int, bool) { return int(model.ContextSize), true }
	}
	return nil
}

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

// stream sends one model request and collects the response, showing it to
// the client as it arrives. The response holds what arrived before an error.
func (tn *turn) stream(ctx context.Context, system []provider.SystemBlock) (*Response, error) {
	ctx = router.WithReport(ctx, func(m router.Move) { tn.moved(ctx, m) })
	req := provider.Request{
		System:          system,
		Model:           tn.model.Model,
		Tools:           tn.model.Tools.Specs(),
		MaxOutputTokens: tn.model.MaxOutputTokens,
		Effort:          tn.model.Effort,
		CacheKey:        string(tn.t.SessionID),
	}
	resp := &Response{}
	if _, routed := tn.model.Provider.(*router.Router); routed {
		ctx = router.WithPrepare(ctx, tn.prepareRequest)
	} else {
		var err error
		req, err = tn.prepareRequest(ctx, router.Entry{}, req)
		if err != nil {
			return resp, err
		}
	}
	for event, err := range tn.model.Provider.Stream(ctx, req) {
		if err != nil {
			return resp, err
		}
		if event.Item != nil {
			item, _, err := compact.AutoItem(*event.Item)
			if err != nil {
				return resp, err
			}
			event.Item = &item
		}
		for _, update := range resp.Add(tn.mapper, event) {
			tn.send(ctx, update)
		}
		if tn.sendErr != nil {
			return resp, tn.sendErr
		}
		if resp.Stop != "" {
			return resp, nil
		}
		// A cancelled turn stops reading at once. Leaving the range closes
		// the connection, and what was not finished is not stored.
		if ctx.Err() != nil {
			return resp, context.Cause(ctx)
		}
	}
	// The stream ended without a stop and without an error. If the turn
	// was cancelled that is why, and the caller sees it in ctx.
	if ctx.Err() != nil {
		return resp, context.Cause(ctx)
	}
	return resp, errors.New("loop: the model's response ended without a stop reason")
}

// moved logs a move of the router and reports a retry or fallback to the
// client. A credential refresh is not a move the client acts on.
func (tn *turn) moved(ctx context.Context, m router.Move) {
	tn.l.o.Logger.WarnContext(ctx, "model request moved", "session", tn.t.SessionID, "kind", m.Kind,
		"from", m.From.String(), "to", m.To.String(), "wait", m.Wait, "err", m.Err)
	if m.Kind == router.MoveRefresh {
		return
	}
	move, ok := MoveFor(m)
	if !ok {
		return
	}
	tn.send(ctx, acp.SessionUpdate{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{
		Meta: harness.Meta(nil, harness.Addition{Move: &move}),
	}})
}

func (tn *turn) toolUpdate(ctx context.Context, r transcript.ToolResult, content []acp.ToolCallContent) {
	update, err := ToolResult(r, content)
	if err != nil {
		tn.sendMu.Lock()
		tn.sendErr = errors.Join(tn.sendErr, err)
		tn.sendMu.Unlock()
		return
	}
	tn.send(ctx, acp.SessionUpdate{ToolCallUpdate: &update})
}

// send sends one update. The first failure is kept and ends the turn: a
// client that cannot be reached is not worth spending tokens on.
func (tn *turn) send(ctx context.Context, update acp.SessionUpdate) {
	_ = tn.update(ctx, update)
}

func (tn *turn) update(ctx context.Context, update acp.SessionUpdate) error {
	tn.sendMu.Lock()
	defer tn.sendMu.Unlock()
	if tn.sendErr != nil {
		return tn.sendErr
	}
	// A cancelled turn still tells the client how its tool calls ended.
	if err := tn.t.Update(context.WithoutCancel(ctx), update); err != nil {
		tn.sendErr = fmt.Errorf("loop: sending an update: %w", err)
	}
	return tn.sendErr
}
