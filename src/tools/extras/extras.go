// Package extras adapts optional capabilities to the common tools contract.
package extras

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// Names identify extras to the toolset's collision and disable checks.
const (
	NameConversationSearch = "conversation_search"
	NameWebFetch           = "webfetch"
	NameWebSearch          = "websearch"
	NameJevDecision        = "jev_decision"
	NameSkill              = "skill_manage"
	NameBatch              = "batch"
)

// ErrDisabled means configuration has turned the extra off.
var ErrDisabled = errors.New("extra is disabled")

func textResult(outcome transcript.Outcome, text string) tools.Result {
	return tools.Result{Outcome: outcome, Parts: []transcript.Part{{Text: text}}, Content: []acp.ToolCallContent{tools.TextContent(text)}}
}

func newTool(name, description string, kind acp.ToolKind, schema jsontext.Value) tools.Tool {
	return tools.Tool{
		Spec: provider.Tool{Name: name, Description: description, InputSchema: schema}, Effect: tools.ReadOnly,
		Describe: func(input jsontext.Value) (tools.Description, error) {
			var in struct {
				Intent string `json:"intent"`
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return tools.Description{}, fmt.Errorf("%s: input: %w", name, err)
			}
			title := name
			if in.Intent != "" {
				title = in.Intent
			}
			return tools.Description{Title: title, Kind: kind}, nil
		},
	}
}
