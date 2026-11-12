package dir_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/gfx-labs/towa/src/store"
	"github.com/gfx-labs/towa/src/store/dir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListPagesAndChildrenSurviveChangesAndDirectoryMoves(t *testing.T) {
	root := t.TempDir()
	s := open(t, root, dir.CompressionZstd)
	parent := newSession(t, s)
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	var expected []store.Meta
	for i := range 12 {
		m := store.Meta{ID: uuid.NewV7(), Cwd: fmt.Sprintf("/work/%d", i%2), Created: now, Updated: now, Archived: i%3 == 0}
		if i%2 == 0 {
			m.ParentID = parent.meta.ID
		}
		require.NoError(t, s.Create(t.Context(), m))
		expected = append(expected, m)
	}
	// Paging applies all filters first. Even a deleted cursor is a position,
	// not a lookup for a session that must still exist.
	for _, f := range []store.Filter{
		{},
		{Cwd: "/work/0"},
		{Archived: store.ArchivedOnly},
		{Archived: store.ArchivedExclude},
		{ParentID: parent.meta.ID},
		{ParentID: parent.meta.ID, Archived: store.ArchivedExclude},
	} {
		whole, err := s.List(t.Context(), f)
		require.NoError(t, err)
		var paged []store.Meta
		f.Limit = 2
		for {
			page, err := s.List(t.Context(), f)
			require.NoError(t, err)
			if len(page) == 0 {
				break
			}
			paged = append(paged, page...)
			f.After = page[len(page)-1].ID
		}
		assert.Equal(t, whole, paged)
	}
	f := store.Filter{ParentID: parent.meta.ID}
	children, err := s.List(t.Context(), f)
	require.NoError(t, err)
	require.Len(t, children, 6)
	// A second process/value uses the saved index. Creating and deleting
	// invalidate it by changing the root namespace.
	reopened := open(t, root, dir.CompressionNone)
	added := store.Meta{ID: uuid.NewV7(), Cwd: "/work/0", ParentID: parent.meta.ID, Created: now, Updated: now}
	require.NoError(t, reopened.Create(t.Context(), added))
	children, err = s.List(t.Context(), f)
	require.NoError(t, err)
	require.Len(t, children, 7)
	assert.Equal(t, added.ID, children[6].ID)
	require.NoError(t, reopened.Delete(t.Context(), expected[0].ID))
	f.After = expected[0].ID
	f.Limit = 2
	children, err = s.List(t.Context(), f)
	require.NoError(t, err)
	require.Len(t, children, 2)
	assert.Equal(t, expected[2].ID, children[0].ID)
	assert.Equal(t, expected[4].ID, children[1].ID)
	// Removing a parent does not remove its children.
	require.NoError(t, s.Delete(t.Context(), parent.meta.ID))
	f.After = uuid.Nil()
	f.Limit = 0
	children, err = s.List(t.Context(), f)
	require.NoError(t, err)
	require.Len(t, children, 6)
	// Only the child's directory is copied to a fresh root. No root index
	// arrives with it, and its ParentID remains discoverable.
	moved := open(t, t.TempDir(), dir.CompressionNone)
	require.NoError(t, os.CopyFS(moved.SessionDir(added.ID), os.DirFS(s.SessionDir(added.ID))))
	children, err = moved.List(t.Context(), f)
	require.NoError(t, err)
	assert.Equal(t, []store.Meta{added}, children)
	// Adding a directory by copy to a root that already has its index also
	// invalidates the index, without an import command.
	another := expected[2]
	require.NoError(t, os.CopyFS(moved.SessionDir(another.ID), os.DirFS(s.SessionDir(another.ID))))
	children, err = moved.List(t.Context(), f)
	require.NoError(t, err)
	assert.Equal(t, []store.Meta{another, added}, children)
	// The index is derived, not session state. Deleting it rebuilds it.
	require.NoError(t, os.RemoveAll(filepath.Join(filepath.Dir(moved.SessionDir(added.ID)), ".index")))
	children, err = moved.List(t.Context(), f)
	require.NoError(t, err)
	assert.Equal(t, []store.Meta{another, added}, children)
	_, err = moved.List(t.Context(), store.Filter{Limit: -1})
	require.Error(t, err)
	_, err = moved.List(t.Context(), store.Filter{Archived: store.Archived(99)})
	require.Error(t, err)
}
