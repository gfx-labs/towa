package session_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/gfx-labs/towa/src/loop"
	"github.com/gfx-labs/towa/src/session"
	"github.com/gfx-labs/towa/src/store/dir"
	"github.com/stretchr/testify/require"
)

func TestSessionOwnsRuntimeUntilItsTurnStops(t *testing.T) {
	for _, end := range []string{"session/close", "session/archive", "session/delete", "session/load", "session/resume", "disconnect", "host close"} {
		t.Run(end, func(t *testing.T) {
			st, err := dir.New(dir.Options{Root: t.TempDir(), Compression: dir.CompressionZstd})
			require.NoError(t, err)
			var mu sync.Mutex
			var events []string
			var lifetimes []context.Context
			record := func(event string) { mu.Lock(); defer mu.Unlock(); events = append(events, event) }
			started := make(chan struct{})
			options := agentOptions(nil)
			options.Store = st
			options.Open = func(ctx context.Context, base loop.Turn) (session.Runtime, error) {
				require.Empty(t, base.Prompt)
				require.NotNil(t, base.Update)
				require.NotNil(t, base.Permit)
				mu.Lock()
				lifetimes = append(lifetimes, ctx)
				events = append(events, "open")
				mu.Unlock()
				return session.Runtime{
					Turn: func(ctx context.Context, _ loop.Turn) (loop.Result, error) {
						record("turn")
						started <- struct{}{}
						<-ctx.Done()
						record("stopped")
						return loop.Result{}, ctx.Err()
					},
					Close: func(context.Context) error { record("close"); return nil },
				}, nil
			}
			host, err := session.NewHost(options)
			require.NoError(t, err)
			first, served := connect(t, host)
			id := start(t, first)
			// Returning from session/new and /mode must not cancel the runtime.
			mu.Lock()
			lifetime := lifetimes[0]
			mu.Unlock()
			require.NoError(t, lifetime.Err())
			first.Send(prompt(`1`, id, "wait"))
			<-started
			switch end {
			case "disconnect":
				first.Close()
				require.NoError(t, <-served)
			case "host close":
				require.NoError(t, host.Close(t.Context()))
				got, _ := first.Await(`1`)
				require.JSONEq(t, cancelled, string(got[`1`].Result))
			default:
				second, _ := connect(t, host)
				second.Send(initialize)
				second.Await(`"init"`)
				line := request(`2`, end, `{"sessionId":"`+id+`"}`)
				if end == "session/close" {
					second = first
				}
				if end == "session/load" || end == "session/resume" {
					line = request(`2`, end, `{"sessionId":"`+id+`","cwd":"/work","mcpServers":[]}`)
				}
				second.Send(line)
				if second == first {
					got, _ := first.Await(`1`, `2`)
					require.Nil(t, got[`2`].Error)
				} else {
					got, _ := first.Await(`1`)
					require.JSONEq(t, cancelled, string(got[`1`].Result))
					got, _ = second.Await(`2`)
					require.Nil(t, got[`2`].Error)
				}
			}
			require.ErrorIs(t, lifetime.Err(), context.Canceled)
			mu.Lock()
			observed := append([]string(nil), events...)
			mu.Unlock()
			expected := []string{"open", "turn", "stopped", "close"}
			if end == "session/load" || end == "session/resume" {
				expected = append(expected, "open")
			}
			require.Equal(t, expected, observed)
			require.NoError(t, host.Close(t.Context()))
			require.NoError(t, host.Close(t.Context()))
		})
	}
}

func TestRuntimeOpenFailureClosesPartialResource(t *testing.T) {
	for _, nilTurn := range []bool{false, true} {
		t.Run(map[bool]string{false: "error", true: "nil turn"}[nilTurn], func(t *testing.T) {
			closed := make(chan struct{})
			options := agentOptions(nil)
			options.Open = func(context.Context, loop.Turn) (session.Runtime, error) {
				runtime := session.Runtime{Close: func(context.Context) error { close(closed); return nil }}
				if nilTurn {
					return runtime, nil
				}
				return runtime, errors.New("resource acquisition failed")
			}
			host, err := session.NewHost(options)
			require.NoError(t, err)
			w, _ := connect(t, host)
			w.Send(initialize, newSession)
			got, _ := w.Await(`"init"`, `"new"`)
			require.NotNil(t, got[`"new"`].Error)
			<-closed
			require.NoError(t, host.Close(t.Context()))
		})
	}
}

func TestHostCloseCancelsRuntimeAcquisition(t *testing.T) {
	started := make(chan struct{})
	closed := make(chan struct{})
	options := agentOptions(nil)
	options.Open = func(ctx context.Context, _ loop.Turn) (session.Runtime, error) {
		close(started)
		<-ctx.Done()
		return session.Runtime{Close: func(context.Context) error { close(closed); return nil }}, ctx.Err()
	}
	host, err := session.NewHost(options)
	require.NoError(t, err)
	w, _ := connect(t, host)
	w.Send(initialize)
	w.Await(`"init"`)
	w.Send(newSession)
	<-started
	require.NoError(t, host.Close(t.Context()))
	<-closed
	got, _ := w.Await(`"new"`)
	require.NotNil(t, got[`"new"`].Error)
	w.Send(request(`1`, "session/new", `{"cwd":"/work","mcpServers":[]}`))
	got, _ = w.Await(`1`)
	require.NotNil(t, got[`1`].Error)
}
