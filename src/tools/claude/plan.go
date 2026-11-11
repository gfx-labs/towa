package claude

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/todo"
)

func planTool(spec provider.Tool, deps Deps) tools.Tool {
	return tools.Tool{
		Spec: spec, Effect: tools.ReadOnly,
		Describe: func(jsontext.Value) (tools.Description, error) {
			return tools.Description{Title: "Update plan", Kind: acp.ToolKindThink}, nil
		},
		Run: func(ctx context.Context, call tools.Call) (tools.Result, error) {
			var args struct {
				Todos []struct {
					Content    string      `json:"content"`
					Status     todo.Status `json:"status"`
					ActiveForm string      `json:"activeForm"`
				} `json:"todos"`
			}
			if err := json.Unmarshal(call.Input, &args, json.RejectUnknownMembers(true)); err != nil {
				return textResult("Invalid TodoWrite input: "+err.Error(), true), nil
			}
			list := todo.List{}
			for _, item := range args.Todos {
				list.Items = append(list.Items, todo.Item{Content: item.Content, Status: item.Status, ActiveForm: item.ActiveForm})
			}
			checked, err := todo.Check(list)
			if err != nil {
				return textResult(err.Error(), true), nil
			}
			if err := deps.ShowPlan(ctx, call.Env, checked.Entries); err != nil {
				return tools.Result{}, err
			}
			// 2.1.280 chunk-00pn10ex.js, mapToolResultToToolResultBlockParam.
			return textResult("Todos have been modified successfully. Ensure that you continue to use the todo list to track your progress. Please proceed with the current tasks if applicable", false), nil
		},
	}
}
