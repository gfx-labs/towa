package codex

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/todo"
)

type planArgs struct {
	Explanation string `json:"explanation"`
	Plan        []struct {
		Step   string      `json:"step"`
		Status todo.Status `json:"status"`
	} `json:"plan"`
}

func planTool(spec provider.Tool, deps Deps) tools.Tool {
	return tools.Tool{
		Spec: spec, Effect: tools.ReadOnly,
		Describe: func(jsontext.Value) (tools.Description, error) {
			return tools.Description{Title: "Update plan", Kind: acp.ToolKindThink}, nil
		},
		Run: func(ctx context.Context, call tools.Call) (tools.Result, error) {
			var args planArgs
			if err := json.Unmarshal(call.Input, &args, json.RejectUnknownMembers(true)); err != nil {
				return textResult("failed to parse function arguments: "+err.Error(), true), nil
			}
			list := todo.List{Explanation: args.Explanation}
			for _, item := range args.Plan {
				list.Items = append(list.Items, todo.Item{Content: item.Step, Status: item.Status})
			}
			checked, err := todo.Check(list)
			if err != nil {
				return textResult("update_plan: "+err.Error(), true), nil
			}
			if err := deps.ShowPlan(ctx, call.Env, checked.Entries); err != nil {
				return tools.Result{}, err
			}
			// core/src/tools/handlers/plan.rs:22. The one-in-progress rule is prose only.
			return textResult("Plan updated", false), nil
		},
	}
}
