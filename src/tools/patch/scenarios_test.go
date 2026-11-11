package patch_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/tools/fsops"
	"github.com/gfx-labs/acpharness/src/tools/patch"
	"github.com/stretchr/testify/require"
)

// Port of codex-rs/apply-patch/tests/suite/scenarios.rs at ce0323f2.
// The upstream fixtures specify final state, including partial application.
func TestUpstreamScenarios(t *testing.T) {
	cases, err := os.ReadDir("testdata/scenarios")
	require.NoError(t, err)
	for _, entry := range cases {
		if !entry.IsDir() {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			dir := t.TempDir()
			fixture := filepath.Join("testdata/scenarios", entry.Name())
			input := filepath.Join(fixture, "input")
			if _, err := os.Stat(input); err == nil {
				require.NoError(t, os.CopyFS(dir, os.DirFS(input)))
			}
			data, err := os.ReadFile(filepath.Join(fixture, "patch.txt"))
			require.NoError(t, err)
			p, err := patch.Parse(string(data))
			if err == nil {
				_, _ = patch.Apply(context.Background(), fsops.Roots{Cwd: dir}, config.ToolsFiles{EditMaxBytes: 4 << 20}, p)
			}
			require.Equal(t, snapshot(t, filepath.Join(fixture, "expected")), snapshot(t, dir))
		})
	}
}

func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return out
	}
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			out[rel] = "directory"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = "file:" + string(data)
		return nil
	}))
	return out
}
