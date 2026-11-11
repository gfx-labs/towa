package session_test

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/pkg/acp/acptest"
	"github.com/gfx-labs/acpharness/src/loop"
	"github.com/gfx-labs/acpharness/src/session"
	"github.com/gfx-labs/acpharness/src/store/dir"
)

type readWriter struct {
	io.Reader
	io.Writer
}

// connect serves one more connection of the host and returns the client's
// end of it and what ServeConn returned, once it has.
func connect(t *testing.T, host *session.Host) (*acptest.Wire, <-chan error) {
	t.Helper()
	w, in, out := acptest.New(t)
	served := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		served <- host.ServeConn(context.Background(), readWriter{in, out})
	}()
	t.Cleanup(func() {
		w.Close()
		<-done
	})
	return w, served
}

func TestSessionMovesBetweenConnectionsOfOneProcess(t *testing.T) {
	st, err := dir.New(dir.Options{Root: t.TempDir(), Compression: dir.CompressionZstd})
	require.NoError(t, err)
	// A prompt of "wait" runs until it is cancelled. Any other is answered
	// and stored.
	started := make(chan string)
	stopped := make(chan string)
	stored := storedTurn(st)
	options := agentOptions(func(ctx context.Context, turn loop.Turn) (loop.Result, error) {
		if turn.Prompt[0].Text.Text != "wait" {
			return stored(ctx, turn)
		}
		started <- string(turn.SessionID)
		<-ctx.Done()
		stopped <- string(turn.SessionID)
		return loop.Result{}, ctx.Err()
	})
	options.Store = st
	host, err := session.NewHost(options)
	require.NoError(t, err)

	// The first connection makes a session, runs a turn and starts one
	// that does not end.
	first, _ := connect(t, host)
	id := start(t, first)
	first.Send(prompt(`1`, id, "first"))
	_, others := first.Await(`1`)
	live, _ := shown(t, others)
	first.Send(prompt(`2`, id, "wait"))
	require.Equal(t, id, <-started)

	// To the second connection the session is in memory but not its own.
	second, secondServed := connect(t, host)
	second.Send(
		initialize,
		request(`1`, "session/status", `{"sessionId":"`+id+`"}`),
		prompt(`2`, id, "not mine"),
		closeSession(`3`, id),
	)
	got, _ := second.Await(`"init"`, `1`, `2`, `3`)
	require.JSONEq(t, `{"status":"open"}`, string(got[`1`].Result))
	require.Equal(t, acp.NewResourceNotFound(nil).Code, got[`2`].Error.Code)
	require.Nil(t, got[`3`].Error)

	// Loading it takes it over: the first connection's turn is cancelled
	// and has stopped before the load answers.
	second.Send(loadRequest(`4`, id, "/work"))
	require.Equal(t, id, <-stopped)
	got, _ = first.Await(`2`)
	require.JSONEq(t, cancelled, string(got[`2`].Result))
	got, others = second.Await(`4`)
	require.Nil(t, got[`4`].Error)
	replayed, _ := shown(t, others)
	require.Equal(t, append([]string{userChunk(t, "first")}, live...), replayed)

	// The first connection no longer has it. The second does.
	first.Send(prompt(`3`, id, "after"))
	got, _ = first.Await(`3`)
	require.Equal(t, acp.NewResourceNotFound(nil).Code, got[`3`].Error.Code)
	second.Send(prompt(`5`, id, "second"))
	got, _ = second.Await(`5`)
	require.JSONEq(t, endTurn, string(got[`5`].Result))

	// The second connection hangs up in the middle of a turn. The turn is
	// cancelled and the session leaves memory.
	second.Send(prompt(`6`, id, "wait"))
	require.Equal(t, id, <-started)
	second.Close()
	require.Equal(t, id, <-stopped)
	require.NoError(t, <-secondServed)

	// It is still in the store, with both answered turns, and the first
	// connection is untouched by the other's end. Requests run concurrently,
	// so the status is read before the resume attaches the session.
	first.Send(request(`4`, "session/status", `{"sessionId":"`+id+`"}`))
	got, _ = first.Await(`4`)
	require.JSONEq(t, `{"status":"saved"}`, string(got[`4`].Result))
	first.Send(request(`5`, "session/resume", `{"sessionId":"`+id+`","cwd":"/work"}`))
	got, _ = first.Await(`5`)
	require.Nil(t, got[`5`].Error)
	first.Send(prompt(`6`, id, "third"))
	got, _ = first.Await(`6`)
	require.JSONEq(t, endTurn, string(got[`6`].Result))

	third, _ := connect(t, host)
	third.Send(initialize, loadRequest(`1`, id, "/work"))
	got, others = third.Await(`"init"`, `1`)
	require.Nil(t, got[`1`].Error)
	replayed, _ = shown(t, others)
	require.Len(t, replayed, 3*(1+len(live)))
}
