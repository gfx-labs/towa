package loop

import (
	"context"
	"errors"
	"fmt"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/transcript"
)

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
