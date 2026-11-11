package fsops_test

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/tools/fsops"
)

func filesConfig(t *testing.T) config.ToolsFiles {
	t.Helper()
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	return loaded.Config.Tools.Files
}

func put(t *testing.T, path, body string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

// requireKind returns the error by value: a helper that returns an error is
// one whose result lint wants checked.
func requireKind(t *testing.T, err error, kind fsops.Kind) fsops.Error {
	t.Helper()
	var e *fsops.Error
	require.ErrorAs(t, err, &e)
	require.Equal(t, kind, e.Kind)
	return *e
}

// session returns a working directory, an additional directory and a
// directory that is in neither, each holding one file.
func session(t *testing.T) (roots fsops.Roots, outside string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	cwd, extra, outside := filepath.Join(base, "cwd"), filepath.Join(base, "extra"), filepath.Join(base, "outside")
	put(t, filepath.Join(cwd, "in.txt"), "inside\n")
	put(t, filepath.Join(extra, "more.txt"), "extra\n")
	put(t, filepath.Join(outside, "secret.txt"), "secret\n")
	return fsops.Roots{Cwd: cwd, Dirs: []string{extra}}, outside
}

// By default nothing is confined: a path outside the session directories is
// read and written, and a relative path is still taken from Cwd.
func TestPathsOutsideTheSessionDirectoriesAreAllowedByDefault(t *testing.T) {
	roots, outside := session(t)
	cfg, ctx := filesConfig(t), context.Background()
	require.False(t, cfg.Confine)
	secret := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.Symlink(outside, filepath.Join(roots.Cwd, "linkdir")))

	for _, path := range []string{secret, "../outside/secret.txt", "linkdir/secret.txt"} {
		res, err := fsops.Read(ctx, roots, cfg, fsops.ReadInput{Path: path})
		require.NoError(t, err, path)
		require.Equal(t, "secret\n", res.Text)
		require.Equal(t, secret, res.Path)
	}

	change, err := fsops.Write(ctx, roots, cfg, fsops.WriteInput{Path: filepath.Join(outside, "new/deep/file.txt"), Content: "made\n"})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(outside, "new/deep/file.txt"), change.Path)
	_, err = fsops.Write(ctx, roots, cfg, fsops.WriteInput{Path: secret, Content: "open\n"})
	require.NoError(t, err)
	_, err = fsops.ApplyEdits(ctx, roots, cfg, fsops.EditInput{Path: "linkdir/secret.txt", Edits: []fsops.Edit{{Old: "open", New: "edited"}}})
	require.NoError(t, err)
	got, err := os.ReadFile(secret)
	require.NoError(t, err)
	require.Equal(t, "edited\n", string(got))

	res, err := fsops.Read(ctx, roots, cfg, fsops.ReadInput{Path: "in.txt"})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(roots.Cwd, "in.txt"), res.Path)
}

func TestPathsAreConfinedToTheSessionDirectoriesWhenAsked(t *testing.T) {
	roots, outside := session(t)
	roots.Confine = true
	cfg, ctx := filesConfig(t), context.Background()
	secret := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.Symlink(secret, filepath.Join(roots.Cwd, "link.txt")))
	require.NoError(t, os.Symlink(outside, filepath.Join(roots.Cwd, "linkdir")))
	require.NoError(t, os.Symlink(filepath.Join(outside, "absent.txt"), filepath.Join(roots.Cwd, "dangling")))
	require.NoError(t, os.Symlink(filepath.Join(roots.Dirs[0], "more.txt"), filepath.Join(roots.Cwd, "to-extra")))

	for _, path := range []string{
		secret,
		"../outside/secret.txt",
		"link.txt",
		"linkdir/secret.txt",
		"linkdir/new.txt",
		"linkdir/sub/new.txt",
	} {
		_, err := fsops.Read(ctx, roots, cfg, fsops.ReadInput{Path: path})
		requireKind(t, err, fsops.KindOutside)
		_, err = fsops.Write(ctx, roots, cfg, fsops.WriteInput{Path: path, Content: "x"})
		requireKind(t, err, fsops.KindOutside)
		_, err = fsops.ApplyEdits(ctx, roots, cfg, fsops.EditInput{Path: path, Edits: []fsops.Edit{{Old: "secret", New: "x"}}})
		requireKind(t, err, fsops.KindOutside)
	}
	// A link to nowhere cannot be written through: the file it would make
	// is outside.
	_, err := fsops.Write(ctx, roots, cfg, fsops.WriteInput{Path: "dangling", Content: "x"})
	require.Error(t, err)
	require.NoFileExists(t, filepath.Join(outside, "absent.txt"))
	require.NoDirExists(t, filepath.Join(outside, "sub"))
	got, err := os.ReadFile(secret)
	require.NoError(t, err)
	require.Equal(t, "secret\n", string(got))

	// The additional directory is allowed, by its own path and through a
	// link from the working directory.
	for _, path := range []string{filepath.Join(roots.Dirs[0], "more.txt"), "to-extra"} {
		res, err := fsops.Read(ctx, roots, cfg, fsops.ReadInput{Path: path})
		require.NoError(t, err, path)
		require.Equal(t, "extra\n", res.Text)
		require.Equal(t, filepath.Join(roots.Dirs[0], "more.txt"), res.Path)
	}
}

func TestReadReturnsTheLinesAskedFor(t *testing.T) {
	roots, _ := session(t)
	cfg, ctx := filesConfig(t), context.Background()
	put(t, filepath.Join(roots.Cwd, "five.txt"), "one\ntwo\nthree\nfour\nfive")

	res, err := fsops.Read(ctx, roots, cfg, fsops.ReadInput{Path: "five.txt", Offset: 2, Limit: 2})
	require.NoError(t, err)
	require.Equal(t, "two\nthree\n", res.Text)
	require.Equal(t, 2, res.FirstLine)
	require.Equal(t, 2, res.Lines)
	require.True(t, res.More)

	// The last line has no newline and is still a line.
	res, err = fsops.Read(ctx, roots, cfg, fsops.ReadInput{Path: "five.txt", Offset: 4})
	require.NoError(t, err)
	require.Equal(t, "four\nfive", res.Text)
	require.Equal(t, 2, res.Lines)
	require.False(t, res.More)

	_, err = fsops.Read(ctx, roots, cfg, fsops.ReadInput{Path: "five.txt", Offset: 6})
	require.Equal(t, 5, requireKind(t, err, fsops.KindPastEnd).Lines)
	_, err = fsops.Read(ctx, roots, cfg, fsops.ReadInput{Path: "five.txt", Offset: -1})
	requireKind(t, err, fsops.KindInvalidRange)
	_, err = fsops.Read(ctx, roots, cfg, fsops.ReadInput{Path: "absent.txt"})
	requireKind(t, err, fsops.KindNotFound)
	_, err = fsops.Read(ctx, roots, cfg, fsops.ReadInput{Path: "."})
	requireKind(t, err, fsops.KindIsDirectory)

	put(t, filepath.Join(roots.Cwd, "empty.txt"), "")
	res, err = fsops.Read(ctx, roots, cfg, fsops.ReadInput{Path: "empty.txt"})
	require.NoError(t, err)
	require.Zero(t, res.Lines)

	// A range over the byte limit is refused whole, not cut: a line longer
	// than the bufio buffer exercises the piecewise path too.
	cfg.ReadMaxBytes = 100
	put(t, filepath.Join(roots.Cwd, "long.txt"), "short\n"+strings.Repeat("x", 10000)+"\nlast\n")
	_, err = fsops.Read(ctx, roots, cfg, fsops.ReadInput{Path: "long.txt"})
	require.Equal(t, int64(100), requireKind(t, err, fsops.KindTooLarge).Limit)
	res, err = fsops.Read(ctx, roots, cfg, fsops.ReadInput{Path: "long.txt", Offset: 3})
	require.NoError(t, err)
	require.Equal(t, "last\n", res.Text)
}

func TestReadTellsImagesAndBinaryFromText(t *testing.T) {
	roots, _ := session(t)
	cfg, ctx := filesConfig(t), context.Background()

	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	put(t, filepath.Join(roots.Cwd, "pixel.dat"), buf.String())
	res, err := fsops.Read(ctx, roots, cfg, fsops.ReadInput{Path: "pixel.dat"})
	require.NoError(t, err)
	require.NotNil(t, res.Image)
	require.Equal(t, "image/png", res.Image.MimeType)
	require.Equal(t, buf.Bytes(), res.Image.Data)
	require.Empty(t, res.Text)

	cfg.ImageMaxBytes = 10
	_, err = fsops.Read(ctx, roots, cfg, fsops.ReadInput{Path: "pixel.dat"})
	requireKind(t, err, fsops.KindTooLarge)

	self, err := os.Executable()
	require.NoError(t, err)
	exe, err := os.ReadFile(self)
	require.NoError(t, err)
	put(t, filepath.Join(roots.Cwd, "prog"), string(exe[:20000]))
	_, err = fsops.Read(ctx, roots, cfg, fsops.ReadInput{Path: "prog"})
	requireKind(t, err, fsops.KindBinary)

	// Latin-1 text has no NUL byte and is still not text we can send.
	put(t, filepath.Join(roots.Cwd, "latin1.txt"), "caf\xe9\n")
	_, err = fsops.Read(ctx, roots, cfg, fsops.ReadInput{Path: "latin1.txt"})
	requireKind(t, err, fsops.KindBinary)
}

func TestWriteCreatesAndReplacesAndReportsTheChange(t *testing.T) {
	roots, _ := session(t)
	cfg, ctx := filesConfig(t), context.Background()

	change, err := fsops.Write(ctx, roots, cfg, fsops.WriteInput{Path: "a/b/new.txt", Content: "hello\n"})
	require.NoError(t, err)
	require.False(t, change.Existed)
	diff := change.Diff().Diff
	require.Nil(t, diff.OldText)
	require.Equal(t, "hello\n", diff.NewText)
	require.Equal(t, filepath.Join(roots.Cwd, "a/b/new.txt"), diff.Path)

	script := put(t, filepath.Join(roots.Cwd, "run.sh"), "#!/bin/sh\n")
	require.NoError(t, os.Chmod(script, 0o750))
	change, err = fsops.Write(ctx, roots, cfg, fsops.WriteInput{Path: script, Content: "#!/bin/sh\necho hi\n"})
	require.NoError(t, err)
	require.Equal(t, "#!/bin/sh\n", *change.Diff().Diff.OldText)
	info, err := os.Stat(script)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o750), info.Mode().Perm())
	got, err := os.ReadFile(script)
	require.NoError(t, err)
	require.Equal(t, "#!/bin/sh\necho hi\n", string(got))

	_, err = fsops.Write(ctx, roots, cfg, fsops.WriteInput{Path: "a", Content: "x"})
	requireKind(t, err, fsops.KindIsDirectory)

	// No temporary file is left behind.
	entries, err := os.ReadDir(roots.Cwd)
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), ".tmp")
	}
}

func TestReadTextReturnsAWholeFileForRewriting(t *testing.T) {
	roots, outside := session(t)
	cfg, ctx := filesConfig(t), context.Background()
	// Longer than a line-ranged read allows, and still within the edit limit.
	cfg.ReadMaxBytes = 10
	body := strings.Repeat("line\n", 100)
	put(t, filepath.Join(roots.Cwd, "big.txt"), body)

	abs, text, err := fsops.ReadText(ctx, roots, cfg, "big.txt")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(roots.Cwd, "big.txt"), abs)
	require.Equal(t, body, text)

	put(t, filepath.Join(roots.Cwd, "latin1.txt"), "caf\xe9\n")
	_, _, err = fsops.ReadText(ctx, roots, cfg, "latin1.txt")
	requireKind(t, err, fsops.KindBinary)
	_, _, err = fsops.ReadText(ctx, roots, cfg, "absent.txt")
	requireKind(t, err, fsops.KindNotFound)
	_, _, err = fsops.ReadText(ctx, roots, cfg, ".")
	requireKind(t, err, fsops.KindIsDirectory)
	cfg.EditMaxBytes = 100
	_, _, err = fsops.ReadText(ctx, roots, cfg, "big.txt")
	require.Equal(t, int64(500), requireKind(t, err, fsops.KindTooLarge).Size)

	roots.Confine = true
	_, _, err = fsops.ReadText(ctx, roots, cfg, filepath.Join(outside, "secret.txt"))
	requireKind(t, err, fsops.KindOutside)
}

func TestRemoveDeletesOneFileAndReportsWhatItHeld(t *testing.T) {
	roots, outside := session(t)
	cfg, ctx := filesConfig(t), context.Background()

	change, err := fsops.Remove(ctx, roots, cfg, "in.txt")
	require.NoError(t, err)
	require.True(t, change.Removed)
	require.Equal(t, "inside\n", change.OldText)
	diff := change.Diff().Diff
	require.Equal(t, filepath.Join(roots.Cwd, "in.txt"), diff.Path)
	require.Equal(t, "inside\n", *diff.OldText)
	require.Empty(t, diff.NewText)
	require.NoFileExists(t, filepath.Join(roots.Cwd, "in.txt"))

	_, err = fsops.Remove(ctx, roots, cfg, "in.txt")
	requireKind(t, err, fsops.KindNotFound)
	require.NoError(t, os.Mkdir(filepath.Join(roots.Cwd, "dir"), 0o755))
	_, err = fsops.Remove(ctx, roots, cfg, "dir")
	requireKind(t, err, fsops.KindIsDirectory)
	require.DirExists(t, filepath.Join(roots.Cwd, "dir"))

	// A move is a write to the new path and a remove of the old one.
	_, text, err := fsops.ReadText(ctx, roots, cfg, filepath.Join(roots.Dirs[0], "more.txt"))
	require.NoError(t, err)
	_, err = fsops.Write(ctx, roots, cfg, fsops.WriteInput{Path: "moved/more.txt", Content: text})
	require.NoError(t, err)
	_, err = fsops.Remove(ctx, roots, cfg, filepath.Join(roots.Dirs[0], "more.txt"))
	require.NoError(t, err)
	require.NoFileExists(t, filepath.Join(roots.Dirs[0], "more.txt"))
	got, err := os.ReadFile(filepath.Join(roots.Cwd, "moved/more.txt"))
	require.NoError(t, err)
	require.Equal(t, "extra\n", string(got))
	// And a file just written can be removed again.
	change, err = fsops.Remove(ctx, roots, cfg, "moved/more.txt")
	require.NoError(t, err)
	require.Equal(t, "extra\n", change.OldText)

	// A file that is not text is removed without its content being kept.
	put(t, filepath.Join(roots.Cwd, "blob"), "\x00\xff\x00")
	change, err = fsops.Remove(ctx, roots, cfg, "blob")
	require.NoError(t, err)
	require.True(t, change.OldBinary)
	require.Nil(t, change.Diff().Diff.OldText)

	secret := filepath.Join(outside, "secret.txt")
	confined := roots
	confined.Confine = true
	_, err = fsops.Remove(ctx, confined, cfg, secret)
	requireKind(t, err, fsops.KindOutside)
	require.FileExists(t, secret)
	_, err = fsops.Remove(ctx, roots, cfg, secret)
	require.NoError(t, err)
	require.NoFileExists(t, secret)
}

func TestEditReplacesExactTextOnce(t *testing.T) {
	roots, _ := session(t)
	cfg, ctx := filesConfig(t), context.Background()
	const original = "a := 1\nb := 1\nc := 2\n"
	path := put(t, filepath.Join(roots.Cwd, "code.go"), original)
	edit := func(edits ...fsops.Edit) (fsops.EditResult, error) {
		return fsops.ApplyEdits(ctx, roots, cfg, fsops.EditInput{Path: "code.go", Edits: edits})
	}
	unchanged := func() {
		t.Helper()
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, original, string(got))
	}

	_, err := edit(fsops.Edit{Old: ":= 1", New: ":= 9"})
	require.Equal(t, 2, requireKind(t, err, fsops.KindNotUnique).Count)
	_, err = edit(fsops.Edit{Old: "missing", New: "x"})
	requireKind(t, err, fsops.KindNoMatch)
	_, err = edit(fsops.Edit{Old: "", New: "x"})
	requireKind(t, err, fsops.KindEmptyOld)
	_, err = edit(fsops.Edit{Old: "c := 2", New: "c := 2"})
	requireKind(t, err, fsops.KindSameText)
	_, err = edit()
	requireKind(t, err, fsops.KindNoEdits)
	// The first edit is fine and the second fails: nothing is written.
	_, err = edit(fsops.Edit{Old: "c := 2", New: "c := 3"}, fsops.Edit{Old: "missing", New: "x"})
	require.Equal(t, 1, requireKind(t, err, fsops.KindNoMatch).Edit)
	// Two edits that undo each other change nothing.
	_, err = edit(fsops.Edit{Old: "c := 2", New: "c := 3"}, fsops.Edit{Old: "c := 3", New: "c := 2"})
	requireKind(t, err, fsops.KindNoChange)
	unchanged()

	// The second edit matches text the first one made.
	res, err := edit(
		fsops.Edit{Old: ":= 1", New: ":= 7", All: true},
		fsops.Edit{Old: "b := 7", New: "b := 8"},
	)
	require.NoError(t, err)
	require.Equal(t, []int{2, 1}, res.Replaced)
	const want = "a := 7\nb := 8\nc := 2\n"
	require.Equal(t, original, *res.Diff().Diff.OldText)
	require.Equal(t, want, res.Diff().Diff.NewText)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, want, string(got))
}
