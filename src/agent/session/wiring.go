package session

import (
	"context"
	"log/slog"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/store"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
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
	// Nil replays with an empty mapper. A supplied mapper stays immutable until Close.
	Mapper *loop.Mapper
}

// Client is what the agent calls on the ACP client.
type Client interface {
	SessionUpdate(ctx context.Context, params acp.SessionNotification) error
	RequestPermission(ctx context.Context, params acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error)
	CreateElicitation(ctx context.Context, params acp.CreateElicitationRequest) (acp.CreateElicitationResponse, error)
	CompleteElicitation(ctx context.Context, params acp.CompleteElicitationNotification) error
	// UnstableMessageMcp sends one MCP request to a server the client
	// provides over this connection (mcp/message).
	UnstableMessageMcp(ctx context.Context, params acp.UnstableMessageMcpRequest) (acp.UnstableMessageMcpResponse, error)
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
	// _meta.towa on session/new.
	ContextOverrides bool
	// MCP: the turn connects to loop.Turn.McpServers over stdio, and over
	// the transports named here. The acp transport is carried on the
	// session's own connection, through loop.Turn.MessageMcp. Nil means it
	// connects to none.
	MCP *acp.McpCapabilities
	// Prompt names the content types beyond text and resource links that
	// the provider takes in a prompt.
	Prompt acp.PromptCapabilities
	// TerminalAuth are the logins a client can run in a terminal, by
	// appending a method's args to the command it starts the agent with.
	// They are advertised to a client that takes terminal auth methods.
	TerminalAuth []TerminalLogin
	// LoggedIn reports whether an account has a stored login, so that
	// authenticate with a terminal method says whether its login was run.
	LoggedIn func(account string) (bool, error)
}

// TerminalLogin is a terminal auth method and the account its login stores.
type TerminalLogin struct {
	Account string
	Method  acp.AuthMethodTerminalInline
}

// Options configures a Host. Every field but Registry, Supports and Store
// is required.
type Options struct {
	// Version is reported to the client as the agent's version.
	Version string
	// Open builds a runtime from the session's immutable base turn. The
	// base has no prompt. Its permission callback uses the calling prompt's
	// context. The runtime's context lives
	// until the session closes, not until session/new or load returns.
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
}
