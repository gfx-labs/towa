package session

import (
	"context"

	acp "github.com/gfx-labs/go-acp"
)

// optionChange is one request to set a config option.
type optionChange struct {
	id    acp.SessionConfigId
	value Value
	// boolean says the request set a boolean option.
	boolean bool
	// inResponse says the caller answers with the config options, as
	// session/set_config_option does, so no config_option_update is sent.
	inResponse bool
}

// setOption changes a config option of a session and tells the client what
// its request did not already tell it.
func (a *Agent) setOption(ctx context.Context, s *session, change optionChange) error {
	gate := a.Gate()
	// Held while the updates are sent, so that two changes reach the client
	// in the order they were made.
	s.setMu.Lock()
	defer s.setMu.Unlock()
	if err := a.checkChange(gate, change, s.options()); err != nil {
		return err
	}
	values, changed, err := s.set(change.id, change.value, a.registry.align)
	if err != nil || !changed {
		return err
	}
	if err := a.persist(ctx, s); err != nil {
		return err
	}
	if change.id == ModeOption {
		if err := a.send(ctx, s, acp.SessionUpdate{CurrentModeUpdate: &acp.SessionCurrentModeUpdate{
			SessionUpdate: "current_mode_update", CurrentModeId: acp.SessionModeId(change.value.ID),
		}}); err != nil {
			return err
		}
	}
	if change.inResponse {
		return nil
	}
	return a.send(ctx, s, acp.SessionUpdate{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{
		SessionUpdate: "config_option_update", ConfigOptions: a.registry.configOptions(values, gate.BooleanOptions()),
	}})
}

// checkChange reports an option change the registry does not accept for a
// session with values.
func (a *Agent) checkChange(gate Gate, change optionChange, values map[acp.SessionConfigId]Value) error {
	option := a.registry.option(change.id)
	switch {
	case option == nil, option.Boolean && !gate.BooleanOptions():
		return invalid("unknown config option %q", change.id)
	case option.Boolean != change.boolean:
		return invalid("config option %q is not of that type", change.id)
	}
	if err := option.check(change.value, values); err != nil {
		return invalid("%v", err)
	}
	return nil
}

// SetSessionConfigOption changes one config option and answers with all of
// them.
func (a *Agent) SetSessionConfigOption(ctx context.Context, p acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	var sessionID acp.SessionId
	var id acp.SessionConfigId
	var value Value
	switch {
	case p.Boolean != nil:
		sessionID, id, value = p.Boolean.SessionId, p.Boolean.ConfigId, Value{On: p.Boolean.Value}
	case p.ValueId != nil:
		sessionID, id, value = p.ValueId.SessionId, p.ValueId.ConfigId, Value{ID: p.ValueId.Value}
	default:
		return acp.SetSessionConfigOptionResponse{}, invalid("the request has no value")
	}
	s, err := a.session(sessionID)
	if err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}
	if err := a.announce(ctx, s); err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}
	if err := a.setOption(ctx, s, optionChange{id: id, value: value, boolean: p.Boolean != nil, inResponse: true}); err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}
	return acp.SetSessionConfigOptionResponse{ConfigOptions: a.registry.configOptions(s.options(), a.Gate().BooleanOptions())}, nil
}

// SetSessionMode switches the session's mode, which is the mode config
// option under its older name.
func (a *Agent) SetSessionMode(ctx context.Context, p acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	s, err := a.session(p.SessionId)
	if err != nil {
		return acp.SetSessionModeResponse{}, err
	}
	if err := a.announce(ctx, s); err != nil {
		return acp.SetSessionModeResponse{}, err
	}
	return acp.SetSessionModeResponse{}, a.setOption(ctx, s, optionChange{id: ModeOption, value: Value{ID: acp.SessionConfigValueId(p.ModeId)}})
}

// UnstableSessionOptions answers with the modes and config options a new
// session would start with. It creates nothing.
func (a *Agent) UnstableSessionOptions(ctx context.Context, p acp.UnstableSessionOptionsRequest) (acp.UnstableSessionOptionsResponse, error) {
	if err := a.ready(); err != nil {
		return acp.UnstableSessionOptionsResponse{}, err
	}
	if err := a.checkDirs(p.Cwd, p.AdditionalDirectories); err != nil {
		return acp.UnstableSessionOptionsResponse{}, err
	}
	var stored map[acp.SessionConfigId]Value
	if p.SessionId != nil {
		// What load or resume would answer, which needs the store.
		if a.store == nil {
			return acp.UnstableSessionOptionsResponse{}, invalid("sessionId needs session/load or session/resume, which are not supported")
		}
		meta, err := a.storedMeta(ctx, acp.SessionId(*p.SessionId))
		if err != nil {
			return acp.UnstableSessionOptionsResponse{}, err
		}
		if meta.Cwd != p.Cwd {
			return acp.UnstableSessionOptionsResponse{}, cwdMismatch(p.Cwd, meta.Cwd)
		}
		state, _, _, err := a.restore(meta, nil, nil)
		if err != nil {
			return acp.UnstableSessionOptionsResponse{}, err
		}
		stored = state.Values
	}
	values, _, err := a.startValues(nil, nil, stored)
	if err != nil {
		return acp.UnstableSessionOptionsResponse{}, err
	}
	mode := a.registry.option(ModeOption)
	modes := &acp.UnstableSessionModeState{CurrentModeId: acp.SessionModeId(values[ModeOption].ID)}
	for _, c := range mode.Choices {
		modes.AvailableModes = append(modes.AvailableModes, acp.UnstableSessionMode{
			Id: acp.SessionModeId(c.Value), Name: c.Name, Description: optional(c.Description),
		})
	}
	return acp.UnstableSessionOptionsResponse{
		Modes:         modes,
		ConfigOptions: a.registry.unstableConfigOptions(values, a.Gate().BooleanOptions()),
	}, nil
}
