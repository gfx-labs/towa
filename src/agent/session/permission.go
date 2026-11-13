package session

import (
	"context"
	"fmt"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/tools"
)

// Permission option ids offered to the client.
const (
	allowOnce   = "allow_once"
	allowAlways = "allow_always"
	reject      = "reject_once"
)

// permitRequest is one tool call that needs a decision.
type permitRequest struct {
	// arrival is the arrival of the prompt that made the call.
	arrival uint64
	tool    string
	effect  tools.Effect
	call    acp.ToolCallUpdate
}

// permit decides whether one tool call of a prompt may run. See
// loop.Turn.Permit.
func (a *Agent) permit(ctx context.Context, s *session, r permitRequest) (bool, error) {
	if r.effect == tools.ReadOnly {
		return true, nil
	}
	s.mu.Lock()
	ask := s.values[ModeOption].ID == ModeAsk && !s.allowed[r.tool]
	s.mu.Unlock()
	if !ask {
		return true, nil
	}
	// When ctx ends, the connection asks the client to cancel the request
	// and this returns ctx's error.
	resp, err := a.client.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: s.id,
		ToolCall:  r.call,
		Options: []acp.PermissionOption{
			{OptionId: allowOnce, Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
			{OptionId: allowAlways, Name: "Always allow " + r.tool, Kind: acp.PermissionOptionKindAllowAlways},
			{OptionId: reject, Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
		},
	})
	if err != nil {
		return false, fmt.Errorf("session: permission request for %s: %w", r.tool, err)
	}
	return s.decide(r, resp.Outcome)
}

// decide turns the client's answer into the decision for the call.
func (s *session) decide(r permitRequest, outcome acp.RequestPermissionOutcome) (bool, error) {
	switch {
	case outcome.Cancelled != nil:
		// The client answers this way for a turn it has cancelled. Its
		// session/cancel may still be on the way, so the turn is stopped
		// here and answers as cancelled either way.
		s.stop(r.arrival)
		return false, context.Canceled
	case outcome.Selected == nil:
		return false, fmt.Errorf("session: permission response for %s has no outcome", r.tool)
	case outcome.Selected.OptionId == allowOnce:
		return true, nil
	case outcome.Selected.OptionId == allowAlways:
		s.mu.Lock()
		s.allowed[r.tool] = true
		s.mu.Unlock()
		return true, nil
	case outcome.Selected.OptionId == reject:
		return false, nil
	default:
		return false, fmt.Errorf("session: permission response for %s selects %q, which was not offered", r.tool, outcome.Selected.OptionId)
	}
}
