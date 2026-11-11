package extras

import (
	"context"
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

// Subagent adapts process-wide child admission to one tool. runner supplies
// the calling session's loop from the current turn, not a turn cached when
// the session was opened. The resolver is called for each invocation.
func Subagent(spawner *subagent.Spawner, current func(context.Context) (loop.Turn, bool), runner func(context.Context, loop.Turn) (subagent.Runner, error)) (tools.Tool, error) {
	if spawner == nil {
		return tools.Tool{}, errors.New("subagent: no spawner")
	}
	if runner == nil {
		return tools.Tool{}, errors.New("subagent: no runner resolver")
	}
	if current == nil {
		return tools.Tool{}, errors.New("subagent: no current-turn accessor")
	}
	t := newTool(NameSubagent, "Run a child agent with its own conversation and return its final message.", acp.ToolKindOther, []byte(`{"type":"object","required":["prompt"],"properties":{"intent":{"type":"string"},"prompt":{"type":"string","minLength":1},"model":{"type":"string","description":"A configured name allowed by subagents.models. Omit to inherit the current model."},"description":{"type":"string","description":"What the child is for."}}}`))
	t.Effect = tools.Executes
	t.Run = func(ctx context.Context, call tools.Call) (tools.Result, error) {
		var in struct {
			Prompt      string `json:"prompt"`
			Model       string `json:"model"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(call.Input, &in); err != nil {
			return tools.Result{}, err
		}
		turn, ok := current(ctx)
		if !ok {
			return tools.Result{}, errors.New("subagent: call has no current turn")
		}
		if call.Env.SessionID != turn.SessionID {
			return tools.Result{}, fmt.Errorf("subagent: calling session %q differs from current turn %q", call.Env.SessionID, turn.SessionID)
		}
		childLoop, err := runner(ctx, turn)
		if err != nil {
			return tools.Result{}, fmt.Errorf("subagent: resolving child runner: %w", err)
		}
		if childLoop == nil {
			return tools.Result{}, errors.New("subagent: resolver returned no child runner")
		}
		res, err := spawner.Run(ctx, subagent.Parent{Loop: childLoop, Turn: turn, Progress: call.Env.Progress}, subagent.Input{Prompt: in.Prompt, Model: in.Model, Description: in.Description})
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
