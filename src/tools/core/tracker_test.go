package core_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/config/configtest"
	"github.com/gfx-labs/towa/src/tools/core"
	"github.com/gfx-labs/towa/src/tools/fsops"
)

func TestReadTrackerGuardsTheEditCapability(t *testing.T) {
	for _, kind := range []string{"external", "same-size-restored-mtime", "replacement", "recreated", "deleted"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			dir := t.TempDir()
			path := filepath.Join(dir, "file")
			require.NoError(t, os.WriteFile(path, []byte("first\nsecond\n"), 0o600))
			loaded, err := configtest.Load(config.Sources{})
			require.NoError(t, err)
			cfg := loaded.Config.Tools.Files
			roots := fsops.Roots{Cwd: dir}
			var tracker core.ReadTracker
			read := func() error {
				return tracker.Read(ctx, path, func() error {
					_, err := fsops.Read(ctx, roots, cfg, fsops.ReadInput{Path: path, Limit: 1}, nil)
					return err
				})
			}
			edits := 0
			edit := func() error {
				edits++
				_, err := fsops.ApplyEdits(ctx, roots, cfg, fsops.EditInput{Path: path, Edits: []fsops.Edit{{Old: "second", New: "SECOND"}}})
				return err
			}
			var unread *core.UnreadError
			require.ErrorAs(t, tracker.Edit(ctx, path, edit), &unread)
			require.Zero(t, edits)
			require.NoError(t, read())
			info, err := os.Stat(path)
			require.NoError(t, err)
			switch kind {
			case "external":
				require.NoError(t, os.WriteFile(path, []byte("first\nother\n"), 0o600))
			case "same-size-restored-mtime":
				require.NoError(t, os.WriteFile(path, []byte("first\nSECOND\n"), 0o600))
				require.NoError(t, os.Chtimes(path, info.ModTime(), info.ModTime()))
			case "replacement":
				next := filepath.Join(dir, "next")
				require.NoError(t, os.WriteFile(next, []byte("first\nsecond\n"), 0o600))
				require.NoError(t, os.Chtimes(next, info.ModTime(), info.ModTime()))
				require.NoError(t, os.Rename(next, path))
			case "recreated":
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.WriteFile(path, []byte("first\nsecond\n"), 0o600))
				require.NoError(t, os.Chtimes(path, info.ModTime(), info.ModTime()))
			case "deleted":
				require.NoError(t, os.Remove(path))
			}
			var changed *core.ChangedError
			require.ErrorAs(t, tracker.Edit(ctx, path, edit), &changed)
			require.Zero(t, edits)
			// Reading again restores authority only to this session.
			require.NoError(t, os.WriteFile(path, []byte("first\nsecond\n"), 0o600))
			require.NoError(t, read())
			require.NoError(t, tracker.Edit(ctx, path, edit))
			require.Equal(t, 1, edits)
			// A successful edit remembers its own replacement inode and new content.
			require.NoError(t, tracker.Edit(ctx, path, func() error {
				_, err := fsops.ApplyEdits(ctx, roots, cfg, fsops.EditInput{Path: path, Edits: []fsops.Edit{{Old: "SECOND", New: "last"}}})
				return err
			}))
		})
	}
}

func TestTrackersAreSessionLocalAndWritesRefreshOnlyTheirOwner(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "file")
	var first, second core.ReadTracker
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
	read := func() error { _, err := os.ReadFile(path); return err }
	require.NoError(t, first.Read(ctx, path, read))
	require.NoError(t, second.Read(ctx, path, read))
	require.NoError(t, first.Write(ctx, path, func() error { return os.WriteFile(path, []byte("new"), 0o600) }))
	var changed *core.ChangedError
	require.ErrorAs(t, second.Edit(ctx, path, func() error { t.Fatal("stale session edited"); return nil }), &changed)
	require.NoError(t, first.Edit(ctx, path, func() error { return os.WriteFile(path, []byte("next"), 0o600) }))
	require.NoError(t, second.Read(ctx, path, read))
	require.NoError(t, second.Edit(ctx, path, func() error { return os.WriteFile(path, []byte("last"), 0o600) }))
	require.ErrorAs(t, first.Edit(ctx, path, func() error { t.Fatal("stale owner edited"); return nil }), &changed)
}

func TestFailedOrUnstableReadsDoNotGrantEditAuthority(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
	var tracker core.ReadTracker
	failure := errors.New("read failed")
	require.ErrorIs(t, tracker.Read(ctx, path, func() error { return failure }), failure)
	var unread *core.UnreadError
	require.ErrorAs(t, tracker.Edit(ctx, path, func() error { t.Fatal("failed read granted edit"); return nil }), &unread)
	var changed *core.ChangedError
	require.ErrorAs(t, tracker.Read(ctx, path, func() error { return os.WriteFile(path, []byte("new"), 0o600) }), &changed)
	require.ErrorAs(t, tracker.Edit(ctx, path, func() error { t.Fatal("unstable read granted edit"); return nil }), &unread)
}
