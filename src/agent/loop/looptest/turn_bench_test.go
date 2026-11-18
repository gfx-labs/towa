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
	var bodies [][]providertest.Bytes
	for n := 1; n <= looptest.ReadTurn.Requests; n++ {
		ex, err := providertest.Load(looptest.RecordingPath(looptest.ReadTurn, n))
		require.NoError(b, err)
		bodies = append(bodies, ex.Response.Chunks)
	}
	var served atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			b.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range bodies[(served.Add(1)-1)%int64(len(bodies))] {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	b.Cleanup(server.Close)

	f := looptest.At(b, looptest.ReadTurn, server.URL+"/v1")
	now := time.Now()
	output := strings.Repeat("output line\n", 20)
	sessions := make([]acp.SessionId, 64)
	for i := range sessions {
		id := uuid.NewV7()
		require.NoError(b, f.Store.Create(b.Context(), store.Meta{ID: id, Cwd: f.Cwd, Created: now, Updated: now}))
		var parent uuid.UUID
		var batch []transcript.Entry
		for n := range 400 {
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
		require.NoError(b, f.Store.Append(b.Context(), id, batch))
		sessions[i] = acp.SessionId(id.String())
	}
	update := func(context.Context, acp.SessionUpdate) error { return nil }
	b.ReportAllocs()
	n := 0
	for b.Loop() {
		served.Store(0)
		if _, err := f.Loop.Run(b.Context(), f.Turn(sessions[n%len(sessions)], update)); err != nil {
			b.Fatal(err)
		}
		n++
	}
}
