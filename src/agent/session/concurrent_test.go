package session_test

import (
	"context"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/agent/session"
	"github.com/gfx-labs/towa/src/store"
	"github.com/gfx-labs/towa/src/store/dir"
)

func TestDrainingOwnerStaysVisibleAfterCallerCancellation(t *testing.T) {
	for _, operation := range []string{"close", "disconnect", "cancelled load"} {
		t.Run(operation, func(t *testing.T) {
			st, err := dir.New(dir.Options{Root: t.TempDir(), Compression: dir.CompressionZstd})
			require.NoError(t, err)
			started := make(chan struct{})
			cancelledTurn := make(chan struct{})
			release := make(chan struct{})
			options := agentOptions(func(ctx context.Context, _ loop.Turn) (loop.Result, error) {
				close(started)
				<-ctx.Done()
				close(cancelledTurn)
				<-release
				return loop.Result{}, ctx.Err()
			})
			options.Store = st
			host, err := session.NewHost(options)
			require.NoError(t, err)
			first, _ := connect(t, host)
			id := start(t, first)
			first.Send(prompt(`1`, id, "wait"))
			<-started
			second, _ := connect(t, host)
			second.Send(initialize)
			second.Await(`"init"`)
			switch operation {
			case "close":
				first.Send(closeSession(`2`, id))
			case "disconnect":
				first.Close()
			case "cancelled load":
				second.Send(loadRequest(`2`, id, "/work"))
			}
			<-cancelledTurn
			if operation == "cancelled load" {
				second.Close()
				second, _ = connect(t, host)
				second.Send(initialize)
				second.Await(`"init"`)
			}
			// A status request never takes the attach lock. The old owner must
			// remain visible while its turn is still draining, even if the load
			// that cancelled it has itself lost its connection.
			second.Send(request(`3`, "session/status", `{"sessionId":"`+id+`"}`))
			got, _ := second.Await(`3`)
			observed := string(got[`3`].Result)
			close(release)
			require.JSONEq(t, `{"status":"open"}`, observed)
			if operation == "close" {
				first.Await(`1`, `2`)
			}
			if operation == "cancelled load" {
				first.Await(`1`)
			}
			second.Send(loadRequest(`4`, id, "/work"))
			got, _ = second.Await(`4`)
			require.Nil(t, got[`4`].Error)
			require.NoError(t, host.Close(t.Context()))
		})
	}
}

func TestUnarchiveRetainsConcurrentAcknowledgedConfig(t *testing.T) {
	st, err := dir.New(dir.Options{Root: t.TempDir(), Compression: dir.CompressionZstd})
	require.NoError(t, err)
	options := agentOptions(endsTurn)
	options.Store = st
	host, err := session.NewHost(options)
	require.NoError(t, err)
	first, _ := connect(t, host)
	id := start(t, first)
	second, _ := connect(t, host)
	second.Send(initialize)
	second.Await(`"init"`)
	// Repeat both request orderings across an archived, explicitly loaded
	// session. Both operations acknowledge success, so neither may undo the
	// other's durable state, regardless of which handler wins the race.
	for _, mode := range []string{"ask", "auto", "ask", "auto", "ask", "auto"} {
		first.Send(request(`1`, "session/archive", `{"sessionId":"`+id+`"}`))
		first.Await(`1`)
		first.Send(loadRequest(`2`, id, "/work"))
		first.Await(`2`)
		set := request(`3`, "session/set_mode", `{"sessionId":"`+id+`","modeId":"`+mode+`"}`)
		unarchive := request(`4`, "session/unarchive", `{"sessionId":"`+id+`"}`)
		if mode == "ask" {
			second.Send(unarchive)
			first.Send(set)
		} else {
			first.Send(set)
			second.Send(unarchive)
		}
		got, _ := first.Await(`3`)
		require.Nil(t, got[`3`].Error)
		got, _ = second.Await(`4`)
		require.Nil(t, got[`4`].Error)
		metas, err := st.List(t.Context(), store.Filter{Archived: store.ArchivedAny})
		require.NoError(t, err)
		require.Len(t, metas, 1)
		require.Equal(t, mode, metas[0].Mode)
		require.False(t, metas[0].Archived)
	}
	second.Send(loadRequest(`5`, id, "/work"))
	got, _ := second.Await(`5`)
	require.Nil(t, got[`5`].Error)
	var loaded acp.LoadSessionResponse
	require.NoError(t, json.Unmarshal(got[`5`].Result, &loaded))
	require.Equal(t, acp.SessionModeId("auto"), loaded.Modes.CurrentModeId)
	require.NoError(t, host.Close(t.Context()))
}
