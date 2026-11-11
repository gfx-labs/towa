package session

import (
	"context"
	"errors"
	"slices"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/loop"
	"github.com/gfx-labs/acpharness/src/tools"
)

// baseTurn snapshots the settings used to build a runtime. Its permission
// callback resolves the prompt from the context passed when it is called.
func (a *Agent) baseTurn(s *session) (loop.Turn, error) {
	gate := a.Gate()
	base := loop.Turn{
		SessionID: s.id,
		Cwd:       s.cwd,
		Dirs:      slices.Clone(s.dirs),
		Client:    gate.Client(),
		Sendable:  gate.Sendable,
		Update:    func(ctx context.Context, update acp.SessionUpdate) error { return a.send(ctx, s, update) },
		Permit: func(ctx context.Context, tool string, effect tools.Effect, call acp.ToolCallUpdate) (bool, error) {
			arrival, ok := acp.Arrival(ctx)
			if !ok {
				return false, errors.New("session: runtime permission request has no prompt arrival")
			}
			return a.permit(ctx, s, arrival, tool, effect, call)
		},
	}
	if err := wireCopy(s.systemPrompt, &base.SystemPrompt); err != nil {
		return loop.Turn{}, err
	}
	if err := wireCopy(s.mcpServers, &base.McpServers); err != nil {
		return loop.Turn{}, err
	}
	for id, value := range s.options() {
		if apply := a.registry.option(id).Apply; apply != nil {
			apply(&base, value)
		}
	}
	for _, apply := range s.perTurn {
		apply(&base)
	}
	base.Dirs = slices.Clone(base.Dirs)
	base.ContextFiles = slices.Clone(base.ContextFiles)
	base.SkillsDirs = slices.Clone(base.SkillsDirs)
	return base, nil
}

// openRuntime returns cleanup for a session that fails before attachment.
// After attachment the session's close path owns that cleanup instead.
func (a *Agent) openRuntime(ctx context.Context, s *session) error {
	a.liveMu.Lock()
	if a.ending || a.ended {
		a.liveMu.Unlock()
		return errors.New("session: the connection or host has ended")
	}
	a.opening.Add(1)
	s.finishOpen = a.opening.Done
	a.liveMu.Unlock()
	s.runtime.Turn = a.turn
	if a.open == nil {
		return nil
	}
	base, err := a.baseTurn(s)
	if err != nil {
		return err
	}
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopHost := context.AfterFunc(a.lifetime, cancel)
	stopRequest := context.AfterFunc(ctx, cancel)
	runtime, err := a.open(lifetime, base)
	stopRequest()
	s.runtime = runtime
	s.stopRuntime = func() { stopHost(); cancel() }
	if err == nil && runtime.Turn == nil {
		err = errors.New("session: Open returned no turn function")
	}
	if err == nil && lifetime.Err() != nil {
		err = context.Cause(lifetime)
	}
	if err == nil && ctx.Err() != nil {
		err = context.Cause(ctx)
	}
	a.liveMu.Lock()
	ended := a.ending || a.ended
	a.liveMu.Unlock()
	if err == nil && ended {
		err = errors.New("session: the connection or host ended while opening its runtime")
	}
	if err != nil {
		return errors.Join(err, s.close(context.WithoutCancel(ctx)))
	}
	return nil
}

// discard closes a runtime opened for a request that failed to attach.
func (a *Agent) discard(ctx context.Context, s *session, attached *bool) {
	if s.finishOpen != nil {
		defer s.finishOpen()
	}
	if *attached {
		return
	}
	if err := s.close(context.WithoutCancel(ctx)); err != nil {
		a.logger.ErrorContext(ctx, "closing unattached session runtime", "session", s.id, "err", err)
	}
}
