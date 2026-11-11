// Package loop runs one prompt turn.
package loop

import (
	"context"
	"fmt"
	"strings"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/pkg/acp/harness"
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
}

// Stub answers a prompt without a model: it says so and repeats the prompt's
// text. It stands in until the first provider exists, so that a client can
// exercise the protocol end to end.
func Stub(ctx context.Context, t Turn) (Result, error) {
	var text strings.Builder
	other := 0
	for _, block := range t.Prompt {
		if block.Text == nil {
			other++
			continue
		}
		text.WriteString(block.Text.Text)
		text.WriteString("\n")
	}
	reply := fmt.Sprintf("acpharness has no model provider yet. Your prompt had %d content blocks that are not text, and this text:\n%s", other, text.String())
	update := acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
		Content: acp.ContentBlock{Text: &acp.ContentBlockText{Type: "text", Text: reply}},
	}}
	if err := t.Update(ctx, update); err != nil {
		return Result{}, err
	}
	return Result{StopReason: acp.StopReasonEndTurn}, nil
}
