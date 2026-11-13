package session_test

import (
	"context"
	"encoding/json/v2"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/agent/session"
	"github.com/gfx-labs/towa/src/store/dir"
	"github.com/gfx-labs/towa/src/transcript"
)

func TestRuntimeMapperReplaysForkedSourceHistory(t *testing.T) {
	for _, kind := range []string{"runtime", "nil runtime mapper"} {
		t.Run(kind, func(t *testing.T) {
			st, err := dir.New(dir.Options{Root: t.TempDir(), Compression: dir.CompressionZstd})
			require.NoError(t, err)
			options := agentOptions(storedTurn(st))
			options.Store = st
			var opened atomic.Int32
			options.Open = func(context.Context, loop.Turn) (session.Runtime, error) {
				opened.Add(1)
				runtime := session.Runtime{Turn: storedTurn(st)}
				if kind == "runtime" {
					runtime.Mapper = &loop.Mapper{ContextSize: func(model string) (int, bool) { return 200, model == "session-model" }}
				}
				return runtime, nil
			}
			host, err := session.NewHost(options)
			require.NoError(t, err)
			defer func() { require.NoError(t, host.Close(t.Context())) }()
			w, _ := connect(t, host)
			source := start(t, w)
			w.Send(prompt(`1`, source, "source history"))
			got, _ := w.Await(`1`)
			require.Nil(t, got[`1`].Error)
			sourceID, err := uuid.Parse(source)
			require.NoError(t, err)
			require.NoError(t, st.Append(t.Context(), sourceID, []transcript.Entry{
				transcript.NewEntry(uuid.Nil(), time.Now(), transcript.Item{Usage: &transcript.Usage{Model: "session-model", InputTokens: 7, OutputTokens: 3}}),
			}))
			w.Send(request(`2`, "session/fork", `{"sessionId":"`+source+`","cwd":"/work","mcpServers":[]}`))
			got, _ = w.Await(`2`)
			require.Nil(t, got[`2`].Error)
			var fork acp.UnstableForkSessionResponse
			require.NoError(t, json.Unmarshal(got[`2`].Result, &fork))
			id := string(fork.SessionId)
			commandNotification(t, w, id)
			w.Send(loadRequest(`3`, id, "/work"))
			got, history := w.Await(`3`)
			require.Nil(t, got[`3`].Error)
			var text string
			var usages []acp.SessionUsageUpdate
			for _, message := range history {
				var notice acp.SessionNotification
				require.NoError(t, json.Unmarshal(message.Params, &notice))
				if chunk := notice.Update.AgentMessageChunk; chunk != nil {
					text += chunk.Content.Text.Text
				}
				if usage := notice.Update.UsageUpdate; usage != nil {
					usages = append(usages, *usage)
				}
			}
			require.Equal(t, "reply to source history", text)
			if kind == "nil runtime mapper" {
				require.Empty(t, usages)
			} else {
				require.Len(t, usages, 1)
				require.Equal(t, 10, usages[0].Used)
				require.Equal(t, 200, usages[0].Size)
			}
			commandNotification(t, w, id)
			require.Equal(t, int32(3), opened.Load(), "new, fork and load each prepare once, replay does not reopen")
		})
	}
}
