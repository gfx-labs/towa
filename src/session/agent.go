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
	"time"
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
	"github.com/gfx-labs/acpharness/src/store"
	"github.com/gfx-labs/acpharness/src/telemetry"
	"github.com/gfx-labs/acpharness/src/tools"
)

// TurnFunc runs one prompt turn and says how it ended. It returns when ctx
// is cancelled, with the cancelled stop reason or with an error. A failure
// the client can act on is a *loop.StopError.
type TurnFunc func(ctx context.Context, turn loop.Turn) (loop.Result, error)

// Runtime belongs to one attached session. Close runs after its turns stop,
// including when Open returns both a partial Runtime and an error.
type Runtime struct {
	Turn  TurnFunc
	Close func(context.Context) error
	// Mapper describes replay with this runtime's tools and model metadata.
	// Nil uses Options.Mapper. A supplied mapper stays immutable until Close.
	Mapper *loop.Mapper
}

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
	// Open builds a runtime from the session's immutable base turn. The
	// base has no prompt. Its permission callback uses the calling prompt's
	// context. The runtime's context lives
	// until the session closes, not until session/new or load returns.
	// Nil preserves the process-wide Turn function.
	Open   func(context.Context, loop.Turn) (Runtime, error)
	Logger *slog.Logger
	Tracer trace.Tracer
	Meter  metric.Meter
	// Session is the session section of the configuration.
	Session config.Session
	// Registry holds the config options, commands and overrides other
	// packages add. Nil means none. The mode option, its override and its
	// command are added to a copy of it.
	Registry *Registry
	Supports Supports
	// Store keeps sessions, so that another process can load them. Nil
	// means sessions live in this process only: the methods that need a
	// store then answer Method not found and are not advertised.
	Store store.Store
	// Mapper is the replay mapper for legacy Turn callers and runtimes
	// that leave Runtime.Mapper nil. A runtime mapper takes precedence.
	Mapper loop.Mapper
}

// Agent is the ACP agent for one connection. It advertises only what it
// implements: a method behind a capability it does not advertise answers
// "Method not found".
//
// What a process has once (the store, the registry, the table of live
// sessions) is in the Host the agent was made from. The agent itself holds
// what belongs to its connection: the client and what the client
// advertised.
type Agent struct {
	*shared
	client Client
	// ended is set once the connection has ended. The host's table lock
	// guards it.
	ended bool

	mu          sync.Mutex
	initialized bool
	gate        Gate
}

var _ acp.Agent = (*Agent)(nil)

// NewAgent returns the agent of a process that serves one connection, such
// as standard input and output. Call SetClient before the connection is
// served.
func NewAgent(o Options) (*Agent, error) {
	h, err := NewHost(o)
	if err != nil {
		return nil, err
	}
	return h.NewAgent(), nil
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
	a.liveMu.Lock()
	ended := a.ended || a.ending
	a.liveMu.Unlock()
	if ended {
		return acp.InitializeResponse{}, acp.NewInvalidRequest(map[string]any{"error": "connection or host has ended"})
	}
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
	if a.store != nil {
		capabilities.LoadSession = true
		capabilities.SessionCapabilities.Resume = &acp.SessionResumeCapabilities{}
		capabilities.SessionCapabilities.List = &acp.SessionListCapabilities{}
		capabilities.SessionCapabilities.Delete = &acp.SessionDeleteCapabilities{}
		capabilities.SessionCapabilities.Fork = &acp.SessionForkCapabilities{}
		capabilities.SessionCapabilities.Archive = &acp.SessionArchiveCapabilities{}
	}
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
func (a *Agent) NewSession(ctx context.Context, p acp.NewSessionRequest) (acp.NewSessionResponse, error) {
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
// request's _meta.acpharness, the request's configOptions.
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
	if s.closed {
		s.mu.Unlock()
		return unknownSession(s.id)
	}
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
	if err := a.persist(ctx, s); err != nil {
		return err
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
			result, err = s.runtime.Turn(ctx, turn)
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
	// The loop reports a prompt a hook refused as its own error type. It is
	// carried to the client as a typed stop like the others.
	var denied *loop.DeniedError
	var stopped *loop.StopError
	switch {
	case errors.As(err, &denied):
		stopped = &loop.StopError{Stop: harness.Stop{Kind: harness.StopDenied, Message: denied.Reason}}
	case !errors.As(err, &stopped):
		return err
	}
	data := harness.Meta(map[string]any{"error": err.Error()}, harness.Addition{Stop: &stopped.Stop})
	// The turn did not start: the request was refused, the agent did not
	// fail.
	if stopped.Stop.Kind == harness.StopDenied {
		return acp.NewInvalidRequest(data)
	}
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
	// A session that is live on another connection is not this one's to
	// close, and is unknown to it like any session it has not loaded.
	defer a.lock(p.SessionId)()
	s, err := a.drain(ctx, p.SessionId, a)
	if err != nil {
		return acp.CloseSessionResponse{}, err
	}
	if s == nil {
		return acp.CloseSessionResponse{}, nil
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

// UnstableSessionStatus says whether the session is in memory, on any
// connection, or only in the store. It changes nothing.
func (a *Agent) UnstableSessionStatus(ctx context.Context, p acp.UnstableSessionStatusRequest) (acp.UnstableSessionStatusResponse, error) {
	if err := a.ready(); err != nil {
		return acp.UnstableSessionStatusResponse{}, err
	}
	id := acp.SessionId(p.SessionId)
	if s, _ := a.lookup(id); s != nil {
		return acp.UnstableSessionStatusResponse{Status: acp.UnstableSessionStatusOpen}, nil
	}
	if a.store == nil {
		return acp.UnstableSessionStatusResponse{}, unknownSession(id)
	}
	if _, err := a.storedMeta(ctx, id); err != nil {
		return acp.UnstableSessionStatusResponse{}, err
	}
	return acp.UnstableSessionStatusResponse{Status: acp.UnstableSessionStatusSaved}, nil
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

// UnstableSetSessionTitle sets a session's title. The session need not be
// live on this connection: one that is live on another has that
// connection's client told, and one that is only in the store has the
// title written there. With a store the title is written before the answer.
func (a *Agent) UnstableSetSessionTitle(ctx context.Context, p acp.UnstableSetSessionTitleRequest) (acp.UnstableSetSessionTitleResponse, error) {
	if err := a.ready(); err != nil {
		return acp.UnstableSetSessionTitleResponse{}, err
	}
	id := acp.SessionId(p.SessionId)
	title := strings.TrimSpace(p.Title)
	// Held so that the session is not loaded on a connection between the
	// lookup and the write, which would then keep the old title.
	defer a.lock(id)()
	s, owner := a.lookup(id)
	if s == nil {
		if a.store == nil {
			return acp.UnstableSetSessionTitleResponse{}, unknownSession(id)
		}
		meta, err := a.storedMeta(ctx, id)
		switch {
		case err != nil:
			return acp.UnstableSetSessionTitleResponse{}, err
		case title == "":
			return acp.UnstableSetSessionTitleResponse{}, invalid("title is empty")
		}
		meta.Title = title
		if err := a.store.SetMeta(ctx, meta); err != nil {
			return acp.UnstableSetSessionTitleResponse{}, storeError(id, err)
		}
		return acp.UnstableSetSessionTitleResponse{Title: title}, nil
	}
	if title == "" {
		return acp.UnstableSetSessionTitleResponse{}, invalid("title is empty")
	}
	if err := owner.announce(ctx, s); err != nil {
		return acp.UnstableSetSessionTitleResponse{}, err
	}
	s.setMu.Lock()
	defer s.setMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return acp.UnstableSetSessionTitleResponse{}, unknownSession(id)
	}
	s.title = title
	s.mu.Unlock()
	if err := a.persist(ctx, s); err != nil {
		return acp.UnstableSetSessionTitleResponse{}, err
	}
	err := owner.send(ctx, s, acp.SessionUpdate{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{
		SessionUpdate: "session_info_update", Title: &title,
	}})
	return acp.UnstableSetSessionTitleResponse{Title: title}, err
}

// ready reports a request that came before initialize.
func (a *Agent) ready() error {
	a.liveMu.Lock()
	ended := a.ended || a.ending
	a.liveMu.Unlock()
	if ended {
		return acp.NewInvalidRequest(map[string]any{"error": "connection or host has ended"})
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.initialized {
		return acp.NewInvalidRequest(map[string]any{"error": "initialize must be called first"})
	}
	return nil
}

// session returns a session that is live on this connection, or the error
// for an unknown one.
func (a *Agent) session(id acp.SessionId) (*session, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	// A session that is live on another connection is unknown to this one
	// until it loads or resumes it.
	s, owner := a.lookup(id)
	if s == nil || owner != a {
		return nil, unknownSession(id)
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, unknownSession(id)
	}
	return s, nil
}

func unknownSession(id acp.SessionId) *acp.RequestError {
	return acp.NewResourceNotFound(map[string]any{"sessionId": id})
}
