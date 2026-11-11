package claude_test

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/image/draw"

	"github.com/gfx-labs/acpharness/src/tools/claude"
)

var imageFixtures = []string{"blue-purple-pink-large.png", "blue-purple-pink-large.lossless.webp", "blue-purple-pink-large.normal-filter.lossy.webp", "go-turns-two-280x360.jpeg", "video-001.gif"}

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "images", name))
	require.NoError(t, err)
	return data
}

func TestNormalizeImagesPreservesAcceptableOriginalsAndShrinksRealFormats(t *testing.T) {
	for _, name := range imageFixtures {
		t.Run(name, func(t *testing.T) {
			data := fixture(t, name)
			original, format, err := image.DecodeConfig(bytes.NewReader(data))
			require.NoError(t, err)
			unchanged, err := claude.NormalizeImage(t.Context(), data, claude.ImageLimits{MaxWidth: 2000, MaxHeight: 2000, MaxBytes: 3932160})
			require.NoError(t, err)
			require.Equal(t, data, unchanged.Data)
			require.Equal(t, &data[0], &unchanged.Data[0])
			require.Equal(t, "image/"+format, unchanged.MIMEType)
			limits := claude.ImageLimits{MaxWidth: max(1, original.Width/2), MaxHeight: max(1, original.Height/2), MaxBytes: 20000}
			result, err := claude.NormalizeImage(t.Context(), data, limits)
			require.NoError(t, err)
			decoded, encoded, err := image.Decode(bytes.NewReader(result.Data))
			require.NoError(t, err)
			require.Equal(t, "image/"+encoded, result.MIMEType)
			require.LessOrEqual(t, decoded.Bounds().Dx(), limits.MaxWidth)
			require.LessOrEqual(t, decoded.Bounds().Dy(), limits.MaxHeight)
			require.LessOrEqual(t, int64(len(result.Data)), limits.MaxBytes)
			require.Equal(t, original.Width, result.OriginalWidth)
			require.Equal(t, original.Height, result.OriginalHeight)
			// A subsequent encoder reusing pooled storage must not change this result.
			kept := bytes.Clone(result.Data)
			_, err = claude.NormalizeImage(t.Context(), data, limits)
			require.NoError(t, err)
			require.Equal(t, kept, result.Data)
		})
	}
}

func TestNormalizeImageEnforcesCapturedDimensionLimit(t *testing.T) {
	source, _, err := image.Decode(bytes.NewReader(fixture(t, "blue-purple-pink-large.png")))
	require.NoError(t, err)
	// Enlarge an actual upstream fixture, rather than inventing image content.
	large := image.NewRGBA(image.Rect(0, 0, 3000, 2000))
	draw.NearestNeighbor.Scale(large, large.Bounds(), source, source.Bounds(), draw.Src, nil)
	var encoded bytes.Buffer
	require.NoError(t, (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&encoded, large))
	got, err := claude.NormalizeImage(t.Context(), encoded.Bytes(), claude.ImageLimits{MaxWidth: 2000, MaxHeight: 2000, MaxBytes: 3932160})
	require.NoError(t, err)
	actual, _, err := image.DecodeConfig(bytes.NewReader(got.Data))
	require.NoError(t, err)
	require.Equal(t, 2000, actual.Width)
	require.Equal(t, 1333, actual.Height)
	require.LessOrEqual(t, len(got.Data), 3932160)
}

func TestNormalizeImageRefusesAnImpossibleBudgetOrInvalidInput(t *testing.T) {
	// Recompress an oversized encoded file even when its dimensions already fit.
	recompressed, err := claude.NormalizeImage(t.Context(), fixture(t, "blue-purple-pink-large.png"), claude.ImageLimits{MaxWidth: 2000, MaxHeight: 2000, MaxBytes: 20000})
	require.NoError(t, err)
	decoded, _, err := image.DecodeConfig(bytes.NewReader(recompressed.Data))
	require.NoError(t, err)
	require.Equal(t, 600, decoded.Width)
	require.Equal(t, 400, decoded.Height)
	require.LessOrEqual(t, len(recompressed.Data), 20000)
	limits := claude.ImageLimits{MaxWidth: 2000, MaxHeight: 2000, MaxBytes: 1}
	got, err := claude.NormalizeImage(t.Context(), fixture(t, "blue-purple-pink-large.png"), limits)
	var limit *claude.ImageLimitError
	require.ErrorAs(t, err, &limit)
	require.Empty(t, got.Data)
	require.Greater(t, limit.Size, limit.Limit)
	_, err = claude.NormalizeImage(t.Context(), []byte("not an image"), limits)
	require.Error(t, err)
	_, err = claude.NormalizeImage(t.Context(), nil, claude.ImageLimits{})
	require.Error(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = claude.NormalizeImage(ctx, fixture(t, "video-001.gif"), limits)
	require.ErrorIs(t, err, context.Canceled)
}

func BenchmarkNormalizeImage(b *testing.B) {
	data := fixture(b, "blue-purple-pink-large.png")
	for _, size := range []int{2000, 300} {
		name := "unchanged"
		if size == 300 {
			name = "resize"
		}
		b.Run(name, func(b *testing.B) {
			limits := claude.ImageLimits{MaxWidth: size, MaxHeight: size, MaxBytes: 3932160}
			b.ReportAllocs()
			for b.Loop() {
				_, err := claude.NormalizeImage(b.Context(), data, limits)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
