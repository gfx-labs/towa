package loop

import (
	"context"
	"slices"
)

// Invocation is the turn that dispatched a tool and the loop that runs it.
// A child tool takes it from its call context, not a closure over an earlier turn.
type Invocation struct {
	Turn Turn
	Loop *Loop
}

type invocationKey struct{}

// Current returns the invocation that dispatched this tool call. It is also
// available inside a batch. The returned slices and limits may be changed by
// the caller. Nested ACP values remain immutable session data.
func Current(ctx context.Context) (Invocation, bool) {
	current, ok := ctx.Value(invocationKey{}).(Invocation)
	if !ok {
		return Invocation{}, false
	}
	current.Turn = copyTurn(current.Turn)
	return current, true
}

func copyTurn(t Turn) Turn {
	t.Prompt = slices.Clone(t.Prompt)
	t.Dirs = slices.Clone(t.Dirs)
	t.McpServers = slices.Clone(t.McpServers)
	t.ContextFiles = slices.Clone(t.ContextFiles)
	t.SkillsDirs = slices.Clone(t.SkillsDirs)
	if t.Limits != nil {
		limits := *t.Limits
		t.Limits = &limits
	}
	return t
}

func (tn *turn) toolContext(ctx context.Context) context.Context {
	t := copyTurn(tn.t)
	limits := tn.limits
	t.Limits = &limits
	t.Update = tn.update
	ctx = context.WithValue(ctx, invocationKey{}, Invocation{Turn: t, Loop: tn.l})
	return context.WithValue(ctx, spendKey{}, tn.add)
}
