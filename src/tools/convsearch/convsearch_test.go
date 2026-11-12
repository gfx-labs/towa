package convsearch_test

import (
	"encoding/json/jsontext"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/store"
	"github.com/gfx-labs/towa/src/store/dir"
	"github.com/gfx-labs/towa/src/tools/convsearch"
	"github.com/gfx-labs/towa/src/transcript"
)

var start = time.Date(2026, time.November, 11, 9, 0, 0, 0, time.UTC)

// session appends entries to a real store, one second apart.
type session struct {
	t    *testing.T
	st   store.Store
	id   uuid.UUID
	now  time.Time
	last uuid.UUID
}

func newStore(t *testing.T) store.Store {
	t.Helper()
	st, err := dir.New(dir.Options{Root: t.TempDir(), Compression: dir.CompressionNone})
	require.NoError(t, err)
	return st
}

func newSession(t *testing.T, st store.Store, parent uuid.UUID, created time.Time) *session {
	t.Helper()
	meta := store.Meta{ID: uuid.NewV7(), ParentID: parent, Cwd: "/work/repo", Created: created, Updated: created}
	require.NoError(t, st.Create(t.Context(), meta))
	return &session{t: t, st: st, id: meta.ID, now: created}
}

func (s *session) add(items ...transcript.Item) {
	s.t.Helper()
	entries := make([]transcript.Entry, 0, len(items))
	for _, item := range items {
		s.now = s.now.Add(time.Second)
		e := transcript.NewEntry(s.last, s.now, item)
		s.last = e.ID
		entries = append(entries, e)
	}
	require.NoError(s.t, s.st.Append(s.t.Context(), s.id, entries))
}

func user(text string) transcript.Item {
	return transcript.Item{User: &transcript.User{Parts: []transcript.Part{{Text: text}}}}
}

func text(text string) transcript.Item { return transcript.Item{Text: &transcript.Text{Text: text}} }

func call(id, name, input string) transcript.Item {
	return transcript.Item{ToolCall: &transcript.ToolCall{ID: id, Name: name, Input: jsontext.Value(input)}}
}

func result(id, name string, parts ...transcript.Part) transcript.Item {
	return transcript.Item{ToolResult: &transcript.ToolResult{
		CallID: id, Name: name, Parts: parts, Outcome: transcript.OutcomeCompleted,
	}}
}

func limits() convsearch.Limits {
	return convsearch.Limits{Hits: 10, SnippetContextBytes: 40, MessageBytes: 80, ToolResultBytes: 80, BlobBytes: 1 << 10}
}

func requireKind(t *testing.T, err error, kind convsearch.Kind) *convsearch.Error {
	t.Helper()
	var cerr *convsearch.Error
	require.ErrorAs(t, err, &cerr)
	require.Equal(t, kind, cerr.Kind)
	return cerr
}

func TestSearchReadsEveryKindOfEntryAndHistoryBeforeACompaction(t *testing.T) {
	st := newStore(t)
	s := newSession(t, st, uuid.UUID{}, start)
	s.add(
		user("Why does the Flock test fail on darwin?"),
		transcript.Item{Thinking: &transcript.Thinking{Text: "the flock call differs there"}},
		call("c1", "shell", `{"command":"go test ./src/auth -run Flock"}`),
		result("c1", "shell", transcript.Part{Text: "--- FAIL: TestFlock"}),
		text("unrelated answer"),
	)
	last := s.last
	s.add(
		transcript.Item{Compaction: &transcript.Compaction{Summary: "flock was discussed", FirstKept: last, Mode: "reactive"}},
		transcript.Item{Usage: &transcript.Usage{Model: "m", InputTokens: 10, OutputTokens: 2}},
		text("Nothing about it after the compaction."),
	)
	access := convsearch.Access{Session: s.id, Widest: convsearch.ScopeDescendants}

	got, err := convsearch.Search(t.Context(), st, access, limits(), convsearch.SearchInput{Query: "FLOCK"})
	require.NoError(t, err)

	fields := make([]convsearch.Field, 0, len(got.Hits))
	for _, h := range got.Hits {
		fields = append(fields, h.Field)
		require.Contains(t, strings.ToLower(h.Snippet), "flock")
	}
	// Every hit is from before the compaction, and the summary is not one.
	require.Equal(t, []convsearch.Field{
		convsearch.FieldUser, convsearch.FieldThinking, convsearch.FieldToolCall, convsearch.FieldToolResult,
	}, fields)
	require.Equal(t, "shell", got.Hits[2].Tool)
	require.Equal(t, 8, got.Entries)
	require.Zero(t, got.Omitted)

	// A hit's position reads the same entry back.
	turns, err := convsearch.Turns(t.Context(), st, access, limits(),
		convsearch.TurnsInput{Start: got.Hits[3].Position, End: got.Hits[3].Position + 1})
	require.NoError(t, err)
	require.Len(t, turns.Turns, 1)
	require.Equal(t, got.Hits[3].ID, turns.Turns[0].ID)
	require.Equal(t, "--- FAIL: TestFlock", turns.Turns[0].Text)
}

func TestSearchMatchesSeveralWordsOnlyAsAPhrase(t *testing.T) {
	st := newStore(t)
	s := newSession(t, st, uuid.UUID{}, start)
	s.add(user("the rate limit resets at noon"), user("limit the rate of retries"))
	access := convsearch.Access{Session: s.id, Widest: convsearch.ScopeSelf}

	got, err := convsearch.Search(t.Context(), st, access, limits(), convsearch.SearchInput{Query: "Rate Limit"})
	require.NoError(t, err)
	require.Len(t, got.Hits, 1)
	require.Equal(t, 0, got.Hits[0].Position)
}

func TestSearchBoundsHitsAndSnippetsAndCountsWhatItLeftOut(t *testing.T) {
	st := newStore(t)
	s := newSession(t, st, uuid.UUID{}, start)
	// Characters of two and three bytes on both sides of the match, and a
	// letter whose lowercase form has another length (İ is two bytes and
	// lowers to the one-byte i), so that an offset into the lowered text is
	// not an offset into the text.
	long := strings.Repeat("İ·a ", 60) + "Needle" + strings.Repeat(" é·İ", 60)
	s.add(user(long), user("needle two"), user("needle three"), user("no match"))
	access := convsearch.Access{Session: s.id, Widest: convsearch.ScopeSelf}

	for _, hitBytes := range []int{3, 6, 7, 40, 41, 42, 43} {
		got, err := convsearch.Search(t.Context(), st, access,
			convsearch.Limits{Hits: 2, SnippetContextBytes: hitBytes, MessageBytes: 80, ToolResultBytes: 80}, convsearch.SearchInput{Query: "needle"})
		require.NoError(t, err)
		require.Len(t, got.Hits, 2)
		require.Equal(t, 1, got.Omitted)
		hit := got.Hits[0]
		require.LessOrEqual(t, len(hit.Snippet), 2*hitBytes+len("Needle"))
		require.True(t, utf8.ValidString(hit.Snippet), "snippet of %d bytes: %q", hitBytes, hit.Snippet)
		require.True(t, hit.CutBefore)
		require.True(t, hit.CutAfter)
		require.Contains(t, hit.Snippet, "Needle")
	}
}

func TestScopeDecidesWhichSessionsAreRead(t *testing.T) {
	st := newStore(t)
	parent := newSession(t, st, uuid.UUID{}, start)
	child := newSession(t, st, parent.id, start.Add(time.Minute))
	grandchild := newSession(t, st, child.id, start.Add(2*time.Minute))
	stranger := newSession(t, st, uuid.UUID{}, start.Add(3*time.Minute))
	for _, s := range []*session{parent, child, grandchild, stranger} {
		s.add(user("the marker word is quokka"))
	}
	query := convsearch.SearchInput{Query: "quokka"}
	found := func(access convsearch.Access, in convsearch.SearchInput) []uuid.UUID {
		t.Helper()
		got, err := convsearch.Search(t.Context(), st, access, limits(), in)
		require.NoError(t, err)
		ids := make([]uuid.UUID, 0, len(got.Hits))
		for _, h := range got.Hits {
			ids = append(ids, h.Session)
		}
		return ids
	}
	descendants := convsearch.Access{Session: parent.id, Widest: convsearch.ScopeDescendants}

	// The default reads the session and everything below it, itself first.
	require.Equal(t, []uuid.UUID{parent.id, child.id, grandchild.id}, found(descendants, query))
	// A child does not read its parent.
	require.Equal(t, []uuid.UUID{child.id, grandchild.id},
		found(convsearch.Access{Session: child.id, Widest: convsearch.ScopeDescendants}, query))
	// With only self allowed, the default narrows to it.
	require.Equal(t, []uuid.UUID{parent.id}, found(convsearch.Access{Session: parent.id, Widest: convsearch.ScopeSelf}, query))

	// One subagent's session, by id.
	query.Session = grandchild.id
	require.Equal(t, []uuid.UUID{grandchild.id}, found(descendants, query))
	// A session outside the scope is refused, not read.
	query.Session = stranger.id
	_, err := convsearch.Search(t.Context(), st, descendants, limits(), query)
	require.Equal(t, stranger.id, requireKind(t, err, convsearch.KindOutOfScope).Session)

	// The whole store is refused until configuration allows it.
	whole := convsearch.SearchInput{Query: "quokka", Target: convsearch.Target{Scope: convsearch.ScopeStore}}
	_, err = convsearch.Search(t.Context(), st, descendants, limits(), whole)
	cerr := requireKind(t, err, convsearch.KindScopeNotAllowed)
	require.Equal(t, convsearch.ScopeStore, cerr.Scope)
	require.Equal(t, convsearch.ScopeDescendants, cerr.Widest)
	_, err = convsearch.Stats(t.Context(), st, descendants, convsearch.StatsInput{Target: whole.Target})
	_ = requireKind(t, err, convsearch.KindScopeNotAllowed)

	all := found(convsearch.Access{Session: parent.id, Widest: convsearch.ScopeStore}, whole)
	require.Equal(t, []uuid.UUID{parent.id, child.id, grandchild.id, stranger.id}, all)
}

func TestSearchReadsAStoredBlobOnlyUnderItsLimit(t *testing.T) {
	st := newStore(t)
	s := newSession(t, st, uuid.UUID{}, start)
	small, err := st.PutBlob(t.Context(), s.id, "text/plain", strings.NewReader("full output with the word capybara in it"))
	require.NoError(t, err)
	large, err := st.PutBlob(t.Context(), s.id, "text/plain", strings.NewReader(strings.Repeat("capybara ", 400)))
	require.NoError(t, err)
	binary, err := st.PutBlob(t.Context(), s.id, "application/octet-stream", strings.NewReader("capybara\xff\xfe"))
	require.NoError(t, err)
	s.add(
		result("c1", "shell", transcript.Part{Text: "output gated"}, transcript.Part{Stored: &small}),
		result("c2", "shell", transcript.Part{Text: "output gated"}, transcript.Part{Stored: &large}),
		result("c3", "shell", transcript.Part{Text: "output gated"}, transcript.Part{Stored: &binary}),
	)
	access := convsearch.Access{Session: s.id, Widest: convsearch.ScopeSelf}
	in := convsearch.SearchInput{Query: "capybara"}

	got, err := convsearch.Search(t.Context(), st, access, limits(), in)
	require.NoError(t, err)
	require.Len(t, got.Hits, 1)
	require.Equal(t, small.Hash, got.Hits[0].Stored)
	require.Equal(t, 1, got.StoredSearched)
	require.Equal(t, 2, got.StoredSkipped, "the blob over the limit and the one that is not text")

	// With no blob limit nothing stored is read, and the result says so.
	none := limits()
	none.BlobBytes = 0
	got, err = convsearch.Search(t.Context(), st, access, none, in)
	require.NoError(t, err)
	require.Empty(t, got.Hits)
	require.Equal(t, 3, got.StoredSkipped)
}

func TestTurnsReturnsARangeWithItsBounds(t *testing.T) {
	st := newStore(t)
	s := newSession(t, st, uuid.UUID{}, start)
	s.add(user("zero"), text("one"), call("c1", "read", `{"path":"a.go"}`), user(strings.Repeat("é", 100)), text("four"))
	access := convsearch.Access{Session: s.id, Widest: convsearch.ScopeSelf}

	got, err := convsearch.Turns(t.Context(), st, access, convsearch.Limits{Hits: 3, SnippetContextBytes: 40, MessageBytes: 21, ToolResultBytes: 10},
		convsearch.TurnsInput{Start: 1, End: 5})
	require.NoError(t, err)
	require.Equal(t, s.id, got.Session)
	require.Len(t, got.Turns, 3)
	require.Equal(t, 1, got.Omitted)
	require.Equal(t, []int{1, 2, 3}, []int{got.Turns[0].Position, got.Turns[1].Position, got.Turns[2].Position})
	require.Equal(t, "one", got.Turns[0].Text)
	require.Equal(t, "read\n{\"path\":\"a.go\"}", got.Turns[1].Text)
	// Twenty bytes, not twenty-one: the cut does not split a character.
	require.Equal(t, strings.Repeat("é", 10), got.Turns[2].Text)
	require.True(t, got.Turns[2].Cut)
	require.Equal(t, 200, got.Turns[2].Bytes)
	s.add(result("c1", "read", transcript.Part{Text: strings.Repeat("é", 20)}))
	preview, err := convsearch.Turns(t.Context(), st, access, convsearch.Limits{Hits: 3, SnippetContextBytes: 40, MessageBytes: 21, ToolResultBytes: 9}, convsearch.TurnsInput{Start: 5, End: 6})
	require.NoError(t, err)
	require.Len(t, preview.Turns, 1)
	require.Equal(t, strings.Repeat("é", 4), preview.Turns[0].Text)
	require.True(t, preview.Turns[0].Cut)

	past, err := convsearch.Turns(t.Context(), st, access, limits(), convsearch.TurnsInput{Start: 50, End: 60})
	require.NoError(t, err)
	require.Empty(t, past.Turns)

	_, err = convsearch.Turns(t.Context(), st, access, limits(), convsearch.TurnsInput{Start: 4, End: 4})
	_ = requireKind(t, err, convsearch.KindInvalidRange)
}

func TestStatsSumsUsageAndSpansTheTimeRange(t *testing.T) {
	st := newStore(t)
	parent := newSession(t, st, uuid.UUID{}, start)
	parent.add(
		user("first"),
		call("c1", "read", `{}`),
		transcript.Item{Usage: &transcript.Usage{Model: "m", InputTokens: 100, OutputTokens: 20, CacheReadTokens: 50, ThoughtTokens: 5}},
	)
	last := parent.last
	parent.add(
		transcript.Item{Compaction: &transcript.Compaction{Summary: "s", FirstKept: last, Mode: "reactive"}},
		user("second"),
		transcript.Item{Usage: &transcript.Usage{Model: "m", InputTokens: 30, OutputTokens: 7, CacheWriteTokens: 9}},
	)
	child := newSession(t, st, parent.id, start.Add(time.Hour))
	child.add(user("subtask"))
	empty := newSession(t, st, parent.id, start.Add(2*time.Hour))

	got, err := convsearch.Stats(t.Context(), st,
		convsearch.Access{Session: parent.id, Widest: convsearch.ScopeDescendants}, convsearch.StatsInput{})
	require.NoError(t, err)
	require.Len(t, got.Sessions, 3)

	p := got.Sessions[0]
	require.Equal(t, parent.id, p.Session)
	require.Equal(t, 6, p.Entries)
	require.Equal(t, 2, p.Prompts)
	require.Equal(t, 1, p.ToolCalls)
	require.Equal(t, 1, p.Compactions)
	require.Equal(t, 3, p.InContext, "the compaction entry and the two after it")
	require.Equal(t, 2, p.Requests)
	require.Equal(t, int64(130), p.InputTokens)
	require.Equal(t, int64(27), p.OutputTokens)
	require.Equal(t, int64(50), p.CacheReadTokens)
	require.Equal(t, int64(9), p.CacheWriteTokens)
	require.Equal(t, int64(5), p.ThoughtTokens)
	require.True(t, p.First.Equal(start.Add(time.Second)))
	require.True(t, p.Last.Equal(start.Add(6*time.Second)))

	require.Equal(t, empty.id, got.Sessions[2].Session)
	require.True(t, got.Sessions[2].First.IsZero(), "a session with no entries has no time range")

	require.Equal(t, 7, got.Total.Entries)
	require.Equal(t, 3, got.Total.Prompts)
	require.Equal(t, int64(130), got.Total.InputTokens)
	// The empty session does not pull the range to the zero time.
	require.True(t, got.Total.First.Equal(start.Add(time.Second)))
	require.True(t, got.Total.Last.Equal(start.Add(time.Hour+time.Second)))
}

func TestACallerTheStoreDoesNotHoldIsNotFound(t *testing.T) {
	st := newStore(t)
	newSession(t, st, uuid.UUID{}, start).add(user("something"))
	missing := uuid.NewV7()

	_, err := convsearch.Search(t.Context(), st,
		convsearch.Access{Session: missing, Widest: convsearch.ScopeStore}, limits(), convsearch.SearchInput{Query: "something"})
	require.Equal(t, missing, requireKind(t, err, convsearch.KindNotFound).Session)
}
