package loop

import (
	"context"

	"github.com/gfx-labs/towa/src/transcript"
)

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
