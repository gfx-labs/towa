package session_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/pkg/acp/acptest"
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
	agent, err := session.NewAgent(session.Options{
		Version: "test",
		Turn:    turn,
		Logger:  slog.New(slog.DiscardHandler),
		Tracer:  tracenoop.NewTracerProvider().Tracer("test"),
		Meter:   metricnoop.NewMeterProvider().Meter("test"),
	})
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
	return string(resp.SessionId)
}

// untilCancelled is a turn that runs until it is cancelled. The channel gets
// each turn's prompt text as the turn starts.
func untilCancelled(started chan<- string) session.TurnFunc {
	return func(ctx context.Context, turn loop.Turn) (acp.StopReason, error) {
		started <- turn.Prompt[0].Text.Text
		<-ctx.Done()
		return "", ctx.Err()
	}
}

func TestPromptRunsATurnInTheSessionsDirectory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := serve(t, func(ctx context.Context, turn loop.Turn) (acp.StopReason, error) {
			require.Equal(t, "/work", turn.Cwd)
			return loop.Stub(ctx, turn)
		})
		id := start(t, w)

		w.Send(prompt(`1`, id, "fix the bug"))
		got, others := w.Await(`1`)
		require.JSONEq(t, endTurn, string(got[`1`].Result))

		// The update was written before the response that ends the turn.
		require.Len(t, others, 1)
		require.Equal(t, acp.ClientMethodSessionUpdate, others[0].Method)
		var update acp.SessionNotification
		require.NoError(t, json.Unmarshal(others[0].Params, &update))
		require.Equal(t, id, string(update.SessionId))
		require.Contains(t, update.Update.AgentMessageChunk.Content.Text.Text, "fix the bug")
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
		w := serve(t, func(ctx context.Context, turn loop.Turn) (acp.StopReason, error) {
			require.Equal(t, int32(1), running.Add(1), "two turns ran in one session at once")
			defer running.Add(-1)
			text := turn.Prompt[0].Text.Text
			started <- text
			if text == "second" {
				return acp.StopReasonEndTurn, nil
			}
			<-ctx.Done()
			return "", ctx.Err()
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
		w := serve(t, func(ctx context.Context, turn loop.Turn) (acp.StopReason, error) {
			if turn.Prompt[0].Text.Text == "second" {
				return acp.StopReasonEndTurn, nil
			}
			<-ctx.Done()
			return "", ctx.Err()
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
