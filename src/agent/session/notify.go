package session

import (
	"context"
	"maps"

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
		SessionUpdate: "available_commands_update", AvailableCommands: a.registry().available(),
	}})
}

// reloaded brings a live session's config values in line with registry, a
// reloaded configuration's, and sends its client the options and commands.
// A value the registry no longer accepts takes its option's default, and
// one whose option is gone is dropped.
func (a *Agent) reloaded(ctx context.Context, s *session, registry *Registry) error {
	gate := a.Gate()
	s.setMu.Lock()
	defer s.setMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	before := maps.Clone(s.values)
	values := registry.defaults()
	for id, value := range s.values {
		option := registry.option(id)
		if option == nil {
			continue
		}
		if option.Depends != nil {
			values[id] = value
			continue
		}
		if kept, err := option.check(value, values); err == nil {
			values[id] = kept
		}
	}
	registry.align(values)
	s.values = values
	modeChanged := before[ModeOption] != values[ModeOption]
	if modeChanged {
		clear(s.allowed)
	}
	announced := s.announced
	s.mu.Unlock()
	if !maps.Equal(before, values) {
		if err := a.persist(ctx, s); err != nil {
			return err
		}
	}
	if modeChanged {
		if err := a.send(ctx, s, acp.SessionUpdate{CurrentModeUpdate: &acp.SessionCurrentModeUpdate{
			SessionUpdate: "current_mode_update", CurrentModeId: acp.SessionModeId(values[ModeOption].ID),
		}}); err != nil {
			return err
		}
	}
	if err := a.send(ctx, s, acp.SessionUpdate{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{
		SessionUpdate: "config_option_update", ConfigOptions: registry.configOptions(values, gate.BooleanOptions()),
	}}); err != nil {
		return err
	}
	if !announced {
		return nil
	}
	return a.send(ctx, s, acp.SessionUpdate{AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{
		SessionUpdate: "available_commands_update", AvailableCommands: registry.available(),
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
