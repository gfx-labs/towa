// Package loop runs one prompt turn.
package loop

import (
	"context"
	"fmt"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/agent/harness"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/tools"
)

// Result is how a turn ended.
type Result struct {
	StopReason acp.StopReason
	// Usage is what the turn's model requests used in total. It is nil when
	// no request was made.
	Usage *acp.Usage
	// Stop is set when the turn ended for a reason other than the model
	// ending it, and still has a stop reason to answer with: a configured
	// limit, answered as max_turn_requests.
	Stop *harness.Stop
	// Final is the text the model wrote in the turn's last response, its
	// text items joined. It is empty when the last response had no text, or
	// no response arrived. A caller that wants only the answer, such as a
	// subagent's parent, reads it here and not from the updates.
	Final string
}

// StopError is a turn that failed for a reason the client can act on: a rate
// limit, a rejected credential, an overloaded provider, a full context. The
// prompt is answered with an error whose data carries Stop.
type StopError struct {
	Stop harness.Stop
	Err  error
}

func (e *StopError) Error() string {
	if e.Err == nil {
		return "turn stopped: " + string(e.Stop.Kind)
	}
	return fmt.Sprintf("turn stopped: %s: %v", e.Stop.Kind, e.Err)
}

func (e *StopError) Unwrap() error { return e.Err }

// Turn is one prompt and what a turn needs to answer it.
type Turn struct {
	SessionID acp.SessionId
	// Cwd is the session's working directory, an absolute path.
	Cwd    string
	Prompt []acp.ContentBlock
	// Update sends one session update to the client. Updates reach the
	// client in the order the calls return.
	Update func(ctx context.Context, update acp.SessionUpdate) error

	// Dirs are the session's additional directories, absolute paths.
	Dirs []string
	// SystemPrompt is the client's system prompt from session/new, nil when
	// there is none. The context builder reads it.
	SystemPrompt *acp.SystemPrompt
	// McpServers are the servers the client named on session/new.
	McpServers []acp.McpServer
	// ContextFiles are extra context files the client named on session/new,
	// added to context.extra_files where the turn is assembled.
	ContextFiles []string
	// SkillsDirs are skills directories the client named on session/new,
	// added to context.skills_dirs.
	SkillsDirs []string
	// Model is the name of the configured model the session chose for this
	// turn. Empty means none was chosen and the configured default applies.
	Model string
	// ParentID is the session that spawned this one as a subagent. It is
	// written to the store when the turn creates the session, and must then
	// be a UUID. It is not read for a session that exists.
	ParentID acp.SessionId
	// Limits replaces the configured limits for this turn when it is set:
	// a session's own from session/new, or the ones a parent gives a
	// subagent. The caller validates them.
	Limits *config.Limits
	// Client is what the client advertised in initialize.
	Client acp.ClientCapabilities
	// Sendable reports whether Update will send this update. Update returns
	// nil without sending an update of a type the client did not advertise.
	Sendable func(update acp.SessionUpdate) bool
	// Permit asks whether a tool call may run. It is called before every
	// tool call. False means the call was rejected: the model is told so
	// and the call does not run. An error means the turn was cancelled or
	// the client failed, and stops the turn. A nil Permit allows every call.
	Permit func(ctx context.Context, tool string, effect tools.Effect, call acp.ToolCallUpdate) (bool, error)
}
