package session

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strings"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider/models"
)

// ModelOption is the id of the config option that holds the session's
// model. It is also the override key and the command's name.
const ModelOption acp.SessionConfigId = "model"

// EffortOption is the id of the config option that holds the session's
// reasoning effort. codex-acp names its option so.
const EffortOption acp.SessionConfigId = "reasoning_effort"

// DefaultEffort is the effort choice of an entry that names no effort: no
// effort is sent, which leaves the provider's default.
const DefaultEffort acp.SessionConfigValueId = "default"

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
func AddModels(r *Registry, configured []string, unlisted models.Unlisted) error {
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
		// A client may name a model as config.ResolveModel reads it, such
		// as by the model alone.
		Resolve: func(v Value) (Value, error) {
			name, err := config.ResolveModel(names, string(v.ID))
			return Value{ID: acp.SessionConfigValueId(name)}, err
		},
		Validate: func(v Value) error {
			_, err := models.Select(names, unlisted, string(v.ID))
			return err
		},
		Apply: func(turn *loop.Turn, v Value) { turn.Model = string(v.ID) },
	}); err != nil {
		return err
	}
	if err := r.AddConfigOption(effortOption(names, unlisted)); err != nil {
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

// effortOption is the reasoning effort of the session's model, a select over
// the efforts its entry takes, as models.Select resolves them. It starts at
// the effort the entry names, or at DefaultEffort for one that names none.
// An effort the entry names and the list does not have is still offered:
// the configuration chose it. A model nothing gives efforts for has no
// option, and its entry's effort applies.
func effortOption(configured []string, unlisted models.Unlisted) ConfigOption {
	return ConfigOption{
		ID:          EffortOption,
		Name:        "Reasoning effort",
		Description: "How much reasoning the model does, in its provider's words.",
		Category:    acp.SessionConfigOptionCategoryThoughtLevel,
		Depends: func(values map[acp.SessionConfigId]Value) ([]Choice, Value) {
			model, err := models.Select(configured, unlisted, string(values[ModelOption].ID))
			if err != nil {
				return nil, Value{}
			}
			entry := model.Entries[0]
			if len(entry.Efforts) == 0 {
				return nil, Value{}
			}
			start := DefaultEffort
			if entry.Effort != "" {
				start = acp.SessionConfigValueId(entry.Effort)
			}
			var choices []Choice
			if !slices.Contains(entry.Efforts, string(start)) {
				choices = append(choices, Choice{Value: start, Name: string(start)})
			}
			for _, effort := range entry.Efforts {
				choices = append(choices, Choice{Value: acp.SessionConfigValueId(effort), Name: effort})
			}
			return choices, Value{ID: start}
		},
		Apply: func(turn *loop.Turn, v Value) {
			if v.ID != DefaultEffort {
				turn.Effort = string(v.ID)
			}
		},
	}
}
