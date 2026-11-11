package search_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/fsops"
	"github.com/gfx-labs/acpharness/src/tools/search"
)

func toolsConfig(t *testing.T) config.Tools {
	t.Helper()
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	return loaded.Config.Tools
}

// tree writes a small source tree and returns the session over it.
func tree(t *testing.T) tools.Env {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	cwd := filepath.Join(base, "cwd")
	files := map[string]string{
		"cwd/a.go":           "package a\n\n// Alpha does a thing.\nfunc Alpha() {}\n\nfunc beta() {}\n",
		"cwd/sub/b.go":       "package sub\n\nfunc Alpha2() {}\n",
		"cwd/sub/notes.md":   "alpha in prose\n",
		"cwd/.hidden/c.go":   "func Alpha3() {}\n",
		"cwd/.ignore":        "ignored/\n",
		"cwd/ignored/d.go":   "func Alpha4() {}\n",
		"cwd/latin1.txt":     "Alpha caf\xe9\n",
		"outside/secret.txt": "Alpha secret\n",
	}
	for name, body := range files {
		path := filepath.Join(base, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	}
	// rg does not follow links, so a link out of the tree is not searched.
	require.NoError(t, os.Symlink(filepath.Join(base, "outside"), filepath.Join(cwd, "out")))
	return tools.Env{Cwd: cwd}
}

func paths(env tools.Env, files []search.File) []string {
	out := make([]string, len(files))
	for i, f := range files {
		rel, err := filepath.Rel(env.Cwd, f.Path)
		if err != nil {
			rel = f.Path
		}
		out[i] = rel
	}
	return out
}

func TestGrepFindsLinesWithContext(t *testing.T) {
	env, cfg, ctx := tree(t), toolsConfig(t), context.Background()

	res, err := search.Grep(ctx, env, cfg, search.GrepInput{Pattern: `func Alpha\d?\(`, Before: 1, Filter: search.Filter{Types: []string{"go"}}})
	require.NoError(t, err)
	a, b := filepath.Join(env.Cwd, "a.go"), filepath.Join(env.Cwd, "sub/b.go")
	require.Equal(t, []search.Line{
		{Path: a, Number: 3, Text: "// Alpha does a thing.", Context: true},
		{Path: a, Number: 4, Text: "func Alpha() {}"},
		{Path: b, Number: 2, Text: "", Context: true},
		{Path: b, Number: 3, Text: "func Alpha2() {}"},
	}, res.Lines)

	// A literal pattern is not a regular expression, and a line that is not
	// UTF-8 comes back with the bad byte replaced.
	res, err = search.Grep(ctx, env, cfg, search.GrepInput{Pattern: "Alpha(", Literal: true})
	require.NoError(t, err)
	require.Len(t, res.Lines, 1)
	res, err = search.Grep(ctx, env, cfg, search.GrepInput{Pattern: "caf", Filter: search.Filter{Path: "latin1.txt"}})
	require.NoError(t, err)
	require.Equal(t, "Alpha caf\uFFFD", res.Lines[0].Text)

	res, err = search.Grep(ctx, env, cfg, search.GrepInput{Pattern: `Alpha\(\) \{\}\n\nfunc beta`, Multiline: true})
	require.NoError(t, err)
	require.Len(t, res.Lines, 1)
	require.Equal(t, "func Alpha() {}\n\nfunc beta() {}", res.Lines[0].Text)

	res, err = search.Grep(ctx, env, cfg, search.GrepInput{Pattern: "no such text anywhere"})
	require.NoError(t, err)
	require.Empty(t, res.Lines)
}

func TestGrepListsFilesAndCounts(t *testing.T) {
	env, cfg, ctx := tree(t), toolsConfig(t), context.Background()

	res, err := search.Grep(ctx, env, cfg, search.GrepInput{Pattern: "alpha", IgnoreCase: true, Output: search.Files})
	require.NoError(t, err)
	require.Equal(t, []string{"a.go", "latin1.txt", "sub/b.go", "sub/notes.md"}, paths(env, res.Files))
	require.False(t, res.Files[0].ModTime.IsZero())

	res, err = search.Grep(ctx, env, cfg, search.GrepInput{
		Pattern: "alpha", IgnoreCase: true, Output: search.Files,
		Filter: search.Filter{Hidden: true, NoIgnore: true, Globs: []string{"*.go"}},
	})
	require.NoError(t, err)
	require.Equal(t, []string{".hidden/c.go", "a.go", "ignored/d.go", "sub/b.go"}, paths(env, res.Files))

	res, err = search.Grep(ctx, env, cfg, search.GrepInput{Pattern: "Alpha", Output: search.Counts, Filter: search.Filter{Types: []string{"go"}}})
	require.NoError(t, err)
	require.Equal(t, []string{"a.go", "sub/b.go"}, paths(env, res.Files))
	require.Equal(t, 2, res.Files[0].Count)
	require.Equal(t, 1, res.Files[1].Count)
}

func TestFindListsFilesWithOffsetAndLimit(t *testing.T) {
	env, cfg, ctx := tree(t), toolsConfig(t), context.Background()

	res, err := search.Find(ctx, env, cfg, search.Filter{})
	require.NoError(t, err)
	require.Equal(t, []string{"a.go", "latin1.txt", "sub/b.go", "sub/notes.md"}, paths(env, res.Files))
	require.False(t, res.More)

	res, err = search.Find(ctx, env, cfg, search.Filter{Offset: 1, Limit: 2})
	require.NoError(t, err)
	require.Equal(t, []string{"latin1.txt", "sub/b.go"}, paths(env, res.Files))
	require.True(t, res.More)

	res, err = search.Find(ctx, env, cfg, search.Filter{Path: "sub", Globs: []string{"*.md"}})
	require.NoError(t, err)
	require.Equal(t, []string{"sub/notes.md"}, paths(env, res.Files))

	// Results over the output size stop the search and say so.
	cfg.MaxOutputBytes = int64(len(filepath.Join(env.Cwd, "a.go")) + 1)
	res, err = search.Find(ctx, env, cfg, search.Filter{})
	require.NoError(t, err)
	require.Equal(t, []string{"a.go"}, paths(env, res.Files))
	require.True(t, res.Cut)
}

func TestSearchRefusesWhatItCannotDo(t *testing.T) {
	env, cfg, ctx := tree(t), toolsConfig(t), context.Background()
	var serr *search.Error
	var ferr *fsops.Error

	_, err := search.Grep(ctx, env, cfg, search.GrepInput{Pattern: "a("})
	require.ErrorAs(t, err, &serr)
	require.Equal(t, search.KindInvalid, serr.Kind)
	require.Contains(t, serr.Message, "unclosed group")

	_, err = search.Find(ctx, env, cfg, search.Filter{Types: []string{"nosuchtype"}})
	require.ErrorAs(t, err, &serr)
	require.Equal(t, search.KindInvalid, serr.Kind)

	// A path outside the session directories is searched unless
	// confinement is on.
	confined := cfg
	confined.Files.Confine = true
	for _, path := range []string{"out", "out/secret.txt", filepath.Dir(env.Cwd)} {
		res, err := search.Grep(ctx, env, cfg, search.GrepInput{Pattern: "Alpha secret", Filter: search.Filter{Path: path}})
		require.NoError(t, err, path)
		require.Len(t, res.Lines, 1, path)
		_, err = search.Grep(ctx, env, confined, search.GrepInput{Pattern: "Alpha", Filter: search.Filter{Path: path}})
		require.ErrorAs(t, err, &ferr, path)
		require.Equal(t, fsops.KindOutside, ferr.Kind)
	}
	_, err = search.Find(ctx, env, cfg, search.Filter{Path: "absent"})
	require.ErrorAs(t, err, &ferr)
	require.Equal(t, fsops.KindNotFound, ferr.Kind)

	cfg.Search.Ripgrep = filepath.Join(env.Cwd, "no-rg-here")
	_, err = search.Grep(ctx, env, cfg, search.GrepInput{Pattern: "Alpha"})
	require.ErrorAs(t, err, &serr)
	require.Equal(t, search.KindNoRipgrep, serr.Kind)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = search.Find(cancelled, env, toolsConfig(t), search.Filter{})
	require.ErrorIs(t, err, context.Canceled)
}
