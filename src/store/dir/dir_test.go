package dir_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/store"
	"github.com/gfx-labs/towa/src/store/dir"
	"github.com/gfx-labs/towa/src/transcript"
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
	envMovedRoot    = "TOWA_TEST_MOVED_ROOT"
	envMovedSession = "TOWA_TEST_MOVED_SESSION"
	envMovedBlob    = "TOWA_TEST_MOVED_BLOB"
)

// The environment variables that turn the test binary into the process of
// TestFailedAppendLeavesNoTornRecord, whose file size limit is lowered.
const (
	envLimitRoot    = "TOWA_TEST_LIMIT_ROOT"
	envLimitSession = "TOWA_TEST_LIMIT_SESSION"
	envLimitBytes   = "TOWA_TEST_LIMIT_BYTES"
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
// reports what it read. With the other variables it is the process that
// appends under a file size limit.
func TestMain(m *testing.M) {
	var err error
	switch {
	case os.Getenv(envMovedRoot) != "":
		err = continueMoved(os.Getenv(envMovedRoot))
	case os.Getenv(envLimitRoot) != "":
		err = appendOverLimit(os.Getenv(envLimitRoot))
	default:
		os.Exit(m.Run())
	}
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
}

// appendOverLimit appends the entries on standard input with the process's
// file size limit lowered. It succeeds when the limit is what failed the
// append.
func appendOverLimit(root string) error {
	id, err := uuid.Parse(os.Getenv(envLimitSession))
	if err != nil {
		return err
	}
	size, err := strconv.ParseUint(os.Getenv(envLimitBytes), 10, 64)
	if err != nil {
		return err
	}
	var entries []transcript.Entry
	if err := json.UnmarshalRead(os.Stdin, &entries); err != nil {
		return err
	}
	s, err := dir.New(dir.Options{Root: root, Compression: dir.CompressionNone})
	if err != nil {
		return err
	}
	// Without this the kernel's signal for the limit ends the process.
	signal.Ignore(syscall.SIGXFSZ)
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		return err
	}
	full := limit
	limit.Cur = size
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		return err
	}
	failed := s.Append(context.Background(), id, entries)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &full); err != nil {
		return err
	}
	switch {
	case failed == nil:
		return fmt.Errorf("append under a file size limit of %d bytes was not refused", size)
	case !errors.Is(failed, syscall.EFBIG):
		return fmt.Errorf("append under a file size limit of %d bytes: %w", size, failed)
	}
	return nil
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
			before := s.Stats().Syncs
			add(sess.user("one"), sess.text(long))
			// An append costs one fsync, however many entries it holds.
			assert.Equal(t, int64(1), s.Stats().Syncs-before)
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

// A crash can leave any prefix of the bytes of an append. Whatever the
// prefix, the session holds the entries of the appends that returned and
// none of the one that did not, and says that it is torn.
func TestTornAppendIsAbsentAndReported(t *testing.T) {
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
			require.NoError(t, s.Append(t.Context(), id, []transcript.Entry{
				sess.user("three"), sess.text("four"), sess.compaction("five"), sess.user("six"),
			}))
			full, err := os.ReadFile(path)
			require.NoError(t, err)

			for cut := len(intact) + 1; cut < len(full); cut++ {
				require.NoError(t, os.WriteFile(path, full[:cut], 0o600))
				meta, seq, err := s.Load(t.Context(), id)
				require.NoError(t, err)
				assert.Equal(t, whole[1].Time, meta.Updated, "cut at %d", cut)
				entries, err := collect(t, seq)
				require.ErrorIs(t, err, store.ErrTorn, "cut at %d", cut)
				var torn *dir.TornError
				require.ErrorAs(t, err, &torn)
				assert.Equal(t, int64(len(intact)), torn.Offset, "cut at %d", cut)
				assert.Equal(t, int64(cut), torn.Size)
				same(t, whole, entries)

				seq, err = s.History(t.Context(), id, store.Range{})
				require.NoError(t, err)
				entries, err = collect(t, seq)
				require.ErrorIs(t, err, store.ErrTorn, "cut at %d", cut)
				same(t, whole, entries)
				// A range that ends before the tail does not reach it.
				same(t, whole, history(t, s, id, store.Range{To: 2}))
				for _, r := range []store.Range{{To: 3}, {From: 1, To: 3}, {From: 4, To: 5}, {From: 4, To: 4}} {
					seq, err = s.History(t.Context(), id, r)
					require.NoError(t, err)
					entries, err = collect(t, seq)
					require.ErrorIs(t, err, store.ErrTorn, "cut at %d, range %+v", cut, r)
					if r.From < len(whole) {
						same(t, whole[r.From:], entries)
					} else {
						require.Empty(t, entries)
					}
				}
			}

			// Nothing is appended after a torn record until it is set aside.
			next := sess.user("seven")
			require.ErrorIs(t, s.Append(t.Context(), id, []transcript.Entry{next}), store.ErrTorn)
			require.ErrorIs(t, s.Fork(t.Context(), id, store.Meta{ID: uuid.NewV7(), Cwd: "/work/repo"}), store.ErrTorn)
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, full[:len(full)-1], after)

			// Through the interface, as the session layer calls it.
			var st store.Store = s
			set, err := st.Repair(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, int64(len(full)-1-len(intact)), set)
			aside, err := filepath.Glob(filepath.Join(s.SessionDir(id), "torn-*"))
			require.NoError(t, err)
			require.Len(t, aside, 1)
			kept, err := os.ReadFile(aside[0])
			require.NoError(t, err)
			assert.Equal(t, full[len(intact):len(full)-1], kept)
			secondTear := []byte("another append cut short")
			require.NoError(t, os.WriteFile(path, append(bytes.Clone(intact), secondTear...), 0o600))
			set, err = st.Repair(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, int64(len(secondTear)), set)
			allAside, err := filepath.Glob(filepath.Join(s.SessionDir(id), "torn-*"))
			require.NoError(t, err)
			require.Len(t, allAside, 2)
			keptAgain, err := os.ReadFile(aside[0])
			require.NoError(t, err)
			assert.Equal(t, kept, keptAgain)

			require.NoError(t, s.Append(t.Context(), id, []transcript.Entry{next}))
			_, entries := load(t, s, id)
			same(t, append(whole, next), entries)
			set, err = st.Repair(t.Context(), id)
			require.NoError(t, err)
			assert.Zero(t, set)
			_, err = st.Repair(t.Context(), uuid.NewV7())
			require.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}

// The write of an append is made to fail part way by the process's file
// size limit, which is a real short write followed by a real error. The
// limit is the whole process's, and the test binary writes files of its
// own, so another process makes that append.
func TestFailedAppendLeavesNoTornRecord(t *testing.T) {
	for _, c := range compressions {
		t.Run(string(c), func(t *testing.T) {
			root := t.TempDir()
			s := open(t, root, c)
			sess := newSession(t, s)
			id := sess.meta.ID
			whole := []transcript.Entry{sess.user("one"), sess.text("two")}
			require.NoError(t, s.Append(t.Context(), id, whole))
			path := logFile(t, s, id)
			before, err := os.ReadFile(path)
			require.NoError(t, err)

			more, err := json.Marshal([]transcript.Entry{sess.user("three"), sess.text("four")})
			require.NoError(t, err)
			cmd := exec.CommandContext(t.Context(), os.Args[0])
			// Room for the start of the next record and no more.
			cmd.Env = append(os.Environ(), envLimitRoot+"="+root, envLimitSession+"="+id.String(),
				envLimitBytes+"="+strconv.Itoa(len(before)+10))
			cmd.Stdin = bytes.NewReader(more)
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, string(out))

			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, before, after)
			next := sess.user("five")
			require.NoError(t, s.Append(t.Context(), id, []transcript.Entry{next}))
			_, entries := load(t, s, id)
			same(t, append(whole, next), entries)
		})
	}
}

func TestForkCopiesHistoryAndContent(t *testing.T) {
	for _, c := range compressions {
		t.Run(string(c), func(t *testing.T) {
			s := open(t, t.TempDir(), c)
			sess := newSession(t, s)
			source := sess.meta.ID
			content := []byte("output the fork must still open")
			stored, err := s.PutBlob(t.Context(), source, "text/plain", bytes.NewReader(content))
			require.NoError(t, err)
			shared := []transcript.Entry{sess.user("one"), sess.compaction("summary"), sess.text("two")}
			require.NoError(t, s.Append(t.Context(), source, shared[:1]))
			require.NoError(t, s.Append(t.Context(), source, shared[1:]))

			// A store set to the other compression forks in the source's
			// format, and a session with no content forks too.
			other := compressions[0]
			if c == other {
				other = compressions[1]
			}
			forker := open(t, filepath.Dir(s.SessionDir(source)), other)
			meta := store.Meta{
				ID: uuid.NewV7(), Cwd: "/work/fork", Title: "forked", Created: sess.now, Updated: sess.now,
				State: []byte("the fork's own state"),
			}
			require.NoError(t, forker.Fork(t.Context(), source, meta))
			require.ErrorIs(t, forker.Fork(t.Context(), source, meta), fs.ErrExist)
			require.ErrorIs(t, forker.Fork(t.Context(), uuid.NewV7(), store.Meta{ID: uuid.NewV7(), Cwd: "/w"}), store.ErrNotFound)
			assert.Equal(t, filepath.Base(logFile(t, s, source)), filepath.Base(logFile(t, s, meta.ID)))

			got, entries := load(t, s, meta.ID)
			want := meta
			want.Updated = shared[2].Time
			assert.Equal(t, want, got)
			same(t, shared[1:], entries)
			same(t, shared, history(t, s, meta.ID, store.Range{}))

			// The two diverge, and the source's end does not reach the fork.
			mine, theirs := sess.user("in the source"), sess.user("in the fork")
			require.NoError(t, s.Append(t.Context(), source, []transcript.Entry{mine}))
			require.NoError(t, s.Append(t.Context(), meta.ID, []transcript.Entry{theirs}))
			same(t, append(shared[:3:3], mine), history(t, s, source, store.Range{}))
			same(t, append(shared[:3:3], theirs), history(t, s, meta.ID, store.Range{}))
			require.NoError(t, s.Delete(t.Context(), source))
			r, err := s.OpenBlob(t.Context(), meta.ID, stored.Hash)
			require.NoError(t, err)
			read, err := io.ReadAll(r)
			require.NoError(t, err)
			require.NoError(t, r.Close())
			assert.Equal(t, content, read)

			empty := store.Meta{ID: uuid.NewV7(), Cwd: "/work/fork", Created: sess.now, Updated: sess.now}
			bare := newSession(t, s)
			require.NoError(t, s.Fork(t.Context(), bare.meta.ID, empty))
			_, entries = load(t, s, empty.ID)
			assert.Empty(t, entries)
		})
	}
}

func TestSweepRemovesWhatACrashLeft(t *testing.T) {
	root := t.TempDir()
	s := open(t, root, dir.CompressionNone)
	swept, err := open(t, filepath.Join(root, "missing"), dir.CompressionNone).Sweep(t.Context())
	require.NoError(t, err)
	assert.Zero(t, swept)

	sess := newSession(t, s)
	require.NoError(t, s.Append(t.Context(), sess.meta.ID, []transcript.Entry{sess.user("kept")}))
	// What a crash leaves: Create before its rename, Delete after its own.
	half := filepath.Join(root, "."+uuid.NewV7().String()+".creating")
	require.NoError(t, os.Mkdir(half, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(half, "meta.json"), []byte("{"), 0o600))
	gone := filepath.Join(root, "."+uuid.NewV7().String()+".deleting")
	require.NoError(t, os.CopyFS(gone, os.DirFS(s.SessionDir(sess.meta.ID))))
	// One that a running call holds, as build and Delete hold theirs.
	busy := filepath.Join(root, "."+uuid.NewV7().String()+".creating")
	require.NoError(t, os.Mkdir(busy, 0o700))
	held, err := os.Open(busy)
	require.NoError(t, err)
	require.NoError(t, syscall.Flock(int(held.Fd()), syscall.LOCK_EX))

	listed, err := s.List(t.Context(), store.Filter{})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	swept, err = s.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, swept)
	assert.NoDirExists(t, half)
	assert.NoDirExists(t, gone)
	assert.DirExists(t, busy)
	_, entries := load(t, s, sess.meta.ID)
	assert.Len(t, entries, 1)

	require.NoError(t, held.Close())
	swept, err = s.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, swept)
	assert.NoDirExists(t, busy)
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
	before := s.Stats()
	again, err := s.PutBlob(t.Context(), id, "text/plain", bytes.NewReader(content))
	require.NoError(t, err)
	// A visible hash might belong to a publisher that has not synced yet.
	// This call must flush its own file and both namespace boundaries.
	assert.GreaterOrEqual(t, s.Stats().Syncs-before.Syncs, int64(3))
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

	// SetMeta changes only what the contract lets it change. The state is
	// not text and not JSON: the store returns the bytes it was given.
	state := []byte{0x00, 0xff, '{', '"', '\n', 0x80}
	change := parent.meta
	change.Title, change.Cwd, change.Created = "second", "/somewhere/else", now.Add(time.Hour)
	change.Archived, change.State = true, state
	require.NoError(t, s.SetMeta(t.Context(), change))
	want := parent.meta
	want.Title, want.Archived, want.State = "second", true, state
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
		{"archived", store.Filter{Archived: store.ArchivedOnly}, []store.Meta{want}},
		{"not archived", store.Filter{Archived: store.ArchivedExclude}, []store.Meta{child}},
		{"archived in another cwd", store.Filter{Archived: store.ArchivedOnly, Cwd: "/work/elsewhere"}, nil},
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
	require.Len(t, names, 2, "only the child and the derived index remain")
	assert.Equal(t, ".index", names[0].Name())
	assert.Equal(t, child.ID.String(), names[1].Name())

	// An entry the transcript would reject is not stored.
	require.Error(t, s.Append(t.Context(), child.ID, []transcript.Entry{{ID: uuid.NewV7(), Time: now}}))
	_, entries := load(t, s, child.ID)
	assert.Empty(t, entries)
}
