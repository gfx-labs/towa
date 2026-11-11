package session_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/pkg/acp/acptest"
	"github.com/gfx-labs/acpharness/pkg/acp/harness"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/loop"
	"github.com/gfx-labs/acpharness/src/session"
)

const (
	initialize = `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":1}}`
	newSession = `{"jsonrpc":"2.0","id":"new","method":"session/new","params":{"cwd":"/work","mcpServers":[]}}`
	cancelled  = `{"stopReason":"cancelled"}`
	endTurn    = `{"stopReason":"end_turn"}`
)

func prompt(id, session, text string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"session/prompt","params":{"sessionId":"` + session +
		`","prompt":[{"type":"text","text":"` + text + `"}]}}`
}

func cancel(session string) string {
	return `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"` + session + `"}}`
}

func closeSession(id, session string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"session/close","params":{"sessionId":"` + session + `"}}`
}

// serve runs the agent behind a real connection and returns the client's
// raw end of it.
func serve(t *testing.T, turn session.TurnFunc) *acptest.Wire {
	t.Helper()
	return serveWith(t, turn, nil)
}

// serveWith is serve with the options changed by adjust.
func serveWith(t *testing.T, turn session.TurnFunc, adjust func(*session.Options)) *acptest.Wire {
	t.Helper()
	options := session.Options{
		Version: "test",
		Turn:    turn,
		Logger:  slog.New(slog.DiscardHandler),
		Tracer:  tracenoop.NewTracerProvider().Tracer("test"),
		Meter:   metricnoop.NewMeterProvider().Meter("test"),
		Session: config.Session{Mode: config.SessionModeAuto},
	}
	if adjust != nil {
		adjust(&options)
	}
	agent, err := session.NewAgent(options)
	require.NoError(t, err)
	w, in, out := acptest.New(t)
	conn := acp.NewAgentSideConnection(agent, out, in, nil)
	agent.SetClient(conn)
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = conn.Serve(context.Background())
	}()
	t.Cleanup(func() {
		w.Close()
		<-served
	})
	return w
}

// start initializes the connection and creates a session.
func start(t *testing.T, w *acptest.Wire) string {
	t.Helper()
	w.Send(initialize, newSession)
	got, _ := w.Await(`"init"`, `"new"`)
	require.Nil(t, got[`"new"`].Error)
	var resp acp.NewSessionResponse
	require.NoError(t, json.Unmarshal(got[`"new"`].Result, &resp))
	require.NotEmpty(t, resp.SessionId)
	// The first request for a session is followed by its commands. /mode is
	// answered without a turn, so this reads them here and leaves the tests
	// their own turn's messages. The pipe has no buffer: a test that waits
	// for its turn before it reads would otherwise wait forever.
	w.Send(prompt(`"warm"`, string(resp.SessionId), "/mode"))
	w.Await(`"warm"`)
	return string(resp.SessionId)
}

// untilCancelled is a turn that runs until it is cancelled. The channel gets
// each turn's prompt text as the turn starts.
func untilCancelled(started chan<- string) session.TurnFunc {
	return func(ctx context.Context, turn loop.Turn) (loop.Result, error) {
		started <- turn.Prompt[0].Text.Text
		<-ctx.Done()
		return loop.Result{}, ctx.Err()
	}
}

func TestPromptRunsATurnInTheSessionsDirectory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := serve(t, func(ctx context.Context, turn loop.Turn) (loop.Result, error) {
			require.Equal(t, "/work", turn.Cwd)
			return loop.Stub(ctx, turn)
		})
		id := start(t, w)

		w.Send(prompt(`1`, id, "fix the bug"))
		got, others := w.Await(`1`)
		require.JSONEq(t, endTurn, string(got[`1`].Result))

		// The update was written before the response that ends the turn.
		updates := updatesOf(t, others)
		require.Len(t, updates, 1)
		require.Equal(t, id, string(updates[0].SessionId))
		require.Contains(t, updates[0].Update.AgentMessageChunk.Content.Text.Text, "fix the bug")
	})
}

func TestCancelEndsTheTurnAsCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan string, 1)
		w := serve(t, untilCancelled(started))
		id := start(t, w)

		w.Send(prompt(`1`, id, "first"))
		<-started
		w.Send(cancel(id))

		// The turn returned context.Canceled. The client gets the stop
		// reason, not an error.
		got, _ := w.Await(`1`)
		require.Nil(t, got[`1`].Error)
		require.JSONEq(t, cancelled, string(got[`1`].Result))
	})
}

func TestNewPromptCancelsTheRunningTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var running atomic.Int32
		started := make(chan string, 2)
		w := serve(t, func(ctx context.Context, turn loop.Turn) (loop.Result, error) {
			require.Equal(t, int32(1), running.Add(1), "two turns ran in one session at once")
			defer running.Add(-1)
			text := turn.Prompt[0].Text.Text
			started <- text
			if text == "second" {
				return loop.Result{StopReason: acp.StopReasonEndTurn}, nil
			}
			<-ctx.Done()
			return loop.Result{}, ctx.Err()
		})
		id := start(t, w)

		w.Send(prompt(`1`, id, "first"))
		require.Equal(t, "first", <-started)
		w.Send(prompt(`2`, id, "second"))

		got, _ := w.Await(`1`, `2`)
		require.JSONEq(t, cancelled, string(got[`1`].Result))
		require.JSONEq(t, endTurn, string(got[`2`].Result))
		require.Equal(t, "second", <-started)
	})
}

// The client does not wait between messages. The three handlers run
// concurrently, and the order the messages were read in decides the outcome:
// the cancel is for the first prompt only.
func TestCancelAppliesOnlyToPromptsBeforeIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := serve(t, func(ctx context.Context, turn loop.Turn) (loop.Result, error) {
			if turn.Prompt[0].Text.Text == "second" {
				return loop.Result{StopReason: acp.StopReasonEndTurn}, nil
			}
			<-ctx.Done()
			return loop.Result{}, ctx.Err()
		})
		id := start(t, w)

		w.Send(prompt(`1`, id, "first"), cancel(id), prompt(`2`, id, "second"))

		got, _ := w.Await(`1`, `2`)
		require.JSONEq(t, cancelled, string(got[`1`].Result))
		require.JSONEq(t, endTurn, string(got[`2`].Result))
	})
}

func TestCloseStopsTheTurnAndForgetsTheSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan string, 1)
		w := serve(t, untilCancelled(started))
		id := start(t, w)

		w.Send(prompt(`1`, id, "first"))
		<-started
		w.Send(closeSession(`2`, id))
		got, _ := w.Await(`1`, `2`)
		require.JSONEq(t, cancelled, string(got[`1`].Result))
		require.JSONEq(t, `{}`, string(got[`2`].Result))

		// The session is gone, and closing it again still succeeds.
		w.Send(prompt(`3`, id, "again"), closeSession(`4`, id))
		got, _ = w.Await(`3`, `4`)
		require.NotNil(t, got[`3`].Error)
		require.Equal(t, acp.NewResourceNotFound(nil).Code, got[`3`].Error.Code)
		require.Equal(t, map[string]any{"sessionId": id}, got[`3`].Error.Data)
		require.JSONEq(t, `{}`, string(got[`4`].Result))
	})
}

func TestRejectedRequests(t *testing.T) {
	tests := []struct {
		name     string
		init     bool
		line     string
		wantCode int
	}{
		{"session/new before initialize", false, newSession, acp.NewInvalidRequest(nil).Code},
		{"prompt before initialize", false, prompt(`"new"`, "s", "hi"), acp.NewInvalidRequest(nil).Code},
		{"prompt for an unknown session", true, prompt(`"new"`, "nope", "hi"), acp.NewResourceNotFound(nil).Code},
		{
			"relative working directory", true,
			`{"jsonrpc":"2.0","id":"new","method":"session/new","params":{"cwd":"work","mcpServers":[]}}`,
			acp.NewInvalidParams(nil).Code,
		},
		{
			"MCP servers, which are not supported yet", true,
			`{"jsonrpc":"2.0","id":"new","method":"session/new","params":{"cwd":"/work","mcpServers":[{"name":"fs","command":"/bin/mcp","args":[],"env":[]}]}}`,
			acp.NewInvalidParams(nil).Code,
		},
		{
			"a method behind a capability that is not advertised", true,
			`{"jsonrpc":"2.0","id":"new","method":"session/list","params":{}}`,
			acp.NewMethodNotFound("").Code,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := serve(t, loop.Stub)
				if tt.init {
					w.Send(initialize)
					w.Await(`"init"`)
				}
				w.Send(tt.line)
				got, _ := w.Await(`"new"`)
				require.NotNil(t, got[`"new"`].Error)
				require.Equal(t, tt.wantCode, got[`"new"`].Error.Code)
			})
		})
	}
}

// A client decides what to do about a stopped turn from a typed field. It
// must get the same value back that the loop reported, through real JSON.
func TestStoppedTurnCarriesATypedReason(t *testing.T) {
	reset := time.Date(2026, 11, 11, 18, 0, 0, 0, time.UTC)

	t.Run("a failed turn carries it in the error data", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			want := harness.Stop{Kind: harness.StopRateLimit, ResetAt: reset, RetryAfterSeconds: 90, Account: "work", Model: "opus"}
			w := serve(t, func(context.Context, loop.Turn) (loop.Result, error) {
				return loop.Result{}, &loop.StopError{Stop: want, Err: errors.New("HTTP 429")}
			})
			id := start(t, w)

			w.Send(prompt(`1`, id, "hi"))
			got, _ := w.Await(`1`)
			require.NotNil(t, got[`1`].Error)
			addition, ok, err := harness.FromError(got[`1`].Error)
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, &want, addition.Stop)
		})
	})

	t.Run("a turn that hit a limit carries it in the response _meta", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			want := harness.Stop{Kind: harness.StopLimit, Limit: "limits.max_tool_calls"}
			w := serve(t, func(context.Context, loop.Turn) (loop.Result, error) {
				return loop.Result{StopReason: acp.StopReasonMaxTurnRequests, Stop: &want}, nil
			})
			id := start(t, w)

			w.Send(prompt(`1`, id, "hi"))
			got, _ := w.Await(`1`)
			require.Nil(t, got[`1`].Error)
			var resp acp.PromptResponse
			require.NoError(t, json.Unmarshal(got[`1`].Result, &resp))
			require.Equal(t, acp.StopReasonMaxTurnRequests, resp.StopReason)
			addition, ok, err := harness.FromMeta(resp.Meta)
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, &want, addition.Stop)
		})
	})

	t.Run("a rejected credential answers as authentication required", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			w := serve(t, func(context.Context, loop.Turn) (loop.Result, error) {
				return loop.Result{}, &loop.StopError{Stop: harness.Stop{Kind: harness.StopAuth}}
			})
			id := start(t, w)

			w.Send(prompt(`1`, id, "hi"))
			got, _ := w.Await(`1`)
			require.NotNil(t, got[`1`].Error)
			require.Equal(t, acp.NewAuthRequired(nil).Code, got[`1`].Error.Code)
		})
	})
}

// updatesOf decodes the session updates among the agent's messages.
func updatesOf(t *testing.T, messages []acptest.Message) []acp.SessionNotification {
	t.Helper()
	var updates []acp.SessionNotification
	for _, m := range messages {
		if m.Method != acp.ClientMethodSessionUpdate {
			continue
		}
		var update acp.SessionNotification
		require.NoError(t, json.Unmarshal(m.Params, &update))
		updates = append(updates, update)
	}
	return updates
}
