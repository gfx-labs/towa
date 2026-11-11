package loop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
	"uuid"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/pkg/acp/harness"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/hooks"
	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/provider/router"
	"github.com/gfx-labs/acpharness/src/store"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/repair"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// Options is what a turn is built from. Every field is required unless it
// says otherwise.
type Options struct {
	// Provider answers model requests. It is the router over the model's
	// chain, which sets each request's model and credential.
	Provider provider.Provider
	Tools    tools.Set
	Store    store.Store
	// System returns the system prompt's sections for a turn.
	System func(ctx context.Context, t Turn) ([]provider.SystemBlock, error)
	Limits config.Limits
	// Now is the clock: entry times and the turn's wall time come from it.
	Now func() time.Time
	// Interruption is the dialect's text that tells the model the turn
	// before was interrupted.
	Interruption string
	// ContextSize returns a model's context window in tokens, for usage
	// updates. It is optional: without it no usage update is sent.
	ContextSize func(model string) (int, bool)
	// MaxOutputTokens and Effort are sent with every request. They are
	// optional and not sent when zero.
	MaxOutputTokens int64
	Effort          string
	// Repair is tools.repair.enabled: fix the known ways a model gets a
	// tool's argument wrong before validating it.
	Repair bool
	// Hooks runs the configured hooks. It may have none.
	Hooks  *hooks.Runner
	Logger *slog.Logger
}

// Loop runs prompt turns. It keeps nothing between turns: a session's
// conversation is read from the store when a turn starts.
type Loop struct {
	o       Options
	mapper  Mapper
	schemas map[string]*repair.Schema
}

// New returns a loop over its dependencies.
func New(o Options) (*Loop, error) {
	switch {
	case o.Provider == nil:
		return nil, errors.New("loop: Provider is required")
	case o.Store == nil:
		return nil, errors.New("loop: Store is required")
	case o.System == nil:
		return nil, errors.New("loop: System is required")
	case o.Now == nil:
		return nil, errors.New("loop: Now is required")
	case o.Interruption == "":
		return nil, errors.New("loop: Interruption is required")
	case o.Hooks == nil:
		return nil, errors.New("loop: Hooks is required")
	case o.Logger == nil:
		return nil, errors.New("loop: Logger is required")
	}
	schemas, err := Schemas(o.Tools)
	if err != nil {
		return nil, err
	}
	return &Loop{o: o, mapper: Mapper{Tools: o.Tools, ContextSize: o.ContextSize}, schemas: schemas}, nil
}

// DeniedError is a prompt a hook denied. The turn did not start.
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
		}
	case e.Usage != nil:
		r.Usage = e.Usage
	case e.Stop != "":
		r.Stop = e.Stop
	}
	return nil
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
	spent   Spent
	// items is the conversation as the next request sends it.
	items []transcript.Item
	// parent is the id of the last entry stored.
	parent uuid.UUID
	// stopDenials is how often a stop hook kept the turn going.
	stopDenials int64

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
	tn := &turn{l: l, t: t, id: id, started: l.o.Now()}
	if l.o.Limits.MaxWall > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, l.o.Limits.MaxWall, errWall)
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
		return Result{}, &DeniedError{Reason: decision.Reason}
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
		if key, over := Exceeded(l.o.Limits, tn.spent); over {
			return LimitResult(key, tn.spent), nil
		}

		resp, streamErr := tn.stream(ctx, system)
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
			tn.spent.Add(*resp.Usage)
			stored = append(stored, transcript.Item{Usage: resp.Usage})
			if update, ok := l.mapper.Usage(*resp.Usage); ok {
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
				return tn.result(reason), nil
			}
			// A hook that always denies must not run the turn forever.
			if tn.stopDenials >= l.o.Limits.MaxStopDenials {
				return LimitResult(config.KeyMaxStopDenials, tn.spent), nil
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

		left, limited := ToolCallsLeft(l.o.Limits, tn.spent)
		done := Dispatcher{
			Tools: l.o.Tools,
			Env:   tools.Env{SessionID: t.SessionID, Cwd: t.Cwd, Dirs: t.Dirs},
			Left:  left, Limited: limited, LimitKey: config.KeyMaxToolCalls,
			Permit: t.Permit,
			Update: func(update acp.SessionToolCallUpdate) {
				tn.send(ctx, acp.SessionUpdate{ToolCallUpdate: &update})
			},
			PutBlob: func(ctx context.Context, mimeType string, content io.Reader) (transcript.Stored, error) {
				return l.o.Store.PutBlob(ctx, id, mimeType, content)
			},
			Schemas: l.schemas, Repair: l.o.Repair, Hooks: l.o.Hooks, Logger: l.o.Logger,
		}.Run(ctx, calls)
		tn.spent.ToolCalls += done.Ran
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
			return LimitResult(config.KeyMaxToolCalls, tn.spent), nil
		}
	}
}

// ended says how the turn ends when its context is over or its model
// request failed. It reports false when the turn goes on.
func (tn *turn) ended(ctx context.Context, streamErr error) (Result, bool, error) {
	switch {
	case ctx.Err() != nil && errors.Is(context.Cause(ctx), errWall):
		return LimitResult(config.KeyMaxWall, tn.spent), true, nil
	case ctx.Err() != nil:
		// The model is told about the interruption when the next turn's
		// request is built.
		if err := tn.append(ctx, transcript.Item{Interrupted: &transcript.Interrupted{}}); err != nil {
			return Result{}, true, err
		}
		return tn.result(acp.StopReasonCancelled), true, nil
	case streamErr != nil:
		return Result{}, true, StopFor(streamErr)
	}
	return Result{}, false, nil
}

func (tn *turn) result(reason acp.StopReason) Result {
	return Result{StopReason: reason, Usage: tn.spent.Usage()}
}

// load reads the session's conversation, creating the session when the
// store does not have it, and stores a result for every call left without
// one.
func (tn *turn) load(ctx context.Context) error {
	st := tn.l.o.Store
	_, entries, err := st.Load(ctx, tn.id)
	if errors.Is(err, store.ErrNotFound) {
		now := tn.l.o.Now()
		return st.Create(ctx, store.Meta{ID: tn.id, Cwd: tn.t.Cwd, Dirs: tn.t.Dirs, Created: now, Updated: now})
	}
	if err != nil {
		return fmt.Errorf("loop: loading session %s: %w", tn.id, err)
	}
	var history []transcript.Item
	for entry, err := range entries {
		if err != nil {
			return fmt.Errorf("loop: loading session %s: %w", tn.id, err)
		}
		history = append(history, entry.Item)
		tn.parent = entry.ID
	}
	items, repairs, err := Conversation(history, tn.l.o.Interruption)
	if err != nil {
		return err
	}
	tn.items = items
	return tn.store(ctx, repairs)
}

// append stores items and adds them to the conversation.
func (tn *turn) append(ctx context.Context, items ...transcript.Item) error {
	for _, item := range items {
		// Usage is recorded and is not part of what the model is sent.
		// Interrupted is the last item of its turn.
		if item.Usage == nil && item.Interrupted == nil {
			tn.items = append(tn.items, item)
		}
	}
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
	return nil
}

// stream sends one model request and collects the response, showing it to
// the client as it arrives. The response holds what arrived before an error.
func (tn *turn) stream(ctx context.Context, system []provider.SystemBlock) (*Response, error) {
	o := tn.l.o
	ctx = router.WithReport(ctx, func(m router.Move) { tn.moved(ctx, m) })
	req := provider.Request{
		System:          system,
		Items:           tn.items,
		Tools:           o.Tools.Specs(),
		MaxOutputTokens: o.MaxOutputTokens,
		Effort:          o.Effort,
		CacheKey:        string(tn.t.SessionID),
	}
	resp := &Response{}
	for event, err := range o.Provider.Stream(ctx, req) {
		if err != nil {
			return resp, err
		}
		for _, update := range resp.Add(tn.l.mapper, event) {
			tn.send(ctx, update)
		}
		if tn.sendErr != nil {
			return resp, tn.sendErr
		}
		if resp.Stop != "" {
			return resp, nil
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
	tn.sendMu.Lock()
	defer tn.sendMu.Unlock()
	if tn.sendErr != nil {
		return
	}
	// A cancelled turn still tells the client how its tool calls ended.
	if err := tn.t.Update(context.WithoutCancel(ctx), update); err != nil {
		tn.sendErr = fmt.Errorf("loop: sending an update: %w", err)
	}
}
