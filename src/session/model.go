package session

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strings"

	"github.com/gfx-labs/towa/pkg/acp"
	"github.com/gfx-labs/towa/src/loop"
	"github.com/gfx-labs/towa/src/models"
)

// ModelOption is the id of the config option that holds the session's
// model. It is also the override key and the command's name.
const ModelOption acp.SessionConfigId = "model"

// AddModels registers the model config option, the model key of
// _meta.towa on session/new and the /model command. The option is a
// select over the configured models in list order, starting at the first. The chosen
// name is written to loop.Turn.Model before each turn, so a switch applies
// from the next turn.
//
// A model is resolved with models.Select when it is chosen, so one whose
// metadata is not known is refused then, with the config keys that would
// complete it. That includes the default, which fails here.
//
// With no model configured there is nothing to choose, and nothing is
// registered. The wiring calls this only once the turn selects its chain
// from loop.Turn.Model: registering it is what advertises the option.
func AddModels(r *Registry, configured []string, unlisted models.Metadata) error {
	if len(configured) == 0 {
		return nil
	}
	names := slices.Clone(configured)
	choices := make([]Choice, len(names))
	for i, name := range names {
		choices[i] = Choice{Value: acp.SessionConfigValueId(name), Name: name}
	}
	if err := r.AddConfigOption(ConfigOption{
		ID:          ModelOption,
		Name:        "Model",
		Description: "The model the session's turns use.",
		Category:    acp.SessionConfigOptionCategoryModel,
		Choices:     choices,
		Default:     Value{ID: acp.SessionConfigValueId(names[0])},
		Validate: func(v Value) error {
			_, err := models.Select(names, unlisted, string(v.ID))
			return err
		},
		Apply: func(turn *loop.Turn, v Value) { turn.Model = string(v.ID) },
	}); err != nil {
		return err
	}
	if err := r.AddOverride(Override{Key: string(ModelOption), Parse: func(value jsontext.Value) (OverrideEffect, error) {
		var name string
		if err := json.Unmarshal(value, &name); err != nil {
			return OverrideEffect{}, err
		}
		return OverrideEffect{Set: map[acp.SessionConfigId]Value{ModelOption: {ID: acp.SessionConfigValueId(name)}}}, nil
	}}); err != nil {
		return err
	}
	return r.AddCommand(Command{
		Name:        string(ModelOption),
		Description: "Show the session's model and the ones it can switch to, or switch to the named one.",
		InputHint:   "a model name",
		Run: func(ctx context.Context, call CommandCall) (loop.Result, error) {
			if call.Args != "" {
				if err := call.SetOption(ctx, ModelOption, Value{ID: acp.SessionConfigValueId(call.Args)}); err != nil {
					return loop.Result{}, err
				}
			}
			current, _ := call.Option(ModelOption)
			reply := acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				Content: acp.ContentBlock{Text: &acp.ContentBlockText{
					Type: "text",
					Text: "Model: " + string(current.ID) + "\nConfigured: " + strings.Join(names, ", "),
				}},
			}}
			if err := call.Turn.Update(ctx, reply); err != nil {
				return loop.Result{}, err
			}
			return loop.Result{StopReason: acp.StopReasonEndTurn}, nil
		},
	})
}
