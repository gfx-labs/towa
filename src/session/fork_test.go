package session_test

import (
	"encoding/json/v2"
	"testing"
	"uuid"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/session"
	"github.com/gfx-labs/acpharness/src/store/dir"
	"github.com/stretchr/testify/require"
)

func TestForkAndArchiveStoredSession(t *testing.T) {
	root := t.TempDir()
	st, err := dir.New(dir.Options{Root: root, Compression: dir.CompressionZstd})
	require.NoError(t, err)
	options := agentOptions(storedTurn(st))
	options.Store = st
	host, err := session.NewHost(options)
	require.NoError(t, err)
	w, _ := connect(t, host)
	source := start(t, w)
	w.Send(prompt(`1`, source, "source"))
	_, sent := w.Await(`1`)
	live, _ := shown(t, sent)
	w.Send(request(`2`, "session/fork", `{"sessionId":"`+source+`","cwd":"/fork","mcpServers":[]}`))
	got, _ := w.Await(`2`)
	require.Nil(t, got[`2`].Error)
	var fork acp.UnstableForkSessionResponse
	require.NoError(t, json.Unmarshal(got[`2`].Result, &fork))
	id := string(fork.SessionId)
	require.NotEqual(t, source, id)
	w.Send(prompt(`3`, id, "fork"), prompt(`4`, source, "original"))
	got, _ = w.Await(`3`, `4`)
	require.JSONEq(t, endTurn, string(got[`3`].Result))
	require.JSONEq(t, endTurn, string(got[`4`].Result))

	// Another connection archives the live fork. The old connection loses
	// ownership, the fork is hidden by default, and history is retained.
	other, _ := connect(t, host)
	other.Send(initialize, request(`1`, "session/archive", `{"sessionId":"`+id+`"}`))
	got, _ = other.Await(`"init"`, `1`)
	require.Nil(t, got[`1`].Error)
	other.Send(request(`2`, "session/list", `{}`), request(`3`, "session/list", `{"archived":"only"}`), request(`4`, "session/list", `{"archived":"include"}`), request(`5`, "session/list", `{"archived":"unknown"}`))
	got, _ = other.Await(`2`, `3`, `4`, `5`)
	var listing acp.ListSessionsResponse
	require.NoError(t, json.Unmarshal(got[`2`].Result, &listing))
	require.Len(t, listing.Sessions, 1)
	require.Equal(t, source, string(listing.Sessions[0].SessionId))
	require.NoError(t, json.Unmarshal(got[`3`].Result, &listing))
	require.Len(t, listing.Sessions, 1)
	require.Equal(t, id, string(listing.Sessions[0].SessionId))
	require.True(t, *listing.Sessions[0].Archived)
	require.NoError(t, json.Unmarshal(got[`4`].Result, &listing))
	require.Len(t, listing.Sessions, 2)
	require.Equal(t, acp.NewInvalidParams(nil).Code, got[`5`].Error.Code)
	w.Send(prompt(`5`, id, "archived"))
	denied, _ := w.Await(`5`)
	require.Equal(t, acp.NewResourceNotFound(nil).Code, denied[`5`].Error.Code)
	other.Send(request(`6`, "session/unarchive", `{"sessionId":"`+id+`"}`), request(`7`, "session/status", `{"sessionId":"`+id+`"}`))
	got, _ = other.Await(`6`, `7`)
	require.Nil(t, got[`6`].Error)
	require.JSONEq(t, `{"status":"saved"}`, string(got[`7`].Result))
	other.Send(loadRequest(`8`, id, "/fork"))
	got, sent = other.Await(`8`)
	require.Nil(t, got[`8`].Error)
	replayed, _ := shown(t, sent)
	require.Len(t, replayed, 2*(1+len(live)))
	require.Equal(t, append([]string{userChunk(t, "source")}, live...), replayed[:1+len(live)])

	// Idempotence and a missing session's error are protocol requirements.
	other.Send(request(`9`, "session/unarchive", `{"sessionId":"`+id+`"}`), request(`10`, "session/archive", `{"sessionId":"`+uuid.NewV7().String()+`"}`))
	got, _ = other.Await(`9`, `10`)
	require.Nil(t, got[`9`].Error)
	require.Equal(t, acp.NewResourceNotFound(nil).Code, got[`10`].Error.Code)
}
