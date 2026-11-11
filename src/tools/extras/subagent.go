package extras

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/pkg/acp/harness"
	"github.com/gfx-labs/acpharness/src/loop"
	"github.com/gfx-labs/acpharness/src/subagent"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// NameSubagent is the child-turn tool's name in every dialect.
const NameSubagent = "subagent"

// Subagent adapts process-wide child admission to one tool. Each invocation
// reads the actual current turn. The runner must open a fresh child runtime.
func Subagent(spawner *subagent.Spawner, runner subagent.Runner) (tools.Tool, error) {
	if spawner == nil {
		return tools.Tool{}, errors.New("subagent: no spawner")
	}
	if runner == nil {
		return tools.Tool{}, errors.New("subagent: no child runtime runner")
	}
	t := newTool(NameSubagent, "Run a child agent with its own conversation and return its final message.", acp.ToolKindOther, []byte(`{"type":"object","required":["prompt"],"properties":{"intent":{"type":"string"},"prompt":{"type":"string","minLength":1},"model":{"type":"string","description":"A configured name allowed by subagents.models. Omit to inherit the current model."},"description":{"type":"string","description":"What the child is for."}}}`))
	t.Effect = tools.Executes
	t.Describe = func(raw jsontext.Value) (tools.Description, error) {
		var in struct {
			Description string `json:"description"`
			Intent      string `json:"intent"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return tools.Description{}, err
		}
		title := in.Description
		if title == "" {
			title = in.Intent
		}
		if title == "" {
			title = NameSubagent
		}
		return tools.Description{Title: title, Kind: acp.ToolKindOther}, nil
	}
	t.Run = func(ctx context.Context, call tools.Call) (tools.Result, error) {
		var in struct {
			Prompt      string `json:"prompt"`
			Model       string `json:"model"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(call.Input, &in); err != nil {
			return tools.Result{}, err
		}
		current, ok := loop.Current(ctx)
		if !ok {
			return tools.Result{}, errors.New("subagent: call has no current turn")
		}
		if call.Env.SessionID != current.Turn.SessionID {
			return tools.Result{}, fmt.Errorf("subagent: calling session %q differs from current turn %q", call.Env.SessionID, current.Turn.SessionID)
		}
		res, err := spawner.Run(ctx, subagent.Parent{Loop: runner, Turn: current.Turn, Progress: call.Env.Progress}, subagent.Input{Prompt: in.Prompt, Model: in.Model, Description: in.Description})
		out := subagentOutput{Final: res.Final, SessionID: res.SessionID, StopReason: res.StopReason, Usage: res.Usage, Charged: res.Charged}
		outcome := transcript.OutcomeCompleted
		if err != nil {
			var failed *subagent.Error
			if !errors.As(err, &failed) {
				return tools.Result{}, err
			}
			outcome = transcript.OutcomeFailed
			out = subagentOutput{
				SessionID: failed.SessionID, StopReason: failed.StopReason, Usage: failed.Usage, Charged: failed.Charged,
				Error: &subagentFailure{Kind: failed.Kind, Message: failed.Error(), Limit: failed.Limit, Stop: failed.Stop},
			}
		}
		body, err := json.Marshal(out)
		if err != nil {
			return tools.Result{}, err
		}
		return textResult(outcome, string(body)), nil
	}
	return t, nil
}

type subagentOutput struct {
	Final      string           `json:"final,omitzero"`
	SessionID  acp.SessionId    `json:"session_id,omitzero"`
	StopReason acp.StopReason   `json:"stop_reason,omitzero"`
	Usage      *acp.Usage       `json:"usage,omitzero"`
	Charged    bool             `json:"charged"`
	Error      *subagentFailure `json:"error,omitzero"`
}

type subagentFailure struct {
	Kind    subagent.Kind `json:"kind"`
	Message string        `json:"message"`
	Limit   string        `json:"limit,omitzero"`
	Stop    *harness.Stop `json:"stop,omitzero"`
}
