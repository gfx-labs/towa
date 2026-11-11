package session_test

import (
	"context"
	"encoding/json/v2"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/loop"
	"github.com/gfx-labs/acpharness/src/session"
)

func TestModelIsChosenPerSessionAndSwitchedBetweenTurns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		configured := map[string]config.Model{
			"opus": {Chain: []string{"claude:work/claude-opus-4-7"}},
			"gpt":  {Chain: []string{"codex:work/gpt-6.1-sol"}},
			// Listed, and refused when chosen: nothing is known about it.
			"glm": {Chain: []string{"zai:main/glm-5"}},
		}
		used := make(chan string, 1)
		w := serveWith(t, func(_ context.Context, turn loop.Turn) (loop.Result, error) {
			used <- turn.Model
			return loop.Result{StopReason: acp.StopReasonEndTurn}, nil
		}, func(o *session.Options) {
			o.Registry = &session.Registry{}
			require.NoError(t, session.AddModels(o.Registry, "opus", configured))
		})
		modelOf := func(options []acp.SessionConfigOption) (acp.SessionConfigValueId, []acp.SessionConfigValueId) {
			for _, o := range options {
				if o.Select != nil && o.Select.Id == session.ModelOption {
					require.Equal(t, acp.SessionConfigOptionCategoryModel, *o.Select.Category)
					var values []acp.SessionConfigValueId
					for _, c := range *o.Select.Options.Ungrouped {
						values = append(values, c.Value)
					}
					return o.Select.CurrentValue, values
				}
			}
			require.Fail(t, "no model option")
			return "", nil
		}

		// A session starts on the default, and one that names a model in
		// _meta on that one.
		first := create(t, w, `{}`, "")
		current, choices := modelOf(first.ConfigOptions)
		require.Equal(t, acp.SessionConfigValueId("opus"), current)
		require.Equal(t, []acp.SessionConfigValueId{"glm", "gpt", "opus"}, choices)

		w.Send(request(`"second"`, "session/new", `{"cwd":"/work","mcpServers":[],"_meta":{"acpharness":{"model":"gpt"}}}`))
		got, _ := w.Await(`"second"`)
		var second acp.NewSessionResponse
		require.NoError(t, json.Unmarshal(got[`"second"`].Result, &second))
		current, _ = modelOf(second.ConfigOptions)
		require.Equal(t, acp.SessionConfigValueId("gpt"), current)

		id, other := string(first.SessionId), string(second.SessionId)
		w.Send(prompt(`1`, id, "go"))
		w.Await(`1`)
		require.Equal(t, "opus", <-used)
		w.Send(prompt(`2`, other, "go"))
		w.Await(`2`)
		require.Equal(t, "gpt", <-used)

		// A switch applies to the session's next turn, and only to it.
		w.Send(request(`3`, "session/set_config_option", `{"sessionId":"`+id+`","configId":"model","value":"gpt"}`))
		got, _ = w.Await(`3`)
		var set acp.SetSessionConfigOptionResponse
		require.NoError(t, json.Unmarshal(got[`3`].Result, &set))
		current, _ = modelOf(set.ConfigOptions)
		require.Equal(t, acp.SessionConfigValueId("gpt"), current)
		w.Send(prompt(`4`, id, "go"))
		w.Await(`4`)
		require.Equal(t, "gpt", <-used)

		// /model switches back and is answered without a turn.
		w.Send(prompt(`5`, id, "/model opus"))
		got, others := w.Await(`5`)
		require.JSONEq(t, endTurn, string(got[`5`].Result))
		updates := updatesOf(t, others)
		require.Len(t, updates, 2)
		current, _ = modelOf(updates[0].Update.ConfigOptionUpdate.ConfigOptions)
		require.Equal(t, acp.SessionConfigValueId("opus"), current)
		require.Contains(t, updates[1].Update.AgentMessageChunk.Content.Text.Text, "opus")

		// A model that cannot be used is refused when it is chosen, by any
		// of the three ways, and the session keeps its model.
		w.Send(
			request(`6`, "session/set_config_option", `{"sessionId":"`+id+`","configId":"model","value":"glm"}`),
			request(`7`, "session/set_config_option", `{"sessionId":"`+id+`","configId":"model","value":"sonnet"}`),
			request(`8`, "session/new", `{"cwd":"/work","mcpServers":[],"_meta":{"acpharness":{"model":"glm"}}}`),
		)
		got, _ = w.Await(`6`, `7`, `8`)
		for _, n := range []string{`6`, `7`, `8`} {
			require.NotNil(t, got[n].Error, n)
			require.Equal(t, acp.NewInvalidParams(nil).Code, got[n].Error.Code, n)
		}
		w.Send(prompt(`9`, id, "/model glm"))
		got, _ = w.Await(`9`)
		require.Equal(t, acp.NewInvalidParams(nil).Code, got[`9`].Error.Code)
		w.Send(prompt(`10`, id, "go"))
		w.Await(`10`)
		require.Equal(t, "opus", <-used)
	})
}

func TestAddModelsWithoutAUsableDefault(t *testing.T) {
	// No model: nothing is registered, and the agent has no model option.
	none := &session.Registry{}
	require.NoError(t, session.AddModels(none, "", nil))
	require.NoError(t, none.AddConfigOption(session.ConfigOption{
		ID: session.ModelOption, Name: "free", Choices: []session.Choice{{Value: "x", Name: "x"}}, Default: session.Value{ID: "x"},
	}))

	// A default nothing is known about fails the registration.
	require.Error(t, session.AddModels(&session.Registry{}, "glm", map[string]config.Model{"glm": {Chain: []string{"zai:main/glm-5"}}}))
}
