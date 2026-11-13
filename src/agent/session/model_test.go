package session_test

import (
	"context"
	"encoding/json/v2"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/pkg/acp"
	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/agent/session"
	"github.com/gfx-labs/towa/src/provider/models"
)

func TestModelIsChosenPerSessionAndSwitchedBetweenTurns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		configured := []string{"claude:work/claude-opus-4-7", "codex:work/gpt-6.1-sol"}
		used := make(chan string, 1)
		w := serveWith(t, func(_ context.Context, turn loop.Turn) (loop.Result, error) {
			used <- turn.Model
			return loop.Result{StopReason: acp.StopReasonEndTurn}, nil
		}, func(o *session.Options) {
			o.Registry = &session.Registry{}
			require.NoError(t, session.AddModels(o.Registry, configured, models.Unlisted{Configured: models.Metadata{ContextWindow: 1000, MaxOutputTokens: 100}}))
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

		// A session starts on the first model, and the choices keep the
		// list's order. One that names a model in
		// _meta on that one.
		first := create(t, w, `{}`, "")
		current, choices := modelOf(first.ConfigOptions)
		require.Equal(t, acp.SessionConfigValueId("claude:work/claude-opus-4-7"), current)
		require.Equal(t, []acp.SessionConfigValueId{"claude:work/claude-opus-4-7", "codex:work/gpt-6.1-sol"}, choices)

		w.Send(request(`"second"`, "session/new", `{"cwd":"/work","mcpServers":[],"_meta":{"towa":{"model":"codex:work/gpt-6.1-sol"}}}`))
		got, _ := w.Await(`"second"`)
		var second acp.NewSessionResponse
		require.NoError(t, json.Unmarshal(got[`"second"`].Result, &second))
		current, _ = modelOf(second.ConfigOptions)
		require.Equal(t, acp.SessionConfigValueId("codex:work/gpt-6.1-sol"), current)

		id, other := string(first.SessionId), string(second.SessionId)
		w.Send(prompt(`1`, id, "go"))
		w.Await(`1`)
		require.Equal(t, "claude:work/claude-opus-4-7", <-used)
		w.Send(prompt(`2`, other, "go"))
		w.Await(`2`)
		require.Equal(t, "codex:work/gpt-6.1-sol", <-used)

		// A switch applies to the session's next turn, and only to it.
		w.Send(request(`3`, "session/set_config_option", `{"sessionId":"`+id+`","configId":"model","value":"codex:work/gpt-6.1-sol"}`))
		got, _ = w.Await(`3`)
		var set acp.SetSessionConfigOptionResponse
		require.NoError(t, json.Unmarshal(got[`3`].Result, &set))
		current, _ = modelOf(set.ConfigOptions)
		require.Equal(t, acp.SessionConfigValueId("codex:work/gpt-6.1-sol"), current)
		w.Send(prompt(`4`, id, "go"))
		w.Await(`4`)
		require.Equal(t, "codex:work/gpt-6.1-sol", <-used)

		// /model switches back and is answered without a turn.
		w.Send(prompt(`5`, id, "/model claude:work/claude-opus-4-7"))
		got, others := w.Await(`5`)
		require.JSONEq(t, endTurn, string(got[`5`].Result))
		updates := updatesOf(t, others)
		require.Len(t, updates, 2)
		current, _ = modelOf(updates[0].Update.ConfigOptionUpdate.ConfigOptions)
		require.Equal(t, acp.SessionConfigValueId("claude:work/claude-opus-4-7"), current)
		require.Contains(t, updates[1].Update.AgentMessageChunk.Content.Text.Text, "claude:work/claude-opus-4-7")

		// A model that is not configured is refused when it is chosen, by any
		// of the three ways, and the session keeps its model.
		w.Send(
			request(`6`, "session/set_config_option", `{"sessionId":"`+id+`","configId":"model","value":"glm"}`),
			request(`7`, "session/set_config_option", `{"sessionId":"`+id+`","configId":"model","value":"sonnet"}`),
			request(`8`, "session/new", `{"cwd":"/work","mcpServers":[],"_meta":{"towa":{"model":"glm"}}}`),
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
		require.Equal(t, "claude:work/claude-opus-4-7", <-used)
	})
}

func TestAddModelsWithoutAUsableDefault(t *testing.T) {
	// No model: nothing is registered, and the agent has no model option.
	none := &session.Registry{}
	require.NoError(t, session.AddModels(none, nil, models.Unlisted{}))
	require.NoError(t, none.AddConfigOption(session.ConfigOption{
		ID: session.ModelOption, Name: "free", Choices: []session.Choice{{Value: "x", Name: "x"}}, Default: session.Value{ID: "x"},
	}))

	// A model nothing is known about fails the registration, wherever it
	// is listed: the first model can fall back to it.
	unknown := []string{"claude:work/claude-opus-4-7", "zai:main/glm-5"}
	var missing *models.MetadataError
	require.ErrorAs(t, session.AddModels(&session.Registry{}, unknown, models.Unlisted{}), &missing)
	require.NoError(t, session.AddModels(&session.Registry{}, unknown, models.Unlisted{Configured: models.Metadata{ContextWindow: 1000, MaxOutputTokens: 100}}))
}
