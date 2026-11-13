package extras_test

import (
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/gfx-labs/towa/pkg/acp"
	"github.com/gfx-labs/towa/src/agent/extras"
	"github.com/gfx-labs/towa/src/store"
	"github.com/gfx-labs/towa/src/store/dir"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/transcript"
	"github.com/stretchr/testify/require"
)

func BenchmarkConversationSearch(b *testing.B) {
	st, err := dir.New(dir.Options{Root: b.TempDir(), Compression: dir.CompressionNone})
	require.NoError(b, err)
	id := uuid.NewV7()
	now := time.Now()
	require.NoError(b, st.Create(b.Context(), store.Meta{ID: id, Cwd: b.TempDir(), Created: now, Updated: now}))
	var entries []transcript.Entry
	var prev uuid.UUID
	for range 10 {
		e := transcript.NewEntry(prev, now, transcript.Item{Body: transcript.User{Parts: []transcript.Part{{Text: strings.Repeat("before ", 20) + "needle" + strings.Repeat(" after", 20)}}}})
		prev = e.ID
		entries = append(entries, e)
	}
	require.NoError(b, st.Append(b.Context(), id, entries))
	tool, err := extras.ConversationSearch(searchConfig(), st)
	require.NoError(b, err)
	call := tools.Call{Input: []byte(`{"query":"needle"}`), Env: tools.Env{SessionID: acp.SessionId(id.String())}}
	res, err := tool.Run(b.Context(), call)
	require.NoError(b, err)
	require.Equal(b, transcript.OutcomeCompleted, res.Outcome)
	require.Contains(b, text(res), "Found 10 matches")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		res, err = tool.Run(b.Context(), call)
		if err != nil || res.Outcome != transcript.OutcomeCompleted {
			b.Fatalf("search failed: %v (%s)", err, res.Outcome)
		}
	}
}
