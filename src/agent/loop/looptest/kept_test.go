package looptest_test

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/agent/loop/looptest"
	"github.com/gfx-labs/towa/src/transcript"
)

// A turn on kept history still sees what another process appended to the
// log after the last turn, and follows it.
func TestAKeptHistoryTakesInWhatWasAppendedSince(t *testing.T) {
	f := looptest.At(t, looptest.ReadTurn, replay(t))
	opts := f.Options
	opts.Kept = loop.NewKept(time.Hour)
	l, err := loop.New(opts)
	require.NoError(t, err)
	session := seedSession(t, f, 2)
	id := uuid.MustParse(string(session))
	update := func(context.Context, acp.SessionUpdate) error { return nil }

	_, err = l.Run(t.Context(), f.Turn(session, update))
	require.NoError(t, err)
	before := history(t, f.Store, id)
	added := transcript.NewEntry(before[len(before)-1].ID, time.Now(), transcript.Item{
		User: &transcript.User{Parts: []transcript.Part{{Text: "from another process"}}},
	})
	require.NoError(t, f.Store.Append(t.Context(), id, []transcript.Entry{added}))

	_, err = l.Run(t.Context(), f.Turn(session, update))
	require.NoError(t, err)
	after := history(t, f.Store, id)
	require.Equal(t, added.ID, after[len(before)].ID)
	require.Equal(t, added.ID, after[len(before)+1].ParentID, "the second turn's prompt follows the appended entry")
}
