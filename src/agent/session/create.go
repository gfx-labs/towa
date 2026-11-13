package session

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"time"
	"uuid"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/prompt/agentctx"
	"github.com/gfx-labs/towa/src/store"
)

func invalid(format string, args ...any) *acp.RequestError {
	return acp.NewInvalidParams(map[string]any{"error": fmt.Sprintf(format, args...)})
}

// checkDirs reports a working directory or additional directory that the
// agent cannot take.
func (a *Agent) checkDirs(cwd string, dirs []string) error {
	if !filepath.IsAbs(cwd) {
		return acp.NewInvalidParams(map[string]any{"error": "cwd must be an absolute path", "cwd": cwd})
	}
	if len(dirs) > 0 && !a.supports.AdditionalDirectories {
		return invalid("additionalDirectories is not supported")
	}
	for _, dir := range dirs {
		if !filepath.IsAbs(dir) {
			return invalid("additionalDirectories: %q is not an absolute path", dir)
		}
	}
	return nil
}

// checkSystemPrompt reports a system prompt the prompt builder cannot
// apply, with the builder's own check, so that the session is not created.
func (a *Agent) checkSystemPrompt(p *acp.SystemPrompt) error {
	if p == nil {
		return nil
	}
	if !a.supports.SystemPrompt {
		return invalid("systemPrompt is not supported")
	}
	_, err := agentctx.Build(agentctx.Prompt{Client: p})
	_, mode := errors.AsType[*agentctx.UnsupportedModeError](err)
	_, content := errors.AsType[*agentctx.ContentError](err)
	if mode || content {
		return invalid("%v", err)
	}
	return err
}

// checkMCP reports an MCP server the turn cannot connect to.
func (a *Agent) checkMCP(servers []acp.McpServer) error {
	mcp := a.supports.MCP
	for i, server := range servers {
		switch {
		case mcp == nil:
			return invalid("mcpServers is not supported")
		case server.Http != nil && !mcp.Http:
			return invalid("mcpServers[%d]: the http transport is not supported", i)
		case server.Sse != nil && !mcp.Sse:
			return invalid("mcpServers[%d]: the sse transport is not supported", i)
		case server.Acp != nil:
			return invalid("mcpServers[%d]: the acp transport is not supported", i)
		}
	}
	return nil
}

// checkRequest reports what the agent cannot take of a request that makes a
// session. The first refusal, when there is one, is the answer.
func (a *Agent) checkRequest(cwd string, dirs []string, prompt *acp.SystemPrompt, servers []acp.McpServer) error {
	err := errors.Join(a.checkDirs(cwd, dirs), a.checkSystemPrompt(prompt), a.checkMCP(servers))
	if refused, ok := errors.AsType[*acp.RequestError](err); ok {
		return refused
	}
	return err
}

// NewSession creates a session in the request's working directory.
func (a *Agent) NewSession(ctx context.Context, p acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	if err := a.ready(); err != nil {
		return acp.NewSessionResponse{}, err
	}
	if err := a.checkRequest(p.Cwd, p.AdditionalDirectories, p.SystemPrompt, p.McpServers); err != nil {
		return acp.NewSessionResponse{}, err
	}
	values, perTurn, err := a.startValues(p.Meta, p.ConfigOptions, nil)
	if err != nil {
		return acp.NewSessionResponse{}, err
	}

	id := uuid.NewV7()
	s := newSession(acp.SessionId(id.String()), p.Cwd)
	s.dirs, s.systemPrompt, s.mcpServers = p.AdditionalDirectories, p.SystemPrompt, p.McpServers
	s.values, s.perTurn = values, perTurn
	s.overrides = overrides(p.Meta)
	attached := false
	defer a.discard(ctx, s, &attached)
	a.announceAfterResponse(ctx, s)
	if err := a.openRuntime(ctx, s); err != nil {
		return acp.NewSessionResponse{}, err
	}
	if a.store != nil {
		// Created here and not by the first turn, so that a session with
		// no turn yet can be listed, titled and loaded.
		now := time.Now()
		s.meta = store.Meta{ID: id, Cwd: s.cwd, Created: now, Updated: now}
		if err := a.fillMeta(s, &s.meta); err != nil {
			return acp.NewSessionResponse{}, err
		}
		if err := a.store.Create(ctx, s.meta); err != nil {
			return acp.NewSessionResponse{}, fmt.Errorf("session: storing session %s: %w", s.id, err)
		}
	}
	if err := a.put(s, a); err != nil {
		return acp.NewSessionResponse{}, err
	}
	attached = true
	a.logger.Info("session created", "session", s.id, "cwd", s.cwd)
	return acp.NewSessionResponse{
		SessionId:     s.id,
		Modes:         modeState(a.registry.option(ModeOption), values[ModeOption].ID),
		ConfigOptions: a.registry.configOptions(values, a.Gate().BooleanOptions()),
	}, nil
}

// startValues returns the config option values a session starts with and
// the changes its _meta overrides make to each turn. stored holds values
// kept with a stored session. The order is: defaults, stored values, the
// request's _meta.towa, the request's configOptions.
func (a *Agent) startValues(meta map[string]any, initial []acp.InitialSessionConfigOption, stored map[acp.SessionConfigId]Value) (map[acp.SessionConfigId]Value, []func(*loop.Turn), error) {
	gate := a.Gate()
	values := a.registry.defaults()
	maps.Copy(values, stored)
	perTurn, err := a.registry.applyMeta(meta, values)
	if err != nil {
		return nil, nil, invalid("%v", err)
	}
	// Initial values are applied in order. Unlike session/set_config_option,
	// the protocol has an entry that does not apply ignored, and only a
	// repeated id rejected.
	seen := map[acp.SessionConfigId]bool{}
	for _, entry := range initial {
		var id acp.SessionConfigId
		var value Value
		switch {
		case entry.Boolean != nil:
			id, value = entry.Boolean.ConfigId, Value{On: entry.Boolean.Value}
		case entry.ValueId != nil:
			id, value = entry.ValueId.ConfigId, Value{ID: entry.ValueId.Value}
		default:
			return nil, nil, invalid("configOptions: an entry has no value")
		}
		if seen[id] {
			return nil, nil, invalid("configOptions: %q is set twice", id)
		}
		seen[id] = true
		option := a.registry.option(id)
		if option == nil || option.Boolean != (entry.Boolean != nil) || option.Boolean && !gate.BooleanOptions() || option.check(value) != nil {
			a.logger.Debug("initial config option ignored", "option", id)
			continue
		}
		values[id] = value
	}
	// A stored value the request did not replace must still be usable
	// here: this process may be configured differently from the one that
	// stored it.
	for id, value := range stored {
		if values[id] != value {
			continue
		}
		option := a.registry.option(id)
		if option == nil {
			return nil, nil, invalid("stored config option %q is not supported here", id)
		}
		if err := option.check(value); err != nil {
			return nil, nil, invalid("the session was stored with a value that cannot be used here, set another in configOptions: %v", err)
		}
	}
	return values, perTurn, nil
}

// modeState lists the choices of the mode option as session modes.
func modeState(option *ConfigOption, current acp.SessionConfigValueId) *acp.SessionModeState {
	state := &acp.SessionModeState{CurrentModeId: acp.SessionModeId(current)}
	for _, c := range option.Choices {
		state.AvailableModes = append(state.AvailableModes, acp.SessionMode{
			Id: acp.SessionModeId(c.Value), Name: c.Name, Description: optional(c.Description),
		})
	}
	return state
}
