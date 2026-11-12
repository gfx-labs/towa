// Package tools is the contract between the agent loop and the tools a
// model can call. The tools are in its subpackages: one package per
// capability, and core, which gives each capability its name, schema,
// output text and error text.
package tools

import (
	"context"
	"encoding/json/jsontext"
	"io"

	"github.com/gfx-labs/towa/pkg/acp"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/transcript"
)

// Tool is one tool as the model is offered it.
type Tool struct {
	// Spec is what the model is told about the tool.
	Spec provider.Tool
	// Guidance is what the system prompt says about when to use the tool,
	// one line each. It is in the prompt only while the tool is enabled,
	// so the prompt never names a tool the model does not have.
	Guidance []string
	// Summary is the tool's line in the prompt's list of tools: a few
	// words on what it does. A tool without one, such as an MCP server's,
	// is not in the list.
	Summary string
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
	// called any number of times, and not at all. Each call carries only
	// new content: the loop appends it to what it has already sent, since
	// the content of an ACP tool call update replaces what the client has.
	// Progress is nil when nobody is listening, so a tool checks before
	// calling it.
	Progress func(ctx context.Context, content []acp.ToolCallContent) error
	// PutBlob keeps content in the session's store, outside the transcript,
	// and returns the reference to put in a result's Parts beside the text
	// the model gets. Output gating uses it for the full output it left out.
	// PutBlob is nil when the session has no store. The full output is then
	// not kept, and the model can still get it by running the call again.
	PutBlob func(ctx context.Context, mimeType string, content io.Reader) (transcript.Stored, error)
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

// Set is a session's tools, in the order the model is told about them.
type Set struct {
	Tools []Tool
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
