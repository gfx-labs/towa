package storebench_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/store"
	"github.com/gfx-labs/acpharness/src/store/dir"
	"github.com/gfx-labs/acpharness/src/store/storebench"
)

// The variables of the run that is made by hand. The numbers in
// docs/notes/2026-11-11-store.md come from it.
const (
	// envCorpus is a directory of text files. Setting it turns the run on.
	envCorpus = "ACPHARNESS_STOREBENCH_CORPUS"
	// envRoot is where the sessions are written, so that the run can be
	// pointed at the file system that is to be measured. It defaults to the
	// test's temporary directory.
	envRoot        = "ACPHARNESS_STOREBENCH_ROOT"
	envTurns       = "ACPHARNESS_STOREBENCH_TURNS"
	envToolCalls   = "ACPHARNESS_STOREBENCH_TOOL_CALLS"
	envResultBytes = "ACPHARNESS_STOREBENCH_RESULT_BYTES"
	envKeyword     = "ACPHARNESS_STOREBENCH_KEYWORD"
)

// subject is the directory backend with one compression.
func subject(t *testing.T, root string, c dir.Compression) storebench.Subject {
	t.Helper()
	opts := dir.Options{Root: filepath.Join(root, string(c)), Compression: c}
	s, err := dir.New(opts)
	require.NoError(t, err)
	return storebench.Subject{
		Name:  "dir " + string(c),
		Store: s,
		Reopen: func() (store.Store, error) {
			return dir.New(opts)
		},
		DiskBytes: func(id uuid.UUID) (int64, error) {
			var total int64
			err := filepath.WalkDir(s.SessionDir(id), func(_ string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				info, err := d.Info()
				total += info.Size()
				return err
			})
			return total, err
		},
		Counters: func() (int64, int64) {
			stats := s.Stats()
			return stats.Syncs, stats.WrittenBytes
		},
		Repeats: 5,
	}
}

func envInt(t *testing.T, name string) int {
	t.Helper()
	n, err := strconv.Atoi(os.Getenv(name))
	require.NoError(t, err, name)
	return n
}

func TestRunByHand(t *testing.T) {
	corpus := os.Getenv(envCorpus)
	if corpus == "" {
		t.Skip(envCorpus + " is not set")
	}
	root := os.Getenv(envRoot)
	if root == "" {
		root = t.TempDir()
	} else {
		var err error
		root, err = os.MkdirTemp(root, "storebench-")
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, os.RemoveAll(root)) })
	}
	base := storebench.Workload{
		Corpus: os.DirFS(corpus), Seed: 1,
		Turns: envInt(t, envTurns), ToolCalls: envInt(t, envToolCalls), ResultBytes: envInt(t, envResultBytes),
		Keyword: os.Getenv(envKeyword),
	}
	perEntry, compacted := base, base
	perEntry.AppendPerEntry = true
	compacted.CompactEvery = max(base.Turns/10, 1)
	for _, w := range []struct {
		name     string
		workload storebench.Workload
	}{
		{"one append per turn", base},
		{"one append per entry", perEntry},
		{"one append per turn, compacted every tenth of the session", compacted},
	} {
		var reports []storebench.Report
		for _, c := range []dir.Compression{dir.CompressionNone, dir.CompressionZstd} {
			report, err := storebench.Run(t.Context(), subject(t, filepath.Join(root, strconv.Itoa(len(w.name))), c), w.workload)
			require.NoError(t, err)
			reports = append(reports, report)
		}
		_, err := os.Stdout.WriteString("\n" + w.name + "\n\n")
		require.NoError(t, err)
		require.NoError(t, storebench.WriteTable(os.Stdout, reports))
	}
}

// The harness is checked on the directory backend with this repository's
// documents as the corpus.
func TestRunMeasuresTheDirectoryBackend(t *testing.T) {
	w := storebench.Workload{
		Corpus: os.DirFS(filepath.Join("..", "..", "..", "docs")), Seed: 7,
		Turns: 30, ToolCalls: 3, ResultBytes: 2000, CompactEvery: 10, Keyword: "session",
	}
	var reports []storebench.Report
	for _, c := range []dir.Compression{dir.CompressionNone, dir.CompressionZstd} {
		report, err := storebench.Run(t.Context(), subject(t, t.TempDir(), c), w)
		require.NoError(t, err)
		reports = append(reports, report)
	}
	plain, compressed := reports[0], reports[1]

	// Prompt, three calls of three entries, answer and usage, and three
	// compactions.
	assert.Equal(t, 30*(1+3*3+2)+3, plain.Entries)
	assert.Equal(t, 30, plain.Appends)
	// The backend's promise that a turn costs one fsync.
	assert.Equal(t, int64(30), plain.Syncs)
	assert.Equal(t, int64(30), compressed.Syncs)
	// The plain log is the payload, a commit line for each append and the
	// session's metadata: nothing is written twice.
	assert.Greater(t, plain.WrittenBytes, plain.PayloadBytes)
	assert.Less(t, plain.WrittenBytes, plain.DiskBytes)
	assert.Less(t, float64(plain.DiskBytes), 1.01*float64(plain.PayloadBytes))
	assert.Less(t, compressed.DiskBytes, plain.DiskBytes)
	// The last compaction starts turn 30, which has 13 entries with it.
	assert.Equal(t, 13, plain.LoadEntries)
	// The same seed writes the same session to both, so they agree.
	assert.Equal(t, plain.PayloadBytes, compressed.PayloadBytes)
	assert.Equal(t, plain.LoadEntries, compressed.LoadEntries)
	assert.Positive(t, plain.ScanHits)
	assert.Equal(t, plain.ScanHits, compressed.ScanHits)

	perEntry := w
	perEntry.AppendPerEntry = true
	report, err := storebench.Run(t.Context(), subject(t, t.TempDir(), dir.CompressionNone), perEntry)
	require.NoError(t, err)
	assert.Equal(t, plain.Entries, report.Appends)
	assert.Equal(t, int64(plain.Entries), report.Syncs)
}
