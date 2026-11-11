package session_test

import (
	"context"
	"encoding/json/v2"
	"testing"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/loop"
	"github.com/gfx-labs/acpharness/src/session"
	"github.com/stretchr/testify/require"
)

func TestStoredSessionRetainsTurnSettings(t *testing.T) {
	root := t.TempDir()
	adjust := func(o *session.Options) {
		o.Supports.ContextOverrides = true
		o.Supports.SystemPrompt = true
		o.Registry = &session.Registry{}
		require.NoError(t, o.Registry.AddConfigOption(session.ConfigOption{ID: "style", Name: "Style", Default: session.Value{ID: "short"}, Choices: []session.Choice{{Value: "short", Name: "Short"}, {Value: "long", Name: "Long"}}}))
	}
	first := pod(t, root, adjust)
	first.Send(initialize, request(`1`, "session/new", `{"cwd":"/work","mcpServers":[],"systemPrompt":{"mode":"append","content":[{"type":"text","text":"be brief"}]},"_meta":{"acpharness":{"context_files":["notes.md"],"skills_dirs":["/skills"],"mode":"ask"}},"configOptions":[{"configId":"style","value":"long"}]}`))
	got, _ := first.Await(`"init"`, `1`)
	require.Nil(t, got[`1`].Error)
	var created acp.NewSessionResponse
	require.NoError(t, json.Unmarshal(got[`1`].Result, &created))
	id := string(created.SessionId)
	first.Send(request(`2`, "session/set_mode", `{"sessionId":"`+id+`","modeId":"auto"}`))
	first.Await(`2`)

	// A separate agent opens the same store. Neither the initial _meta nor
	// the system prompt is repeated in its load request.
	next := pod(t, root, func(o *session.Options) {
		adjust(o)
		o.Turn = func(_ context.Context, turn loop.Turn) (loop.Result, error) {
			require.Equal(t, []string{"notes.md"}, turn.ContextFiles)
			require.Equal(t, []string{"/skills"}, turn.SkillsDirs)
			require.NotNil(t, turn.SystemPrompt)
			require.Equal(t, "be brief", turn.SystemPrompt.Content[0].Text.Text)
			return loop.Result{StopReason: acp.StopReasonEndTurn}, nil
		}
	})
	next.Send(initialize, loadRequest(`1`, id, "/work"))
	got, _ = next.Await(`"init"`, `1`)
	require.Nil(t, got[`1`].Error)
	var loaded acp.LoadSessionResponse
	require.NoError(t, json.Unmarshal(got[`1`].Result, &loaded))
	require.Equal(t, acp.SessionModeId("auto"), loaded.Modes.CurrentModeId)
	require.Len(t, loaded.ConfigOptions, 2)
	require.Equal(t, acp.SessionConfigValueId("long"), loaded.ConfigOptions[0].Select.CurrentValue)
	next.Send(prompt(`2`, id, "check settings"))
	got, _ = next.Await(`2`)
	require.JSONEq(t, endTurn, string(got[`2`].Result))
}
