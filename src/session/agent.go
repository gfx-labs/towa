// Package session implements the ACP agent: it owns the sessions of one
// connection and runs their prompt turns.
package session

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"uuid"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/gfx-labs/acpharness/src/acp"
	"github.com/gfx-labs/acpharness/src/loop"
	"github.com/gfx-labs/acpharness/src/telemetry"
)

// TurnFunc runs one prompt turn and says why it stopped. It returns when ctx
// is cancelled, with the cancelled stop reason or with an error.
type TurnFunc func(ctx context.Context, turn loop.Turn) (acp.StopReason, error)

// Client is what the agent calls on the ACP client.
type Client interface {
	SessionUpdate(ctx context.Context, params acp.SessionNotification) error
}

// Options configures an Agent. Every field is required.
type Options struct {
	// Version is reported to the client as the agent's version.
	Version string
	Turn    TurnFunc
	Logger  *slog.Logger
	Tracer  trace.Tracer
	Meter   metric.Meter
}

// Agent is the ACP agent for one connection. It advertises only what it
// implements: a method behind a capability it does not advertise answers
// "Method not found".
type Agent struct {
	version string
	turn    TurnFunc
	logger  *slog.Logger
	tracer  trace.Tracer
	turns   metric.Int64Counter
	client  Client

	mu          sync.Mutex
	initialized bool
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
	return &Agent{
		version:  o.Version,
		turn:     o.Turn,
		logger:   o.Logger,
		tracer:   o.Tracer,
		turns:    turns,
		sessions: map[acp.SessionId]*session{},
	}, nil
}

// SetClient sets the client the agent sends session updates to. The agent is
// needed to make the connection and the connection is the client, so this
// is a separate step.
func (a *Agent) SetClient(c Client) { a.client = c }

// Initialize answers with the protocol version and the agent's capabilities.
func (a *Agent) Initialize(_ context.Context, _ acp.InitializeRequest) (acp.InitializeResponse, error) {
	a.mu.Lock()
	a.initialized = true
	a.mu.Unlock()
	return acp.InitializeResponse{
		// The only version there is to offer: the client's own when it asked
		// for it, and otherwise the latest this agent supports.
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentInfo:       &acp.Implementation{Name: "acpharness", Version: a.version},
		AgentCapabilities: acp.AgentCapabilities{
			SessionCapabilities: acp.SessionCapabilities{Close: &acp.SessionCloseCapabilities{}},
		},
	}, nil
}

// NewSession creates a session in the request's working directory.
func (a *Agent) NewSession(_ context.Context, p acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	if err := a.ready(); err != nil {
		return acp.NewSessionResponse{}, err
	}
	if !filepath.IsAbs(p.Cwd) {
		return acp.NewSessionResponse{}, acp.NewInvalidParams(map[string]any{"error": "cwd must be an absolute path", "cwd": p.Cwd})
	}
	// A client may send these only when the matching capability is
	// advertised. Accepting them would drop them without a word.
	for _, field := range []struct {
		name string
		sent bool
	}{
		{"mcpServers", len(p.McpServers) > 0},
		{"additionalDirectories", len(p.AdditionalDirectories) > 0},
		{"systemPrompt", p.SystemPrompt != nil},
		{"configOptions", len(p.ConfigOptions) > 0},
	} {
		if field.sent {
			return acp.NewSessionResponse{}, acp.NewInvalidParams(map[string]any{"error": field.name + " is not supported yet"})
		}
	}

	s := newSession(acp.SessionId(uuid.NewV7().String()), p.Cwd)
	a.mu.Lock()
	a.sessions[s.id] = s
	a.mu.Unlock()
	a.logger.Info("session created", "session", s.id, "cwd", s.cwd)
	return acp.NewSessionResponse{SessionId: s.id}, nil
}

// Prompt runs one turn. A prompt for a session whose turn is still running
// cancels that turn and runs once it has stopped.
func (a *Agent) Prompt(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	s, err := a.session(p.SessionId)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	arrival, ok := acp.Arrival(ctx)
	if !ok {
		return acp.PromptResponse{}, errors.New("session: prompt did not come through a connection")
	}

	ctx, span := a.tracer.Start(telemetry.WithRemoteParent(ctx, p.Meta), acp.AgentMethodSessionPrompt,
		trace.WithAttributes(attribute.String("acp.session.id", string(s.id))))
	defer span.End()

	reason, err := s.run(ctx, arrival, func(ctx context.Context) (acp.StopReason, error) {
		reason, err := a.turn(ctx, loop.Turn{
			SessionID: s.id,
			Cwd:       s.cwd,
			Prompt:    p.Prompt,
			Update: func(ctx context.Context, update acp.SessionUpdate) error {
				return a.client.SessionUpdate(ctx, acp.SessionNotification{SessionId: s.id, Update: update})
			},
		})
		if err != nil && ctx.Err() != nil {
			// run answers this with the cancelled stop reason. The error
			// is kept here in case it was not the cancellation's.
			a.logger.DebugContext(ctx, "cancelled turn returned an error", "session", s.id, "err", err)
		}
		return reason, err
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		a.logger.ErrorContext(ctx, "turn failed", "session", s.id, "err", err)
		return acp.PromptResponse{}, err
	}
	stop := attribute.String("acp.stop_reason", string(reason))
	span.SetAttributes(stop)
	a.turns.Add(ctx, 1, metric.WithAttributes(stop))
	return acp.PromptResponse{StopReason: reason}, nil
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

// SetSessionConfigOption is not advertised: sessions have no config options.
func (a *Agent) SetSessionConfigOption(context.Context, acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetConfigOption)
}

// SetSessionMode is not advertised: sessions have no modes.
func (a *Agent) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetMode)
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
