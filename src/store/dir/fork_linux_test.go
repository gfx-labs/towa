//go:build linux

package dir_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"uuid"

	"github.com/gfx-labs/towa/src/store"
	"github.com/gfx-labs/towa/src/store/dir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fork can encounter a source on another filesystem when a session
// directory is linked or mounted into the root. This makes os.Link fail
// with EXDEV on real files, and checks that the content is copied instead.
func TestForkCopiesBlobsAcrossFilesystems(t *testing.T) {
	sourceRoot, err := os.MkdirTemp("/dev/shm", "towa-fork-")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, os.RemoveAll(sourceRoot)) })
	source := open(t, sourceRoot, dir.CompressionZstd)
	sess := newSession(t, source)
	content := []byte("content copied from another filesystem")
	ref, err := source.PutBlob(t.Context(), sess.meta.ID, "text/plain", bytes.NewReader(content))
	require.NoError(t, err)
	root := t.TempDir()
	dest := open(t, root, dir.CompressionZstd)
	var a, b syscall.Stat_t
	require.NoError(t, syscall.Stat(sourceRoot, &a))
	require.NoError(t, syscall.Stat(root, &b))
	if a.Dev == b.Dev {
		t.Skip("/dev/shm and the test directory are on the same filesystem")
	}
	require.NoError(t, os.Symlink(source.SessionDir(sess.meta.ID), dest.SessionDir(sess.meta.ID)))
	meta := store.Meta{ID: uuid.NewV7(), Cwd: "/work/fork", Created: sess.now, Updated: sess.now}
	require.NoError(t, dest.Fork(t.Context(), sess.meta.ID, meta))
	srcBlob := filepath.Join(source.SessionDir(sess.meta.ID), "blobs", ref.Hash[len("sha256:"):])
	dstBlob := filepath.Join(dest.SessionDir(meta.ID), "blobs", ref.Hash[len("sha256:"):])
	srcInfo, err := os.Stat(srcBlob)
	require.NoError(t, err)
	dstInfo, err := os.Stat(dstBlob)
	require.NoError(t, err)
	assert.False(t, os.SameFile(srcInfo, dstInfo))
	require.NoError(t, source.Delete(t.Context(), sess.meta.ID))
	r, err := dest.OpenBlob(t.Context(), meta.ID, ref.Hash)
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	assert.Equal(t, content, got)
}
