// Package subagent runs a child turn for a tool call: a prompt answered by
// the same loop in the same process, stored as a session of its own under
// the calling session.
package subagent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"uuid"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/agent/harness"
	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/transcript"
)

// Runner runs one turn. *loop.Loop is one.
type Runner interface {
	Run(ctx context.Context, t loop.Turn) (loop.Result, error)
}

// Options is what a Spawner is built from.
type Options struct {
	Config config.Subagents
}

// Spawner starts subagents. One serves every session of the process, since
// subagents.max_concurrent counts them together.
type Spawner struct {
	cfg config.Subagents

	mu      sync.Mutex
	running int
}

// New returns a spawner.
func New(o Options) (*Spawner, error) {
	if err := o.Config.Validate(); err != nil {
		return nil, err
	}
	o.Config.Models = slices.Clone(o.Config.Models)
	return &Spawner{cfg: o.Config}, nil
}

// Parent is the call that asks for a subagent.
type Parent struct {
	// Loop runs this call's child with this session's tools, context,
	// store and providers. When nil, Options.Loop must have been supplied.
	Loop Runner
	// Turn is the turn the call belongs to. The child's turn is a copy of
	// it with its own session, prompt, model and updates, so the child has
	// the parent's working directory, client and Permit: a child's tool
	// call asks the parent's client.
	Turn loop.Turn
	// Progress is the call's tools.Env.Progress. It may be nil.
	Progress func(ctx context.Context, content []acp.ToolCallContent) error
}

// Input is one subagent to run.
type Input struct {
	// Prompt is the whole of what the child is told. It sees nothing of
	// the parent's conversation.
	Prompt string
	// Model is an entry of subagents.models. Empty means the
	// parent's model.
	Model string
	// Description says in a few words what the child is for. It is not
	// sent to the child. The caller titles its tool call with it.
	Description string
}

// Result is a subagent the model ended.
type Result struct {
	// Final is the text of the child's last response. It is never empty.
	Final string
	// SessionID is the child's session in the store.
	SessionID acp.SessionId
	// StopReason is end_turn, max_tokens or refusal. On max_tokens Final
	// was cut off by the model's output limit.
	StopReason acp.StopReason
	// Usage is what the child's model requests used, nil when none
	// reported usage.
	Usage *acp.Usage
	// Charged says the usage was added to the parent turn, so the parent's
	// limits count it. It is false when ctx was not from a call the loop
	// dispatched, and when Usage is nil.
	Charged bool
}

// Kind says why a call did not give a Result.
type Kind string

// Kinds of error.
const (
	// KindTooDeep: the caller is already Depth sessions below the client's
	// session and subagents.max_depth is Max. Nothing ran.
	KindTooDeep Kind = "too_deep"
	// KindTooMany: Max subagents are running, which is
	// subagents.max_concurrent. Nothing ran.
	KindTooMany Kind = "too_many"
	// KindModelNotAllowed: Model is not in subagents.models, which is
	// Allowed. Nothing ran.
	KindModelNotAllowed Kind = "model_not_allowed"
	// KindNoPrompt: the prompt is empty. Nothing ran.
	KindNoPrompt Kind = "no_prompt"
	// KindStopped: the child stopped short of the model ending it: a
	// limit, a rate limit, a rejected credential and the rest of
	// harness.StopKind. Stop says which.
	KindStopped Kind = "stopped"
	// KindCancelled: the parent's turn was cancelled while the child ran.
	KindCancelled Kind = "cancelled"
	// KindNoMessage: the model ended the child's turn and its last
	// response had no text.
	KindNoMessage Kind = "no_message"
	// KindFailed: the child's turn failed for a reason outside the lists
	// above. Err has it.
	KindFailed Kind = "failed"
)

// Error is a call that did not give a Result. When the child ran, SessionID
// names its session, which keeps what was stored before the end.
type Error struct {
	Kind Kind
	// Limit is the config key that refused the call, for KindTooDeep,
	// KindTooMany and KindModelNotAllowed.
	Limit      string
	Depth, Max int
	Model      string
	Allowed    []string
	SessionID  acp.SessionId
	// Stop is set for KindStopped.
	Stop *harness.Stop
	// StopReason is how the child's turn ended, when it returned a result.
	StopReason acp.StopReason
	// Usage and Charged are as in Result, including usage the child
	// reported before its turn failed.
	Usage   *acp.Usage
	Charged bool
	Err     error
}

func (e *Error) Error() string {
	switch e.Kind {
	case KindTooDeep:
		return fmt.Sprintf("subagent: %s: a session %d below the client's may not start one, %s is %d", e.Kind, e.Depth, e.Limit, e.Max)
	case KindTooMany:
		return fmt.Sprintf("subagent: %s: %d are running, %s is %d", e.Kind, e.Max, e.Limit, e.Max)
	case KindModelNotAllowed:
		return fmt.Sprintf("subagent: %s: %q is not in %s %q", e.Kind, e.Model, e.Limit, e.Allowed)
	case KindNoPrompt:
		return fmt.Sprintf("subagent: %s", e.Kind)
	case KindStopped:
		return fmt.Sprintf("subagent: %s: session %s: %s %s", e.Kind, e.SessionID, e.Stop.Kind, e.Stop.Limit)
	}
	if e.Err != nil {
		return fmt.Sprintf("subagent: %s: session %s: %v", e.Kind, e.SessionID, e.Err)
	}
	return fmt.Sprintf("subagent: %s: session %s", e.Kind, e.SessionID)
}

func (e *Error) Unwrap() error { return e.Err }

type depthKey struct{}

// depth is how many sessions below the client's the caller is. A child's
// tools get a context made from the one the child's turn ran with.
func depth(ctx context.Context) int {
	d, _ := ctx.Value(depthKey{}).(int)
	return d
}

// Run runs one subagent to its end and returns its last message. ctx is the
// context the tool's Run was given: cancelling it cancels the child, and the
// child's usage is charged to the turn it belongs to.
func (s *Spawner) Run(ctx context.Context, parent Parent, in Input) (Result, error) {
	runner := parent.Loop
	if runner == nil {
		return Result{}, &Error{Kind: KindFailed, Err: errors.New("Parent.Loop is required")}
	}
	if _, err := uuid.Parse(string(parent.Turn.SessionID)); err != nil {
		return Result{}, &Error{Kind: KindFailed, Err: fmt.Errorf("parent session id: %w", err)}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, &Error{Kind: KindCancelled, Err: err}
	}
	if in.Prompt == "" {
		return Result{}, &Error{Kind: KindNoPrompt}
	}
	at := depth(ctx)
	if at >= s.cfg.MaxDepth {
		return Result{}, &Error{Kind: KindTooDeep, Limit: config.KeySubagentsMaxDepth, Depth: at, Max: s.cfg.MaxDepth}
	}
	model := parent.Turn.Model
	if in.Model != "" {
		if !slices.Contains(s.cfg.Models, in.Model) {
			return Result{}, &Error{
				Kind: KindModelNotAllowed, Limit: config.KeySubagentsModels,
				Model: in.Model, Allowed: slices.Clone(s.cfg.Models),
			}
		}
		model = in.Model
	}
	if !s.start() {
		return Result{}, &Error{Kind: KindTooMany, Limit: config.KeySubagentsMaxConcurrent, Max: s.cfg.MaxConcurrent}
	}
	defer s.done()

	shown := &view{progress: parent.Progress}
	child := parent.Turn
	child.SessionID = acp.SessionId(uuid.NewV7().String())
	child.ParentID = parent.Turn.SessionID
	child.Prompt = []acp.ContentBlock{{Text: &acp.ContentBlockText{Type: "text", Text: in.Prompt}}}
	child.Model = model
	child.Update = shown.update
	// The view takes every update and picks what it shows.
	child.Sendable = func(acp.SessionUpdate) bool { return true }

	res, err := runner.Run(context.WithValue(ctx, depthKey{}, at+1), child)
	charged := false
	if res.Usage != nil {
		charged = loop.Spend(ctx, usage(*res.Usage))
	}
	if err != nil {
		failed := &Error{
			Kind: KindFailed, SessionID: child.SessionID, Err: err,
			Usage: res.Usage, Charged: charged, StopReason: res.StopReason,
		}
		var stop *loop.StopError
		switch {
		case errors.As(err, &stop):
			failed.Kind, failed.Stop = KindStopped, &stop.Stop
		case ctx.Err() != nil:
			failed.Kind = KindCancelled
		}
		return Result{}, failed
	}
	ended := &Error{SessionID: child.SessionID, StopReason: res.StopReason, Usage: res.Usage, Charged: charged}
	switch {
	case res.Stop != nil:
		ended.Kind, ended.Stop = KindStopped, res.Stop
	case res.StopReason == acp.StopReasonCancelled:
		ended.Kind = KindCancelled
	case res.StopReason != acp.StopReasonEndTurn && res.StopReason != acp.StopReasonMaxTokens && res.StopReason != acp.StopReasonRefusal:
		ended.Kind = KindFailed
		ended.Err = fmt.Errorf("unexpected child stop reason %q", res.StopReason)
	case res.Final == "":
		ended.Kind = KindNoMessage
	default:
		return Result{
			Final: res.Final, SessionID: child.SessionID, StopReason: res.StopReason,
			Usage: res.Usage, Charged: charged,
		}, nil
	}
	return Result{}, ended
}

// start takes a place among the running subagents, and reports false when
// there is none.
func (s *Spawner) start() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.MaxConcurrent > 0 && s.running >= s.cfg.MaxConcurrent {
		return false
	}
	s.running++
	return true
}

func (s *Spawner) done() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running--
}

// usage turns a turn's totals back into the counts the loop adds up.
func usage(u acp.Usage) transcript.Usage {
	out := transcript.Usage{InputTokens: int64(u.InputTokens), OutputTokens: int64(u.OutputTokens)}
	if u.CachedReadTokens != nil {
		out.CacheReadTokens = int64(*u.CachedReadTokens)
	}
	if u.CachedWriteTokens != nil {
		out.CacheWriteTokens = int64(*u.CachedWriteTokens)
	}
	if u.ThoughtTokens != nil {
		out.ThoughtTokens = int64(*u.ThoughtTokens)
	}
	return out
}
