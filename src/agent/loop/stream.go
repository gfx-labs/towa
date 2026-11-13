package loop

import (
	"context"
	"errors"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/agent/harness"
	"github.com/gfx-labs/towa/src/prompt/compact"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/provider/router"
)

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
