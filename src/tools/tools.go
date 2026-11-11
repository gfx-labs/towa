// Package tools is the contract between the agent loop and the tools a
// model can call. The tool sets are in its subpackages: one implementation
// per capability, with an adapter per dialect that gives it the upstream
// name, schema, output text and error text.
package tools

import (
	"context"
	"encoding/json/jsontext"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// Tool is one tool as one dialect presents it.
type Tool struct {
	// Spec is what the model is told about the tool.
	Spec provider.Tool
	// Effect is the most the tool can do. Ask mode asks the client before
	// running a tool that writes or executes.
	Effect Effect
	// Describe says how one call is shown to the client and which paths it
	// writes. It is called before Run, with the same input.
	Describe func(input jsontext.Value) (Description, error)
	// Run runs one call. A failure of the tool itself is a Result with
	// OutcomeFailed and the text the upstream tool would have returned. An
	// error means the harness could not run the tool. The loop reports that
	// to the model as a failed call and logs it.
	Run func(ctx context.Context, call Call) (Result, error)
}

// Effect is what a tool can do to the machine.
type Effect int

// Effects of a tool.
const (
	// ReadOnly tools may run at the same time as each other.
	ReadOnly Effect = iota
	// Writes tools change files. Two calls that write the same path run one
	// after the other.
	Writes
	// Executes tools run commands, whose effects are not known in advance.
	Executes
)

// Description is how one call appears to the client.
type Description struct {
	Title     string
	Kind      acp.ToolKind
	Locations []acp.ToolCallLocation
	// WritePaths are the absolute paths the call will write.
	WritePaths []string
}

// Call is one tool call to run.
type Call struct {
	// ID is the model's id for the call.
	ID string
	// Input is the argument after repair and validation against the schema.
	Input jsontext.Value
	Env   Env
}

// Env is what a tool may use from the session it runs in.
type Env struct {
	SessionID acp.SessionId
	// Cwd is the session's working directory, an absolute path.
	Cwd string
	// Dirs are the session's additional directories, absolute paths.
	Dirs []string
	// Progress sends content to the client while the tool runs. It may be
	// called any number of times, and not at all.
	Progress func(ctx context.Context, content []acp.ToolCallContent) error
}

// Result is the outcome of one call.
type Result struct {
	Outcome transcript.Outcome
	// Parts is what the model gets.
	Parts []transcript.Part
	// Content is what the client is shown, such as a diff. When it is empty
	// the client is shown Parts.
	Content []acp.ToolCallContent
}

// Set is the tools one dialect offers, in the order the model is told about
// them.
type Set struct {
	// Dialect is "claude", "codex" or "generic".
	Dialect string
	Tools   []Tool
}

// Lookup returns the tool with the given name.
func (s Set) Lookup(name string) (Tool, bool) {
	for _, t := range s.Tools {
		if t.Spec.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// Specs returns what the model is told, in order.
func (s Set) Specs() []provider.Tool {
	specs := make([]provider.Tool, len(s.Tools))
	for i, t := range s.Tools {
		specs[i] = t.Spec
	}
	return specs
}
