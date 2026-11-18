package looptest_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/agent/loop/looptest"
	"github.com/gfx-labs/towa/src/provider/providertest"
	"github.com/gfx-labs/towa/src/store"
	"github.com/gfx-labs/towa/src/transcript"
)

// BenchmarkTurnOnLongSession runs the recorded read turn (two model calls
// and one tool call) on sessions that already hold 2000 entries. The
// recorded responses are served in order without comparing requests, which
// carry the added history.
func BenchmarkTurnOnLongSession(b *testing.B) {
	b.Run("load", func(b *testing.B) { benchmarkTurn(b, false) })
	b.Run("kept", func(b *testing.B) { benchmarkTurn(b, true) })
}

// benchmarkTurn runs turns on sessions of 2000 entries. With keep, every
// turn is on one session whose history the loop kept from the turn before.
func benchmarkTurn(b *testing.B, keep bool) {
	f := looptest.At(b, looptest.ReadTurn, replay(b))
	l := f.Loop
	count := 64
	if keep {
		count = 1
		opts := f.Options
		opts.Kept = loop.NewKept(time.Hour)
		var err error
		l, err = loop.New(opts)
		require.NoError(b, err)
	}
	sessions := make([]acp.SessionId, count)
	for i := range sessions {
		sessions[i] = seedSession(b, f, 400)
	}
	update := func(context.Context, acp.SessionUpdate) error { return nil }
	if keep {
		_, err := l.Run(b.Context(), f.Turn(sessions[0], update))
		require.NoError(b, err)
	}
	b.ReportAllocs()
	n := 0
	for b.Loop() {
		if _, err := l.Run(b.Context(), f.Turn(sessions[n%len(sessions)], update)); err != nil {
			b.Fatal(err)
		}
		n++
	}
}

// replay serves the read turn's recorded responses in order, again and
// again, without comparing requests, which carry the added history.
func replay(t testing.TB) string {
	var bodies [][]providertest.Bytes
	for n := 1; n <= looptest.ReadTurn.Requests; n++ {
		ex, err := providertest.Load(looptest.RecordingPath(looptest.ReadTurn, n))
		require.NoError(t, err)
		bodies = append(bodies, ex.Response.Chunks)
	}
	var served atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range bodies[(served.Add(1)-1)%int64(len(bodies))] {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server.URL + "/v1"
}

// seedSession stores a session of rounds rounds of five entries: a prompt, a
// read and its result, an answer and its usage.
func seedSession(t testing.TB, f *looptest.Fixture, rounds int) acp.SessionId {
	now := time.Now()
	output := strings.Repeat("output line\n", 20)
	id := uuid.NewV7()
	require.NoError(t, f.Store.Create(t.Context(), store.Meta{ID: id, Cwd: f.Cwd, Created: now, Updated: now}))
	var parent uuid.UUID
	var batch []transcript.Entry
	for n := range rounds {
		call := "seed_" + strconv.Itoa(n)
		for _, item := range []transcript.Item{
			{User: &transcript.User{Parts: []transcript.Part{{Text: "do the next thing"}}}},
			{ToolCall: &transcript.ToolCall{ID: call, Name: looptest.ReadToolName, Input: []byte(`{"path":"notes.txt"}`)}},
			{ToolResult: &transcript.ToolResult{CallID: call, Name: looptest.ReadToolName, Outcome: transcript.OutcomeCompleted, Parts: []transcript.Part{{Text: output}}}},
			{Text: &transcript.Text{Text: "done"}},
			{Usage: &transcript.Usage{Model: looptest.Model}},
		} {
			e := transcript.NewEntry(parent, now, item)
			parent = e.ID
			batch = append(batch, e)
		}
	}
	require.NoError(t, f.Store.Append(t.Context(), id, batch))
	return acp.SessionId(id.String())
}
