package claude_test

import (
	"bytes"
	"image"
	"os"
	"path/filepath"
	"testing"

	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/tools/claude"
	"github.com/gfx-labs/acpharness/src/tools/fsops"
	"github.com/stretchr/testify/require"
)

func TestReadFileNormalizesBeforeOutputLimitAndRecordsOnlySuccess(t *testing.T) {
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	cfg := loaded.Config.Tools.Files
	cfg.ImageMaxBytes = 20000
	dir := t.TempDir()
	path := filepath.Join(dir, "image.png")
	data := fixture(t, "blue-purple-pink-large.png")
	require.Greater(t, int64(len(data)), cfg.ImageMaxBytes)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	roots := fsops.Roots{Cwd: dir, Confine: true}
	input := fsops.ReadInput{Path: path}
	_, err = fsops.Read(t.Context(), roots, cfg, input)
	var tooLarge *fsops.Error
	require.ErrorAs(t, err, &tooLarge)
	require.Equal(t, fsops.KindTooLarge, tooLarge.Kind)

	var tracker claude.ReadTracker
	result, err := claude.ReadFile(t.Context(), roots, cfg, &tracker, input)
	require.NoError(t, err)
	require.NotNil(t, result.Image)
	require.LessOrEqual(t, int64(len(result.Image.Data)), cfg.ImageMaxBytes)
	decoded, _, err := image.Decode(bytes.NewReader(result.Image.Data))
	require.NoError(t, err)
	require.Equal(t, 600, decoded.Bounds().Dx())
	require.Equal(t, 400, decoded.Bounds().Dy())
	// This edit runs only if the actual successful read granted session authority.
	require.NoError(t, tracker.Edit(t.Context(), path, func() error {
		return os.WriteFile(path, data, 0o600)
	}))

	var failed claude.ReadTracker
	cfg.ImageMaxBytes = 1
	_, err = claude.ReadFile(t.Context(), roots, cfg, &failed, input)
	var imageLimit *claude.ImageLimitError
	require.ErrorAs(t, err, &imageLimit)
	var unread *claude.UnreadError
	require.ErrorAs(t, failed.Edit(t.Context(), path, func() error {
		t.Fatal("failed normalization must not grant edit authority")
		return nil
	}), &unread)
}
