// Package loop runs one prompt turn.
package loop

import (
	"context"
	"fmt"
	"strings"

	"github.com/gfx-labs/acpharness/src/acp"
)

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
func Stub(ctx context.Context, t Turn) (acp.StopReason, error) {
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
		return "", err
	}
	return acp.StopReasonEndTurn, nil
}
