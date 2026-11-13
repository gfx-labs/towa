package session

import (
	"context"

	acp "github.com/gfx-labs/go-acp"
)

// send sends one update for a session, unless it is of a type the client
// did not advertise.
func (a *Agent) send(ctx context.Context, s *session, update acp.SessionUpdate) error {
	if !a.Gate().Sendable(update) {
		return nil
	}
	return a.client.SessionUpdate(ctx, acp.SessionNotification{SessionId: s.id, Update: update})
}

// announce tells the client the session's commands once. A pending creation
// response owns the announcement until its AfterResponse callback runs.
func (a *Agent) announce(ctx context.Context, s *session) error {
	s.mu.Lock()
	if s.announcePending {
		s.mu.Unlock()
		return nil
	}
	done := s.announced
	s.announced = true
	s.mu.Unlock()
	if done {
		return nil
	}
	return a.send(ctx, s, acp.SessionUpdate{AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{
		SessionUpdate: "available_commands_update", AvailableCommands: a.registry.available(),
	}})
}

func (a *Agent) announceAfterResponse(ctx context.Context, s *session) {
	s.mu.Lock()
	s.announcePending = true
	s.mu.Unlock()
	if acp.AfterResponse(ctx, func() {
		live, owner := a.lookup(s.id)
		if live != s || owner != a {
			return
		}
		s.mu.Lock()
		s.announcePending = false
		s.mu.Unlock()
		if err := a.announce(ctx, s); err != nil {
			a.logger.WarnContext(ctx, "announcing session commands after response", "session", s.id, "err", err)
		}
	}) {
		return
	}
	// Direct callers without an ACP request context retain the first-later-
	// request behavior. No goroutine guesses when their response was sent.
	s.mu.Lock()
	s.announcePending = false
	s.mu.Unlock()
}
