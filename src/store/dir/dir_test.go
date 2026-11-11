package dir_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/store"
	"github.com/gfx-labs/acpharness/src/store/dir"
	"github.com/gfx-labs/acpharness/src/transcript"
)

var compressions = []dir.Compression{dir.CompressionNone, dir.CompressionZstd}

func open(t *testing.T, root string, c dir.Compression) *dir.Store {
	t.Helper()
	s, err := dir.New(dir.Options{Root: root, Compression: c})
	require.NoError(t, err)
	return s
}

// session writes entries one after another, as a turn does.
type session struct {
	meta store.Meta
	now  time.Time
	last uuid.UUID
}

func newSession(t *testing.T, s store.Store) *session {
	t.Helper()
	now := time.Date(2026, time.November, 11, 9, 0, 0, 0, time.UTC)
	meta := store.Meta{ID: uuid.NewV7(), Cwd: "/work/repo", Title: "first", Created: now, Updated: now}
	require.NoError(t, s.Create(t.Context(), meta))
	return &session{meta: meta, now: now}
}

func (s *session) entry(item transcript.Item) transcript.Entry {
	s.now = s.now.Add(time.Second)
	e := transcript.NewEntry(s.last, s.now, item)
	s.last = e.ID
	return e
}

func (s *session) user(text string) transcript.Entry {
	return s.entry(transcript.Item{User: &transcript.User{Parts: []transcript.Part{{Text: text}}}})
}

func (s *session) text(text string) transcript.Entry {
	return s.entry(transcript.Item{Text: &transcript.Text{Text: text}})
}

func (s *session) compaction(summary string) transcript.Entry {
	return s.entry(transcript.Item{Compaction: &transcript.Compaction{Summary: summary, FirstKept: s.last, Mode: "summary"}})
}

// collect reads a sequence to its end or its first error.
func collect(t *testing.T, seq func(func(transcript.Entry, error) bool)) ([]transcript.Entry, error) {
	t.Helper()
	var entries []transcript.Entry
	for e, err := range seq {
		if err != nil {
			return entries, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func load(t *testing.T, s store.Store, id uuid.UUID) (store.Meta, []transcript.Entry) {
	t.Helper()
	meta, seq, err := s.Load(t.Context(), id)
	require.NoError(t, err)
	entries, err := collect(t, seq)
	require.NoError(t, err)
	return meta, entries
}

func history(t *testing.T, s store.Store, id uuid.UUID, r store.Range) []transcript.Entry {
	t.Helper()
	seq, err := s.History(t.Context(), id, r)
	require.NoError(t, err)
	entries, err := collect(t, seq)
	require.NoError(t, err)
	return entries
}

// same compares entries as JSON, which is what the store promises to keep.
func same(t *testing.T, want, got []transcript.Entry) {
	t.Helper()
	require.Len(t, got, len(want))
	for i := range want {
		w, err := json.Marshal(want[i])
		require.NoError(t, err)
		g, err := json.Marshal(got[i])
		require.NoError(t, err)
		require.Equal(t, string(w), string(g), "entry %d", i)
	}
}

func logFile(t *testing.T, s *dir.Store, id uuid.UUID) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(s.SessionDir(id), "log.jsonl*"))
	require.NoError(t, err)
	require.Len(t, matches, 1)
	return matches[0]
}

// The environment variables that turn the test binary into the second
// process of TestMovedSessionContinues.
const (
	envMovedRoot    = "ACPHARNESS_TEST_MOVED_ROOT"
	envMovedSession = "ACPHARNESS_TEST_MOVED_SESSION"
	envMovedBlob    = "ACPHARNESS_TEST_MOVED_BLOB"
)

// movedReport is what the second process says it found.
type movedReport struct {
	Meta     store.Meta         `json:"meta"`
	Context  []transcript.Entry `json:"context"`
	History  []transcript.Entry `json:"history"`
	Blob     []byte             `json:"blob"`
	Appended transcript.Entry   `json:"appended"`
}

// TestMain runs the second process when its variables are set: it loads the
// session from the root it is given, continues it with one more entry and
// reports what it read.
func TestMain(m *testing.M) {
	root := os.Getenv(envMovedRoot)
	if root == "" {
		os.Exit(m.Run())
	}
	if err := continueMoved(root); err != nil {
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
}

func continueMoved(root string) error {
	ctx := context.Background()
	id, err := uuid.Parse(os.Getenv(envMovedSession))
	if err != nil {
		return err
	}
	// The other compression than the session has: a session is read in the
	// format it was written in, whatever this store creates.
	s, err := dir.New(dir.Options{Root: root, Compression: dir.CompressionNone})
	if err != nil {
		return err
	}
	var report movedReport
	meta, seq, err := s.Load(ctx, id)
	if err != nil {
		return err
	}
	report.Meta = meta
	for e, err := range seq {
		if err != nil {
			return err
		}
		report.Context = append(report.Context, e)
	}
	seq, err = s.History(ctx, id, store.Range{})
	if err != nil {
		return err
	}
	for e, err := range seq {
		if err != nil {
			return err
		}
		report.History = append(report.History, e)
	}
	blob, err := s.OpenBlob(ctx, id, os.Getenv(envMovedBlob))
	if err != nil {
		return err
	}
	if report.Blob, err = io.ReadAll(blob); err != nil {
		return err
	}
	if err := blob.Close(); err != nil {
		return err
	}
	last := report.Context[len(report.Context)-1]
	report.Appended = transcript.NewEntry(last.ID, last.Time.Add(time.Minute), transcript.Item{
		User: &transcript.User{Parts: []transcript.Part{{Text: "continued on the other machine"}}},
	})
	if err := s.Append(ctx, id, []transcript.Entry{report.Appended}); err != nil {
		return err
	}
	return json.MarshalWrite(os.Stdout, report)
}

func TestMovedSessionContinues(t *testing.T) {
	for _, c := range compressions {
		t.Run(string(c), func(t *testing.T) {
			first := open(t, t.TempDir(), c)
			sess := newSession(t, first)
			id := sess.meta.ID

			output := bytes.Repeat([]byte("a tool's full output\n"), 5000)
			stored, err := first.PutBlob(t.Context(), id, "text/plain", bytes.NewReader(output))
			require.NoError(t, err)

			before := []transcript.Entry{sess.user("start"), sess.text("an answer that is summarised later")}
			require.NoError(t, first.Append(t.Context(), id, before))
			inContext := []transcript.Entry{
				sess.compaction("the summary"),
				sess.user("read the file"),
				sess.entry(transcript.Item{
					Thinking: &transcript.Thinking{Text: "which file"},
					Provider: "anthropic", Opaque: jsontext.Value(`{"signature":"c2ln"}`),
				}),
				sess.entry(transcript.Item{ToolCall: &transcript.ToolCall{ID: "call_1", Name: "read", Input: jsontext.Value(`{"path":"a.go"}`)}}),
				sess.entry(transcript.Item{ToolResult: &transcript.ToolResult{
					CallID: "call_1", Name: "read", Outcome: transcript.OutcomeCompleted,
					Parts: []transcript.Part{{Text: "the first lines"}, {Stored: &stored}},
				}}),
			}
			require.NoError(t, first.Append(t.Context(), id, inContext))
			sess.meta.Title, sess.meta.Model, sess.meta.Mode = "renamed", "big", "code"
			sess.meta.Dirs = []string{"/work/other"}
			require.NoError(t, first.SetMeta(t.Context(), sess.meta))

			// Another machine: only the session's directory arrives.
			otherRoot := t.TempDir()
			require.NoError(t, os.CopyFS(filepath.Join(otherRoot, id.String()), os.DirFS(first.SessionDir(id))))

			cmd := exec.CommandContext(t.Context(), os.Args[0])
			cmd.Env = append(os.Environ(), envMovedRoot+"="+otherRoot, envMovedSession+"="+id.String(), envMovedBlob+"="+stored.Hash)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			require.NoError(t, err, stderr.String())
			var report movedReport
			require.NoError(t, json.Unmarshal(out, &report))

			want := sess.meta
			want.Updated = inContext[len(inContext)-1].Time
			assert.Equal(t, want, report.Meta)
			same(t, inContext, report.Context)
			same(t, append(before, inContext...), report.History)
			assert.Equal(t, output, report.Blob)

			// What the second process appended is in the moved directory.
			second := open(t, otherRoot, c)
			meta, entries := load(t, second, id)
			same(t, append(inContext, report.Appended), entries)
			assert.Equal(t, report.Appended.Time, meta.Updated)
			listed, err := second.List(t.Context(), store.Filter{})
			require.NoError(t, err)
			assert.Equal(t, []store.Meta{meta}, listed)
		})
	}
}

func TestLoadStartsAtTheLatestCompaction(t *testing.T) {
	// A line longer than the read buffer, and a provider value that has the
	// compaction key in it without being a compaction.
	long := strings.Repeat("0123456789abcdef", 20_000)
	decoy := jsontext.Value(`{"compaction":{"summary":"not one"}}`)
	for _, c := range compressions {
		t.Run(string(c), func(t *testing.T) {
			s := open(t, t.TempDir(), c)
			sess := newSession(t, s)
			id := sess.meta.ID

			_, entries := load(t, s, id)
			assert.Empty(t, entries)

			var all []transcript.Entry
			add := func(entries ...transcript.Entry) {
				require.NoError(t, s.Append(t.Context(), id, entries))
				all = append(all, entries...)
			}
			add(sess.user("one"), sess.text(long))
			_, entries = load(t, s, id)
			same(t, all, entries)

			add(sess.compaction("first summary"), sess.user("two"))
			// Two compactions in one append: the context starts at the second.
			add(sess.text("three"), sess.compaction("second summary"), sess.compaction("third summary"), sess.user("four"))
			add(sess.entry(transcript.Item{Native: &transcript.Native{Kind: "server_tool"}, Provider: "openai", Opaque: decoy}), sess.text(long))
			_, entries = load(t, s, id)
			same(t, all[6:], entries)
			assert.Equal(t, "third summary", entries[0].Item.Compaction.Summary)

			same(t, all, history(t, s, id, store.Range{}))
			same(t, all[1:4], history(t, s, id, store.Range{From: 1, To: 4}))
			same(t, all[7:], history(t, s, id, store.Range{From: 7}))
			same(t, all, history(t, s, id, store.Range{To: 100}))
			assert.Empty(t, history(t, s, id, store.Range{From: 100}))
			_, err := s.History(t.Context(), id, store.Range{From: 3, To: 2})
			assert.Error(t, err)
		})
	}
}

func TestTornTailIsReported(t *testing.T) {
	for _, c := range compressions {
		t.Run(string(c), func(t *testing.T) {
			s := open(t, t.TempDir(), c)
			sess := newSession(t, s)
			id := sess.meta.ID
			whole := []transcript.Entry{sess.user("one"), sess.text("two")}
			require.NoError(t, s.Append(t.Context(), id, whole))
			path := logFile(t, s, id)
			intact, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, s.Append(t.Context(), id, []transcript.Entry{sess.user("three"), sess.text("four")}))
			full, err := os.ReadFile(path)
			require.NoError(t, err)

			// A crash can leave any prefix of the last append.
			for cut := len(intact) + 1; cut < len(full); cut++ {
				require.NoError(t, os.WriteFile(path, full[:cut], 0o600))
				_, seq, err := s.Load(t.Context(), id)
				require.NoError(t, err)
				entries, err := collect(t, seq)
				if c == dir.CompressionNone && full[cut-1] == '\n' {
					// The plain log has no record around an append, so a
					// tear between two lines leaves a log that is whole: it
					// holds the first entries of an append that never
					// returned.
					require.NoError(t, err)
					same(t, append(whole[:2:2], history(t, s, id, store.Range{From: 2})...), entries)
					require.Len(t, entries, 3)
					continue
				}
				var torn *dir.TornError
				require.ErrorAs(t, err, &torn, "cut at %d", cut)
				assert.Equal(t, int64(cut), torn.Size)
				// The plain log keeps a whole line of a torn append.
				require.GreaterOrEqual(t, len(entries), len(whole), "cut at %d", cut)
				same(t, whole, entries[:len(whole)])
				assert.Less(t, len(entries), 4)

				// A range that ends before the tail does not reach it.
				same(t, whole, history(t, s, id, store.Range{To: 2}))
			}

			// Nothing is appended after a torn record until it is set aside.
			next := sess.user("five")
			var torn *dir.TornError
			require.ErrorAs(t, s.Append(t.Context(), id, []transcript.Entry{next}), &torn)
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, full[:len(full)-1], after)

			set, err := s.Repair(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, torn.Size-torn.Offset, set)
			aside, err := filepath.Glob(filepath.Join(s.SessionDir(id), "torn-*"))
			require.NoError(t, err)
			require.Len(t, aside, 1)
			kept, err := os.ReadFile(aside[0])
			require.NoError(t, err)
			assert.Equal(t, full[torn.Offset:len(full)-1], kept)

			require.NoError(t, s.Append(t.Context(), id, []transcript.Entry{next}))
			_, entries := load(t, s, id)
			assert.Equal(t, next.ID, entries[len(entries)-1].ID)
			set, err = s.Repair(t.Context(), id)
			require.NoError(t, err)
			assert.Zero(t, set)
		})
	}
}

func TestCorruptLogIsReported(t *testing.T) {
	for _, c := range compressions {
		t.Run(string(c), func(t *testing.T) {
			s := open(t, t.TempDir(), c)
			sess := newSession(t, s)
			id := sess.meta.ID
			require.NoError(t, s.Append(t.Context(), id, []transcript.Entry{sess.user("one")}))
			require.NoError(t, s.Append(t.Context(), id, []transcript.Entry{sess.text("two")}))
			path := logFile(t, s, id)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			// Damage inside the first record, which no crash explains.
			data[40] ^= 0xff
			require.NoError(t, os.WriteFile(path, data, 0o600))

			seq, err := s.History(t.Context(), id, store.Range{})
			require.NoError(t, err)
			_, err = collect(t, seq)
			var corrupt *dir.CorruptError
			require.ErrorAs(t, err, &corrupt)
			assert.Equal(t, path, corrupt.Path)
		})
	}
}

// Several processes may share a root. Each has its own Store here, and the
// lock on the log is all that orders them.
func TestConcurrentAppendsKeepEveryEntry(t *testing.T) {
	const writers, appends = 8, 25
	for _, c := range compressions {
		t.Run(string(c), func(t *testing.T) {
			root := t.TempDir()
			first := open(t, root, c)
			sess := newSession(t, first)
			id := sess.meta.ID
			want := map[uuid.UUID]bool{}
			batches := make([][][]transcript.Entry, writers)
			for w := range batches {
				for range appends {
					batch := []transcript.Entry{sess.user("a"), sess.text("b")}
					batches[w] = append(batches[w], batch)
					want[batch[0].ID], want[batch[1].ID] = true, true
				}
			}
			var wg sync.WaitGroup
			for w := range batches {
				s := open(t, root, c)
				wg.Go(func() {
					for _, batch := range batches[w] {
						assert.NoError(t, s.Append(t.Context(), id, batch))
					}
				})
			}
			wg.Wait()
			got := history(t, first, id, store.Range{})
			require.Len(t, got, len(want))
			for i := 0; i < len(got); i += 2 {
				assert.True(t, want[got[i].ID])
				// The two entries of an append stay together.
				assert.Equal(t, got[i].ID, got[i+1].ParentID)
			}
		})
	}
}

func TestBlobs(t *testing.T) {
	s := open(t, t.TempDir(), dir.CompressionNone)
	sess := newSession(t, s)
	id := sess.meta.ID
	content := []byte("the full output of a tool")
	sum := sha256.Sum256(content)

	stored, err := s.PutBlob(t.Context(), id, "text/plain", bytes.NewReader(content))
	require.NoError(t, err)
	assert.Equal(t, transcript.Stored{Hash: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(content)), MimeType: "text/plain"}, stored)

	// The same content again keeps one copy and leaves nothing else behind.
	again, err := s.PutBlob(t.Context(), id, "text/plain", bytes.NewReader(content))
	require.NoError(t, err)
	assert.Equal(t, stored, again)
	files, err := os.ReadDir(filepath.Join(s.SessionDir(id), "blobs"))
	require.NoError(t, err)
	require.Len(t, files, 1)

	r, err := s.OpenBlob(t.Context(), id, stored.Hash)
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	assert.Equal(t, content, got)

	other := sha256.Sum256([]byte("never stored"))
	_, err = s.OpenBlob(t.Context(), id, "sha256:"+hex.EncodeToString(other[:]))
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.OpenBlob(t.Context(), uuid.NewV7(), stored.Hash)
	require.ErrorIs(t, err, store.ErrNotFound)
	// A hash is the only caller-supplied text that becomes a path.
	for _, bad := range []string{"", "sha256:../meta.json", "sha256:" + strings.ToUpper(hex.EncodeToString(sum[:])), hex.EncodeToString(sum[:])} {
		_, err = s.OpenBlob(t.Context(), id, bad)
		require.Error(t, err, bad)
		assert.NotErrorIs(t, err, store.ErrNotFound, bad)
	}

	// Content that changed on disk is not returned as if it were intact.
	path := filepath.Join(s.SessionDir(id), "blobs", files[0].Name())
	require.NoError(t, os.WriteFile(path, []byte("the full output of a tooL"), 0o600))
	r, err = s.OpenBlob(t.Context(), id, stored.Hash)
	require.NoError(t, err)
	_, err = io.ReadAll(r)
	var corrupt *dir.CorruptError
	require.ErrorAs(t, err, &corrupt)
	require.NoError(t, r.Close())
}

func TestSessions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not", "made", "yet")
	s := open(t, root, dir.CompressionNone)
	listed, err := s.List(t.Context(), store.Filter{})
	require.NoError(t, err)
	assert.Empty(t, listed)

	parent := newSession(t, s)
	now := parent.now
	child := store.Meta{ID: uuid.NewV7(), ParentID: parent.meta.ID, Cwd: "/work/elsewhere", Created: now, Updated: now}
	require.NoError(t, s.Create(t.Context(), child))
	require.ErrorIs(t, s.Create(t.Context(), child), fs.ErrExist)

	// SetMeta changes only what the contract lets it change.
	change := parent.meta
	change.Title, change.Cwd, change.Created = "second", "/somewhere/else", now.Add(time.Hour)
	require.NoError(t, s.SetMeta(t.Context(), change))
	want := parent.meta
	want.Title = "second"
	got, _ := load(t, s, parent.meta.ID)
	assert.Equal(t, want, got)

	for _, tc := range []struct {
		name   string
		filter store.Filter
		want   []store.Meta
	}{
		{"all", store.Filter{}, []store.Meta{want, child}},
		{"cwd", store.Filter{Cwd: "/work/repo"}, []store.Meta{want}},
		{"parent", store.Filter{ParentID: parent.meta.ID}, []store.Meta{child}},
		{"both", store.Filter{Cwd: "/work/repo", ParentID: parent.meta.ID}, nil},
	} {
		listed, err := s.List(t.Context(), tc.filter)
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.want, listed, tc.name)
	}

	require.NoError(t, s.Delete(t.Context(), parent.meta.ID))
	missing := parent.meta.ID
	require.ErrorIs(t, s.Delete(t.Context(), missing), store.ErrNotFound)
	require.ErrorIs(t, s.Append(t.Context(), missing, []transcript.Entry{parent.user("late")}), store.ErrNotFound)
	require.ErrorIs(t, s.SetMeta(t.Context(), parent.meta), store.ErrNotFound)
	_, _, err = s.Load(t.Context(), missing)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.History(t.Context(), missing, store.Range{})
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.PutBlob(t.Context(), missing, "", strings.NewReader("x"))
	require.ErrorIs(t, err, store.ErrNotFound)
	names, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, names, 1, "a deleted session leaves nothing under the root")

	// An entry the transcript would reject is not stored.
	require.Error(t, s.Append(t.Context(), child.ID, []transcript.Entry{{ID: uuid.NewV7(), Time: now}}))
	_, entries := load(t, s, child.ID)
	assert.Empty(t, entries)
}
