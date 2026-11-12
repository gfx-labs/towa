package patch_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/tools/fsops"
	"github.com/gfx-labs/towa/src/tools/patch"
	"github.com/stretchr/testify/require"
)

// Inputs from codex-rs/apply-patch/tests/suite/tool.rs:108-183 at ce0323f2.
func TestUpstreamLineEndingUpdates(t *testing.T) {
	for _, tc := range []struct{ name, old, body, want string }{
		{"crlf", "one\r\ntwo\r\nthree\r\n", "@@\n-one\n+uno\n@@\n two\n+\n+between\n three", "uno\r\ntwo\r\n\r\nbetween\r\nthree\r\n"},
		{"cr", "one\rtwo\rthree\r", "@@\n-one\n+uno\n@@\n two\n+\n+between\n three", "uno\rtwo\r\rbetween\rthree\r"},
		{"repeated", "a\nb\n", "@@\n-a\n-b\n+b\n+b\n+a", "b\nb\na\n"},
		{"repeated_context", "same\r\nsame\n", "@@\n-same\n same", "same\n"},
		{"unterminated", "one\r\ntwo", "@@\n-one\n+ONE", "ONE\r\ntwo\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "file.txt")
			require.NoError(t, os.WriteFile(path, []byte(tc.old), 0o600))
			p, err := patch.Parse("*** Begin Patch\n*** Update File: file.txt\n" + tc.body + "\n*** End Patch")
			require.NoError(t, err)
			applied, err := patch.Apply(context.Background(), fsops.Roots{Cwd: dir}, config.ToolsFiles{EditMaxBytes: 4 << 20}, p)
			require.NoError(t, err)
			require.Equal(t, "Success. Updated the following files:\nM file.txt\n", applied.Summary())
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, tc.want, string(got))
		})
	}
}

// Parser inputs from parser.rs:test_parse_patch_lenient at ce0323f2.
func TestUpstreamHeredocs(t *testing.T) {
	text := "*** Begin Patch\n*** Update File: file2.py\n import foo\n+bar\n*** End Patch"
	for _, wrapper := range []string{"<<EOF", "<<'EOF'", "<<\"EOF\""} {
		p, err := patch.Parse(wrapper + "\n" + text + "\nEOF\n")
		require.NoError(t, err)
		got, err := patch.Derive("file2.py", "import foo\n", p.Hunks[0].Chunks)
		require.NoError(t, err)
		require.Equal(t, "import foo\nbar\n", got)
	}
	_, err := patch.Parse("<<\"EOF'\n" + text + "\nEOF\n")
	require.Error(t, err)
}

// A move whose destination names the source updates it in place. Removing
// the source after the write used to delete the file.
func TestMoveToSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		dest func(t *testing.T, dir string) string
	}{
		{"same", func(*testing.T, string) string { return "file.txt" }},
		{"dot", func(*testing.T, string) string { return "./file.txt" }},
		{"absolute", func(_ *testing.T, dir string) string { return filepath.Join(dir, "file.txt") }},
		{"symlink", func(t *testing.T, dir string) string {
			require.NoError(t, os.Symlink("file.txt", filepath.Join(dir, "link.txt")))
			return "link.txt"
		}},
		{"case", func(t *testing.T, dir string) string {
			// Only a file system that ignores case has this alias.
			if _, err := os.Stat(filepath.Join(dir, "FILE.txt")); err != nil {
				t.Skip("the file system distinguishes case")
			}
			return "FILE.txt"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "file.txt")
			require.NoError(t, os.WriteFile(path, []byte("one\ntwo\n"), 0o600))
			p, err := patch.Parse("*** Begin Patch\n*** Update File: file.txt\n*** Move to: " + tc.dest(t, dir) + "\n@@\n-one\n+uno\n two\n*** End Patch")
			require.NoError(t, err)
			_, err = patch.Apply(context.Background(), fsops.Roots{Cwd: dir}, config.ToolsFiles{EditMaxBytes: 4 << 20}, p)
			require.NoError(t, err)
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, "uno\ntwo\n", string(got))
		})
	}

	// A hard link is another name: moving onto it removes the source name.
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("one\ntwo\n"), 0o600))
	require.NoError(t, os.Link(path, filepath.Join(dir, "hard.txt")))
	p, err := patch.Parse("*** Begin Patch\n*** Update File: file.txt\n*** Move to: hard.txt\n@@\n-one\n+uno\n two\n*** End Patch")
	require.NoError(t, err)
	_, err = patch.Apply(context.Background(), fsops.Roots{Cwd: dir}, config.ToolsFiles{EditMaxBytes: 4 << 20}, p)
	require.NoError(t, err)
	got, err := os.ReadFile(filepath.Join(dir, "hard.txt"))
	require.NoError(t, err)
	require.Equal(t, "uno\ntwo\n", string(got))
	require.NoFileExists(t, path)
}
