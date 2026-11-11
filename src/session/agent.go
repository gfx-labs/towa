// Package session implements the ACP agent: it owns the sessions of one
// connection and runs their prompt turns.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"strings"
	"sync"
	"uuid"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/pkg/acp/harness"
	"github.com/gfx-labs/acpharness/src/agentctx"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/loop"
	"github.com/gfx-labs/acpharness/src/telemetry"
	"github.com/gfx-labs/acpharness/src/tools"
)

// TurnFunc runs one prompt turn and says how it ended. It returns when ctx
// is cancelled, with the cancelled stop reason or with an error. A failure
// the client can act on is a *loop.StopError.
type TurnFunc func(ctx context.Context, turn loop.Turn) (loop.Result, error)

// Client is what the agent calls on the ACP client.
type Client interface {
	SessionUpdate(ctx context.Context, params acp.SessionNotification) error
	RequestPermission(ctx context.Context, params acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error)
}

// Supports says which optional parts of a session the turn acts on. The
// agent carries each of them to the turn, and advertises one only when the
// wiring says the turn uses it. A client that sends one that is off gets an
// error, since accepting it would drop it.
type Supports struct {
	// AdditionalDirectories: the turn's tools work in loop.Turn.Dirs.
	AdditionalDirectories bool
	// SystemPrompt: the turn builds its system prompt with agentctx.Build
	// from loop.Turn.SystemPrompt.
	SystemPrompt bool
	// ContextOverrides: the turn's context is loaded with
	// loop.Turn.ContextFiles and loop.Turn.SkillsDirs added to the
	// configured ones. It allows the context_files and skills_dirs keys of
	// _meta.acpharness on session/new.
	ContextOverrides bool
	// MCP: the turn connects to loop.Turn.McpServers over stdio, and over
	// the transports named here. Nil means it connects to none.
	MCP *acp.McpCapabilities
	// Prompt names the content types beyond text and resource links that
	// the provider takes in a prompt.
	Prompt acp.PromptCapabilities
}

// Options configures an Agent. Every field but Registry and Supports is
// required.
type Options struct {
	// Version is reported to the client as the agent's version.
	Version string
	Turn    TurnFunc
	Logger  *slog.Logger
	Tracer  trace.Tracer
	Meter   metric.Meter
	// Session is the session section of the configuration.
	Session config.Session
	// Registry holds the config options, commands and overrides other
	// packages add. Nil means none. The mode option, its override and its
	// command are added to a copy of it.
	Registry *Registry
	Supports Supports
}

// Agent is the ACP agent for one connection. It advertises only what it
// implements: a method behind a capability it does not advertise answers
// "Method not found".
type Agent struct {
	version  string
	turn     TurnFunc
	logger   *slog.Logger
	tracer   trace.Tracer
	turns    metric.Int64Counter
	client   Client
	registry *Registry
	supports Supports

	mu          sync.Mutex
	initialized bool
	gate        Gate
	sessions    map[acp.SessionId]*session
}

var _ acp.Agent = (*Agent)(nil)

// NewAgent returns an agent with no sessions. Call SetClient before the
// connection is served.
func NewAgent(o Options) (*Agent, error) {
	turns, err := o.Meter.Int64Counter("acpharness.turns",
		metric.WithDescription("Prompt turns finished, by stop reason."),
		metric.WithUnit("{turn}"))
	if err != nil {
		return nil, err
	}
	registry := &Registry{}
	if o.Registry != nil {
		registry = o.Registry.clone()
	}
	if err := errors.Join(
		registry.AddConfigOption(modeOption(acp.SessionConfigValueId(o.Session.Mode))),
		registry.AddOverride(modeOverride()),
		registry.AddCommand(modeCommand()),
	); err != nil {
		return nil, err
	}
	if o.Supports.ContextOverrides {
		if err := errors.Join(
			registry.AddOverride(pathsOverride("context_files", func(turn *loop.Turn, paths []string) { turn.ContextFiles = paths })),
			registry.AddOverride(pathsOverride("skills_dirs", func(turn *loop.Turn, paths []string) { turn.SkillsDirs = paths })),
		); err != nil {
			return nil, err
		}
	}
	return &Agent{
		version:  o.Version,
		turn:     o.Turn,
		logger:   o.Logger,
		tracer:   o.Tracer,
		turns:    turns,
		registry: registry,
		supports: o.Supports,
		sessions: map[acp.SessionId]*session{},
	}, nil
}

// SetClient sets the client the agent sends session updates to. The agent is
// needed to make the connection and the connection is the client, so this
// is a separate step.
func (a *Agent) SetClient(c Client) { a.client = c }

// Gate returns what the client advertised in initialize. Before initialize
// it is the gate of a client that advertised nothing.
func (a *Agent) Gate() Gate {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gate
}

// Initialize records what the client supports and answers with the protocol
// version and the agent's capabilities.
func (a *Agent) Initialize(_ context.Context, p acp.InitializeRequest) (acp.InitializeResponse, error) {
	a.mu.Lock()
	a.initialized = true
	a.gate = NewGate(p.ClientCapabilities)
	a.mu.Unlock()

	sessions := acp.SessionCapabilities{
		Close:   &acp.SessionCloseCapabilities{},
		Options: &acp.SessionOptionsCapabilities{},
		Status:  &acp.SessionStatusCapabilities{},
		Title:   &acp.SessionTitleCapabilities{},
	}
	if a.supports.AdditionalDirectories {
		sessions.AdditionalDirectories = &acp.SessionAdditionalDirectoriesCapabilities{}
	}
	if a.supports.SystemPrompt {
		sessions.SystemPrompt = agentctx.Capabilities()
	}
	capabilities := acp.AgentCapabilities{SessionCapabilities: sessions, PromptCapabilities: a.supports.Prompt}
	if mcp := a.supports.MCP; mcp != nil {
		// The ACP transport for MCP is not implemented, whatever the
		// wiring says.
		capabilities.McpCapabilities = acp.McpCapabilities{Http: mcp.Http, Sse: mcp.Sse}
	}
	return acp.InitializeResponse{
		// The only version there is to offer: the client's own when it asked
		// for it, and otherwise the latest this agent supports.
		ProtocolVersion:   acp.ProtocolVersionNumber,
		AgentInfo:         &acp.Implementation{Name: "acpharness", Version: a.version},
		AgentCapabilities: capabilities,
	}, nil
}

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
	var mode *agentctx.UnsupportedModeError
	var content *agentctx.ContentError
	switch {
	case errors.As(err, &mode), errors.As(err, &content):
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

// NewSession creates a session in the request's working directory.
func (a *Agent) NewSession(_ context.Context, p acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	if err := a.ready(); err != nil {
		return acp.NewSessionResponse{}, err
	}
	if err := errors.Join(a.checkDirs(p.Cwd, p.AdditionalDirectories), a.checkSystemPrompt(p.SystemPrompt), a.checkMCP(p.McpServers)); err != nil {
		// The first refusal, when there is one, is the answer.
		var refused *acp.RequestError
		if errors.As(err, &refused) {
			return acp.NewSessionResponse{}, refused
		}
		return acp.NewSessionResponse{}, err
	}
	gate := a.Gate()
	values := a.registry.defaults()
	perTurn, err := a.registry.applyMeta(p.Meta, values)
	if err != nil {
		return acp.NewSessionResponse{}, invalid("%v", err)
	}
	// Initial values are applied in order. Unlike session/set_config_option,
	// the protocol has an entry that does not apply ignored, and only a
	// repeated id rejected.
	seen := map[acp.SessionConfigId]bool{}
	for _, initial := range p.ConfigOptions {
		var id acp.SessionConfigId
		var value Value
		switch {
		case initial.Boolean != nil:
			id, value = initial.Boolean.ConfigId, Value{On: initial.Boolean.Value}
		case initial.ValueId != nil:
			id, value = initial.ValueId.ConfigId, Value{ID: initial.ValueId.Value}
		default:
			return acp.NewSessionResponse{}, invalid("configOptions: an entry has no value")
		}
		if seen[id] {
			return acp.NewSessionResponse{}, invalid("configOptions: %q is set twice", id)
		}
		seen[id] = true
		option := a.registry.option(id)
		if option == nil || option.Boolean != (initial.Boolean != nil) || option.Boolean && !gate.BooleanOptions() || option.check(value) != nil {
			a.logger.Debug("initial config option ignored", "option", id)
			continue
		}
		values[id] = value
	}

	s := newSession(acp.SessionId(uuid.NewV7().String()), p.Cwd)
	s.dirs, s.systemPrompt, s.mcpServers = p.AdditionalDirectories, p.SystemPrompt, p.McpServers
	s.values, s.perTurn = values, perTurn
	a.mu.Lock()
	a.sessions[s.id] = s
	a.mu.Unlock()
	a.logger.Info("session created", "session", s.id, "cwd", s.cwd)
	return acp.NewSessionResponse{
		SessionId:     s.id,
		Modes:         modeState(a.registry.option(ModeOption), values[ModeOption].ID),
		ConfigOptions: a.registry.configOptions(values, gate.BooleanOptions()),
	}, nil
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

// send sends one update for a session, unless it is of a type the client
// did not advertise.
func (a *Agent) send(ctx context.Context, s *session, update acp.SessionUpdate) error {
	if !a.Gate().Sendable(update) {
		return nil
	}
	return a.client.SessionUpdate(ctx, acp.SessionNotification{SessionId: s.id, Update: update})
}

// announce tells the client the session's commands, once. The update names
// the session, so it cannot go out before the session/new response, and a
// handler cannot write after its own response: it goes out with the first
// later request for the session.
func (a *Agent) announce(ctx context.Context, s *session) error {
	s.mu.Lock()
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

// setOption changes a config option of a session and tells the client what
// its request did not already tell it. inResponse says the caller answers
// with the config options, as session/set_config_option does.
func (a *Agent) setOption(ctx context.Context, s *session, id acp.SessionConfigId, value Value, boolean, inResponse bool) error {
	gate := a.Gate()
	option := a.registry.option(id)
	switch {
	case option == nil, option.Boolean && !gate.BooleanOptions():
		return invalid("unknown config option %q", id)
	case option.Boolean != boolean:
		return invalid("config option %q is not of that type", id)
	}
	if err := option.check(value); err != nil {
		return invalid("%v", err)
	}
	// Held while the updates are sent, so that two changes reach the client
	// in the order they were made.
	s.setMu.Lock()
	defer s.setMu.Unlock()
	s.mu.Lock()
	changed := s.values[id] != value
	s.values[id] = value
	if changed && id == ModeOption {
		// "Allow always" was given in the mode it was asked in.
		clear(s.allowed)
	}
	values := maps.Clone(s.values)
	s.mu.Unlock()
	if !changed {
		return nil
	}
	if id == ModeOption {
		if err := a.send(ctx, s, acp.SessionUpdate{CurrentModeUpdate: &acp.SessionCurrentModeUpdate{
			SessionUpdate: "current_mode_update", CurrentModeId: acp.SessionModeId(value.ID),
		}}); err != nil {
			return err
		}
	}
	if inResponse {
		return nil
	}
	return a.send(ctx, s, acp.SessionUpdate{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{
		SessionUpdate: "config_option_update", ConfigOptions: a.registry.configOptions(values, gate.BooleanOptions()),
	}})
}

// permit decides whether one tool call of the prompt with the given arrival
// may run. See loop.Turn.Permit.
func (a *Agent) permit(ctx context.Context, s *session, arrival uint64, tool string, effect tools.Effect, call acp.ToolCallUpdate) (bool, error) {
	if effect == tools.ReadOnly {
		return true, nil
	}
	s.mu.Lock()
	ask := s.values[ModeOption].ID == ModeAsk && !s.allowed[tool]
	s.mu.Unlock()
	if !ask {
		return true, nil
	}
	const allowOnce, allowAlways, reject = "allow_once", "allow_always", "reject_once"
	// When ctx ends, the connection asks the client to cancel the request
	// and this returns ctx's error.
	resp, err := a.client.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: s.id,
		ToolCall:  call,
		Options: []acp.PermissionOption{
			{OptionId: allowOnce, Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
			{OptionId: allowAlways, Name: "Always allow " + tool, Kind: acp.PermissionOptionKindAllowAlways},
			{OptionId: reject, Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
		},
	})
	if err != nil {
		return false, fmt.Errorf("session: permission request for %s: %w", tool, err)
	}
	switch outcome := resp.Outcome; {
	case outcome.Cancelled != nil:
		// The client answers this way for a turn it has cancelled. Its
		// session/cancel may still be on the way, so the turn is stopped
		// here and answers as cancelled either way.
		s.stop(arrival)
		return false, context.Canceled
	case outcome.Selected == nil:
		return false, fmt.Errorf("session: permission response for %s has no outcome", tool)
	case outcome.Selected.OptionId == allowOnce:
		return true, nil
	case outcome.Selected.OptionId == allowAlways:
		s.mu.Lock()
		s.allowed[tool] = true
		s.mu.Unlock()
		return true, nil
	case outcome.Selected.OptionId == reject:
		return false, nil
	default:
		return false, fmt.Errorf("session: permission response for %s selects %q, which was not offered", tool, outcome.Selected.OptionId)
	}
}

// checkPrompt reports a content block of a type that is not advertised.
func (a *Agent) checkPrompt(prompt []acp.ContentBlock) error {
	for i, block := range prompt {
		switch {
		case block.Image != nil && !a.supports.Prompt.Image:
			return invalid("prompt[%d]: image content is not supported", i)
		case block.Audio != nil && !a.supports.Prompt.Audio:
			return invalid("prompt[%d]: audio content is not supported", i)
		case block.Resource != nil && !a.supports.Prompt.EmbeddedContext:
			return invalid("prompt[%d]: embedded resources are not supported", i)
		}
	}
	return nil
}

// Prompt runs one turn. A prompt for a session whose turn is still running
// cancels that turn and runs once it has stopped.
func (a *Agent) Prompt(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	s, err := a.session(p.SessionId)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	if err := a.checkPrompt(p.Prompt); err != nil {
		return acp.PromptResponse{}, err
	}
	arrival, ok := acp.Arrival(ctx)
	if !ok {
		return acp.PromptResponse{}, errors.New("session: prompt did not come through a connection")
	}
	if err := a.announce(ctx, s); err != nil {
		return acp.PromptResponse{}, err
	}

	ctx, span := a.tracer.Start(telemetry.WithRemoteParent(ctx, p.Meta), acp.AgentMethodSessionPrompt,
		trace.WithAttributes(attribute.String("acp.session.id", string(s.id))))
	defer span.End()

	result, err := s.run(ctx, arrival, func(ctx context.Context) (loop.Result, error) {
		gate := a.Gate()
		turn := loop.Turn{
			SessionID:    s.id,
			Cwd:          s.cwd,
			Prompt:       p.Prompt,
			Dirs:         s.dirs,
			SystemPrompt: s.systemPrompt,
			McpServers:   s.mcpServers,
			Client:       gate.Client(),
			Sendable:     gate.Sendable,
			Update:       func(ctx context.Context, update acp.SessionUpdate) error { return a.send(ctx, s, update) },
			Permit: func(ctx context.Context, tool string, effect tools.Effect, call acp.ToolCallUpdate) (bool, error) {
				return a.permit(ctx, s, arrival, tool, effect, call)
			},
		}
		for id, value := range s.options() {
			if apply := a.registry.option(id).Apply; apply != nil {
				apply(&turn, value)
			}
		}
		for _, apply := range s.perTurn {
			apply(&turn)
		}
		var result loop.Result
		var err error
		if command, args, ok := a.registry.command(p.Prompt); ok && command.Run != nil {
			result, err = command.Run(ctx, CommandCall{
				Turn: turn, Args: args, Option: s.option,
				SetOption: func(ctx context.Context, id acp.SessionConfigId, value Value) error {
					option := a.registry.option(id)
					return a.setOption(ctx, s, id, value, option != nil && option.Boolean, false)
				},
			})
		} else {
			result, err = a.turn(ctx, turn)
		}
		if err != nil && ctx.Err() != nil {
			// run answers this with the cancelled stop reason. The error
			// is kept here in case it was not the cancellation's.
			a.logger.DebugContext(ctx, "cancelled turn returned an error", "session", s.id, "err", err)
		}
		return result, err
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		a.logger.ErrorContext(ctx, "turn failed", "session", s.id, "err", err)
		return acp.PromptResponse{}, stopError(err)
	}
	stop := attribute.String("acp.stop_reason", string(result.StopReason))
	span.SetAttributes(stop)
	a.turns.Add(ctx, 1, metric.WithAttributes(stop))
	resp := acp.PromptResponse{StopReason: result.StopReason, Usage: result.Usage}
	if result.Stop != nil {
		resp.Meta = harness.Meta(nil, harness.Addition{Stop: result.Stop})
	}
	return resp, nil
}

// stopError turns a turn's failure into the error the client gets. A
// failure with a typed reason carries it in the error's data, so the client
// does not have to read the message.
func stopError(err error) error {
	// A hook refused the prompt and the turn did not start. That is the
	// request being refused, not a failure inside the agent.
	var denied *loop.DeniedError
	if errors.As(err, &denied) {
		return acp.NewInvalidRequest(map[string]any{"error": err.Error(), "deniedReason": denied.Reason})
	}
	var stopped *loop.StopError
	if !errors.As(err, &stopped) {
		return err
	}
	data := harness.Meta(map[string]any{"error": err.Error()}, harness.Addition{Stop: &stopped.Stop})
	if stopped.Stop.Kind == harness.StopAuth {
		return acp.NewAuthRequired(data)
	}
	return acp.NewInternalError(data)
}

// Cancel cancels the session's prompts that arrived before it.
func (a *Agent) Cancel(ctx context.Context, p acp.CancelNotification) error {
	s, err := a.session(p.SessionId)
	if err != nil {
		return err
	}
	arrival, ok := acp.Arrival(ctx)
	if !ok {
		return errors.New("session: cancel did not come through a connection")
	}
	s.cancel(arrival)
	return nil
}

// CloseSession cancels the session's turn, waits for it to stop and forgets
// the session. Closing a session that does not exist succeeds.
func (a *Agent) CloseSession(ctx context.Context, p acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	if err := a.ready(); err != nil {
		return acp.CloseSessionResponse{}, err
	}
	a.mu.Lock()
	s := a.sessions[p.SessionId]
	delete(a.sessions, p.SessionId)
	a.mu.Unlock()
	if s == nil {
		return acp.CloseSessionResponse{}, nil
	}
	if err := s.close(ctx); err != nil {
		return acp.CloseSessionResponse{}, err
	}
	a.logger.Info("session closed", "session", s.id)
	return acp.CloseSessionResponse{}, nil
}

// Authenticate rejects every method: the agent advertises none.
func (a *Agent) Authenticate(_ context.Context, p acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, acp.NewInvalidParams(map[string]any{"error": "unknown authentication method", "methodId": p.MethodId})
}

// Logout is not advertised.
func (a *Agent) Logout(context.Context, acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, acp.NewMethodNotFound(acp.AgentMethodLogout)
}

// DeleteSession is not advertised.
func (a *Agent) DeleteSession(context.Context, acp.DeleteSessionRequest) (acp.DeleteSessionResponse, error) {
	return acp.DeleteSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionDelete)
}

// ListSessions is not advertised.
func (a *Agent) ListSessions(context.Context, acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionList)
}

// ResumeSession is not advertised.
func (a *Agent) ResumeSession(context.Context, acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionResume)
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
	if err := a.setOption(ctx, s, id, value, p.Boolean != nil, true); err != nil {
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
	return acp.SetSessionModeResponse{}, a.setOption(ctx, s, ModeOption, Value{ID: acp.SessionConfigValueId(p.ModeId)}, false, false)
}

// UnstableSessionStatus says whether the session is in memory. It changes
// nothing. Without a store a session is open or does not exist.
func (a *Agent) UnstableSessionStatus(_ context.Context, p acp.UnstableSessionStatusRequest) (acp.UnstableSessionStatusResponse, error) {
	if _, err := a.session(acp.SessionId(p.SessionId)); err != nil {
		return acp.UnstableSessionStatusResponse{}, err
	}
	return acp.UnstableSessionStatusResponse{Status: acp.UnstableSessionStatusOpen}, nil
}

// UnstableSessionOptions answers with the modes and config options a new
// session would start with. It creates nothing.
func (a *Agent) UnstableSessionOptions(_ context.Context, p acp.UnstableSessionOptionsRequest) (acp.UnstableSessionOptionsResponse, error) {
	if err := a.ready(); err != nil {
		return acp.UnstableSessionOptionsResponse{}, err
	}
	if err := a.checkDirs(p.Cwd, p.AdditionalDirectories); err != nil {
		return acp.UnstableSessionOptionsResponse{}, err
	}
	if p.SessionId != nil {
		// What load or resume would answer, and neither is implemented.
		if _, err := a.session(acp.SessionId(*p.SessionId)); err != nil {
			return acp.UnstableSessionOptionsResponse{}, err
		}
		return acp.UnstableSessionOptionsResponse{}, invalid("sessionId needs session/load or session/resume, which are not supported")
	}
	values := a.registry.defaults()
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

// UnstableSetSessionTitle sets the session's title and tells the client.
// The title lives as long as the session does in this process.
func (a *Agent) UnstableSetSessionTitle(ctx context.Context, p acp.UnstableSetSessionTitleRequest) (acp.UnstableSetSessionTitleResponse, error) {
	s, err := a.session(acp.SessionId(p.SessionId))
	if err != nil {
		return acp.UnstableSetSessionTitleResponse{}, err
	}
	title := strings.TrimSpace(p.Title)
	if title == "" {
		return acp.UnstableSetSessionTitleResponse{}, invalid("title is empty")
	}
	if err := a.announce(ctx, s); err != nil {
		return acp.UnstableSetSessionTitleResponse{}, err
	}
	s.mu.Lock()
	s.title = title
	s.mu.Unlock()
	err = a.send(ctx, s, acp.SessionUpdate{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{
		SessionUpdate: "session_info_update", Title: &title,
	}})
	return acp.UnstableSetSessionTitleResponse{Title: title}, err
}

// ready reports a request that came before initialize.
func (a *Agent) ready() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.initialized {
		return acp.NewInvalidRequest(map[string]any{"error": "initialize must be called first"})
	}
	return nil
}

// session returns an active session, or the error for an unknown one.
func (a *Agent) session(id acp.SessionId) (*session, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[id]
	if s == nil {
		return nil, unknownSession(id)
	}
	return s, nil
}

func unknownSession(id acp.SessionId) *acp.RequestError {
	return acp.NewResourceNotFound(map[string]any{"sessionId": id})
}
