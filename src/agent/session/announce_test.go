package session_test

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/go-acp/acptest"
)

func commandNotification(t *testing.T, w *acptest.Wire, id string) acp.SessionNotification {
	t.Helper()
	message := w.Recv()
	require.Equal(t, "session/update", message.Method)
	var notice acp.SessionNotification
	require.NoError(t, json.Unmarshal(message.Params, &notice))
	require.Equal(t, id, string(notice.SessionId))
	require.NotNil(t, notice.Update.AvailableCommandsUpdate)
	require.NotEmpty(t, notice.Update.AvailableCommandsUpdate.AvailableCommands)
	return notice
}

func TestCommandsFollowSessionResponseWithoutAnotherRequest(t *testing.T) {
	w := pod(t, t.TempDir(), nil)
	w.Send(initialize, newSession)
	got, before := w.Await(`"init"`, `"new"`)
	require.Empty(t, before, "new commands must not precede the creation response")
	var created acp.NewSessionResponse
	require.NoError(t, json.Unmarshal(got[`"new"`].Result, &created))
	id := string(created.SessionId)
	commandNotification(t, w, id)
	for _, method := range []string{"session/load", "session/resume", "session/fork"} {
		w.Send(request(`1`, method, `{"sessionId":"`+id+`","cwd":"/work","mcpServers":[]}`))
		got, before = w.Await(`1`)
		require.Nil(t, got[`1`].Error)
		for _, message := range before {
			var notice acp.SessionNotification
			require.NoError(t, json.Unmarshal(message.Params, &notice))
			require.Nil(t, notice.Update.AvailableCommandsUpdate, "commands must follow this session response")
		}
		if method == "session/fork" {
			var fork acp.UnstableForkSessionResponse
			require.NoError(t, json.Unmarshal(got[`1`].Result, &fork))
			id = string(fork.SessionId)
		}
		commandNotification(t, w, id)
	}
	// A later request must not announce the same immutable list again.
	w.Send(prompt(`2`, id, "/mode"))
	_, others := w.Await(`2`)
	for _, message := range others {
		var notice acp.SessionNotification
		require.NoError(t, json.Unmarshal(message.Params, &notice))
		require.Nil(t, notice.Update.AvailableCommandsUpdate)
	}
}
