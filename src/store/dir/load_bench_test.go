package dir_test

import (
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/store"
	"github.com/gfx-labs/towa/src/store/dir"
	"github.com/gfx-labs/towa/src/transcript"
)

// BenchmarkParallelHistory reads the whole history of compressed sessions
// from many goroutines at once, as a pool of sessions starting turns does.
// Each session has 2000 entries in 400 appends.
func BenchmarkParallelHistory(b *testing.B) {
	for _, concurrency := range []int{1, 0} {
		b.Run("zstd_concurrency="+strconv.Itoa(concurrency), func(b *testing.B) {
			st, err := dir.New(dir.Options{Root: b.TempDir(), Compression: dir.CompressionZstd, NoSync: true, ZstdConcurrency: concurrency})
			require.NoError(b, err)
			now := time.Now()
			output := strings.Repeat("output line\n", 20)
			ids := make([]uuid.UUID, 32)
			for i := range ids {
				ids[i] = uuid.NewV7()
				require.NoError(b, st.Create(b.Context(), store.Meta{ID: ids[i], Cwd: b.TempDir(), Created: now, Updated: now}))
				var parent uuid.UUID
				for n := range 400 {
					call := strconv.Itoa(n)
					var batch []transcript.Entry
					for _, item := range []transcript.Item{
						{User: &transcript.User{Parts: []transcript.Part{{Text: "do the next thing"}}}},
						{ToolCall: &transcript.ToolCall{ID: call, Name: "bash", Input: []byte(`{"command":"ls"}`)}},
						{ToolResult: &transcript.ToolResult{CallID: call, Name: "bash", Outcome: transcript.OutcomeCompleted, Parts: []transcript.Part{{Text: output}}}},
						{Text: &transcript.Text{Text: "done"}},
						{Usage: &transcript.Usage{Model: "m"}},
					} {
						e := transcript.NewEntry(parent, now, item)
						parent = e.ID
						batch = append(batch, e)
					}
					require.NoError(b, st.Append(b.Context(), ids[i], batch))
				}
			}
			var next atomic.Int64
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					entries, err := st.History(b.Context(), ids[next.Add(1)%int64(len(ids))], store.Range{})
					if err != nil {
						b.Error(err)
						return
					}
					for _, err := range entries {
						if err != nil {
							b.Error(err)
							return
						}
					}
				}
			})
		})
	}
}
