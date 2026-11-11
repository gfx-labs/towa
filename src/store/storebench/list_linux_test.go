//go:build linux

package storebench_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/gfx-labs/acpharness/src/store"
	"github.com/gfx-labs/acpharness/src/store/dir"
	"github.com/gfx-labs/acpharness/src/transcript"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestListByHand measures metadata listing on persistent roots, so the
// same sessions can be measured before and after a change. The caller
// removes the roots afterwards. Cold means file pages evicted with
// POSIX_FADV_DONTNEED, not the directory and inode caches (those require
// privilege to drop).
func TestListByHand(t *testing.T) {
	root := os.Getenv("ACPHARNESS_STOREBENCH_LIST_ROOT")
	if root == "" {
		t.Skip("ACPHARNESS_STOREBENCH_LIST_ROOT is not set")
	}
	for _, count := range []int{1000, 10000} {
		s, err := dir.New(dir.Options{Root: filepath.Join(root, fmt.Sprint(count)), Compression: dir.CompressionZstd})
		require.NoError(t, err)
		parent := uuid.MustParse("00000000-0000-7000-8000-000000000001")
		now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
		if _, err := os.Stat(filepath.Join(root, fmt.Sprint(count))); os.IsNotExist(err) {
			for i := 0; i < count; i++ {
				meta := store.Meta{ID: uuid.NewV7(), Cwd: fmt.Sprintf("/work/project-%d", i%100), Created: now, Updated: now}
				if i%100 == 0 {
					meta.ParentID = parent
				}
				require.NoError(t, s.Create(t.Context(), meta))
				require.NoError(t, s.Append(t.Context(), meta.ID, []transcript.Entry{transcript.NewEntry(uuid.Nil(), now, transcript.Item{Text: &transcript.Text{Text: "a short completed turn"}})}))
			}
		}
		// Warm every file before measuring construction, separately from
		// the steady-state children lookup. This is derived cache only.
		warm, err := s.List(t.Context(), store.Filter{})
		require.NoError(t, err)
		require.Len(t, warm, count)
		err = os.Remove(filepath.Join(root, fmt.Sprint(count), ".index", "children.json"))
		if err != nil && !os.IsNotExist(err) {
			require.NoError(t, err)
		}
		start := time.Now()
		built, err := s.List(t.Context(), store.Filter{ParentID: parent})
		buildTime := time.Since(start)
		require.NoError(t, err)
		require.Len(t, built, count/100)
		fmt.Printf("sessions=%d children-index-construction-warm=%s\n", count, buildTime)
		for _, f := range []struct {
			name   string
			filter store.Filter
		}{
			{"all", store.Filter{}},
			{"cwd", store.Filter{Cwd: "/work/project-0"}},
			{"children", store.Filter{ParentID: parent}},
			{"all-page-50", store.Filter{Limit: 50}},
			{"cwd-page-50", store.Filter{Cwd: "/work/project-0", Limit: 50}},
			{"children-page-50", store.Filter{ParentID: parent, Limit: 50}},
		} {
			for _, cold := range []bool{false, true} {
				var times []time.Duration
				var matched int
				for range 3 {
					if cold {
						err := filepath.WalkDir(filepath.Join(root, fmt.Sprint(count)), func(path string, d os.DirEntry, err error) error {
							if err != nil || !d.Type().IsRegular() {
								return err
							}
							file, err := os.Open(path)
							if err != nil {
								return err
							}
							advice := unix.Fadvise(int(file.Fd()), 0, 0, unix.FADV_DONTNEED)
							closeErr := file.Close()
							if advice != nil {
								return advice
							}
							return closeErr
						})
						require.NoError(t, err)
					}
					start := time.Now()
					metas, err := s.List(t.Context(), f.filter)
					elapsed := time.Since(start)
					require.NoError(t, err)
					matched = len(metas)
					want := count
					if f.filter.Cwd != "" || f.filter.ParentID != uuid.Nil() {
						want = count / 100
					}
					if strings.HasSuffix(f.name, "page-50") {
						want = min(want, 50)
					}
					require.Len(t, metas, want)
					times = append(times, elapsed)
				}
				slices.Sort(times)
				fmt.Printf("sessions=%d filter=%s data-cold=%v matched=%d median=%s\n", count, f.name, cold, matched, times[1])
			}
		}
	}
}
