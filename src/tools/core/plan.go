package core

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/todo"
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
					// The confidences, the plan and the goals are the model's own
					// record. The tool's schema has checked them and nothing
					// here reads them.
					Confidence           string `json:"confidence"`
					CompletionConfidence string `json:"completion_confidence"`
				} `json:"todos"`
				Plan  jsontext.Value `json:"plan"`
				Goals jsontext.Value `json:"goals"`
			}
			if err := json.Unmarshal(call.Input, &args, json.RejectUnknownMembers(true)); err != nil {
				return textResult("Invalid todo input: "+err.Error(), true), nil
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
			return textResult("The task list was updated.", false), nil
		},
	}
}
