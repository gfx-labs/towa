package extras

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	acp "github.com/gfx-labs/go-acp"

	"github.com/gfx-labs/towa/src/agent/harness"
	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/agent/subagent"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/transcript"
)

// NameSubagent is the child-turn tool's name.
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
	t := newTool(NameSubagent, "Run a child agent and wait for it to finish. The child starts with none of this conversation, so the prompt must hold everything it needs: the task, the paths and what to report back. It has this session's tools and working directory. Returns JSON with the child's last message in final, or an error.", acp.ToolKindOther, []byte(`{"type":"object","required":["prompt"],"properties":{"intent":{"type":"string"},"prompt":{"type":"string","minLength":1,"description":"The whole task for the child, written for a reader who has seen nothing else."},"model":{"type":"string","description":"A configured name allowed by subagents.models. Omit to inherit the current model."},"description":{"type":"string","description":"A few words saying what the child is for. The user sees this."}}}`))
	t.Summary = "Run a child agent on a task and wait for its result"
	t.Guidance = []string{"Give " + NameSubagent + " a task that stands on its own and whose working would fill your context, such as a wide search or an independent piece of work. Do small things yourself."}
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
	t.Run = subagentTool{spawner: spawner, runner: runner}.run
	return t, nil
}

// subagentTool holds what one call needs besides its input.
type subagentTool struct {
	spawner *subagent.Spawner
	runner  subagent.Runner
}

func (s subagentTool) run(ctx context.Context, call tools.Call) (tools.Result, error) {
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
	res, err := s.spawner.Run(ctx, subagent.Parent{Loop: s.runner, Turn: current.Turn, Progress: call.Env.Progress}, subagent.Input{Prompt: in.Prompt, Model: in.Model, Description: in.Description})
	out, outcome, err := childOutput(res, err)
	if err != nil {
		return tools.Result{}, err
	}
	body, err := json.Marshal(out)
	if err != nil {
		return tools.Result{}, err
	}
	return tools.TextResult(outcome, string(body)), nil
}

// childOutput turns the child's result, or its failure, into the tool's JSON shape.
func childOutput(res subagent.Result, err error) (subagentOutput, transcript.Outcome, error) {
	if err == nil {
		return subagentOutput{Final: res.Final, SessionID: res.SessionID, StopReason: res.StopReason, Usage: res.Usage, Charged: res.Charged}, transcript.OutcomeCompleted, nil
	}
	failed, ok := errors.AsType[*subagent.Error](err)
	if !ok {
		return subagentOutput{}, "", err
	}
	return subagentOutput{
		SessionID: failed.SessionID, StopReason: failed.StopReason, Usage: failed.Usage, Charged: failed.Charged,
		Error: &subagentFailure{Kind: failed.Kind, Message: failed.Error(), Limit: failed.Limit, Stop: failed.Stop},
	}, transcript.OutcomeFailed, nil
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
