package session

import (
	"context"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/agent/loop"
)

// elicitation lets the turn's MCP servers ask the user through the client,
// when the client advertised elicitation. Each request is scoped to s.
func (a *Agent) elicitation(s *session, turn *loop.Turn) {
	if turn.Client.Elicitation == nil {
		return
	}
	turn.Elicit = func(ctx context.Context, params acp.CreateElicitationRequest) (acp.CreateElicitationResponse, error) {
		switch {
		case params.Form != nil:
			params.Form.SessionId = s.id
		case params.Url != nil:
			params.Url.SessionId = s.id
		}
		return a.client.CreateElicitation(ctx, params)
	}
	turn.ElicitationComplete = a.client.CompleteElicitation
}
