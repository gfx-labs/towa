package claude

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif" // Register GIF headers and first-frame decoding.
	"image/jpeg"
	"image/png"
	"math"

	"github.com/valyala/bytebufferpool"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // Register lossless and lossy WebP decoding.
)

// ImageLimits are the output dimensions and encoded-file byte budget. The
// Claude adapter passes 2000x2000 and tools.files.image_max_bytes, from
// chunk-6bcrsx06.js:11 in the captured 2.1.280 bundle.
type ImageLimits struct {
	MaxWidth  int
	MaxHeight int
	MaxBytes  int64
}

// NormalizedImage is either the unchanged original or a normalized PNG/JPEG.
// On the unchanged path Data aliases the caller's input.
type NormalizedImage struct {
	Data                          []byte
	MIMEType                      string
	OriginalWidth, OriginalHeight int
	Width, Height                 int
}

// ImageLimitError means the smallest upstream fallback still exceeds the
// requested byte budget. No over-budget image is returned.
type ImageLimitError struct{ Size, Limit int64 }

func (e *ImageLimitError) Error() string {
	return fmt.Sprintf("claude: normalized image is %d bytes, limit %d", e.Size, e.Limit)
}

// NormalizeImage preserves acceptable PNG/JPEG/GIF/WebP bytes. Otherwise it
// scales without enlargement and tries PNG then JPEG quality 80/60/40/20,
// following the captured sharp path's ordering, not its exact encoded bytes.
// Resized animations become a still frame. EXIF orientation is not applied.
func NormalizeImage(ctx context.Context, data []byte, limits ImageLimits) (NormalizedImage, error) {
	if limits.MaxWidth <= 0 || limits.MaxHeight <= 0 || limits.MaxBytes <= 0 {
		return NormalizedImage{}, fmt.Errorf("claude: image limits must be positive")
	}
	if err := ctx.Err(); err != nil {
		return NormalizedImage{}, err
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return NormalizedImage{}, fmt.Errorf("claude: image header: %w", err)
	}
	switch format {
	case "png", "jpeg", "gif", "webp":
	default:
		return NormalizedImage{}, fmt.Errorf("claude: unsupported image format %q", format)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return NormalizedImage{}, fmt.Errorf("claude: image dimensions must be positive")
	}
	result := NormalizedImage{OriginalWidth: cfg.Width, OriginalHeight: cfg.Height, Width: cfg.Width, Height: cfg.Height, MIMEType: "image/" + format}
	if int64(len(data)) <= limits.MaxBytes && cfg.Width <= limits.MaxWidth && cfg.Height <= limits.MaxHeight {
		result.Data = data
		return result, nil
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return NormalizedImage{}, fmt.Errorf("claude: image decode: %w", err)
	}
	w, h := fit(cfg.Width, cfg.Height, limits.MaxWidth, limits.MaxHeight)
	current := src
	if w != cfg.Width || h != cfg.Height {
		if err := ctx.Err(); err != nil {
			return NormalizedImage{}, err
		}
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
		current = dst
	}
	result.Width, result.Height = w, h
	// One message's encoder storage must not escape the pool.
	buf := bytebufferpool.Get()
	defer bytebufferpool.Put(buf)
	encode := func(img image.Image, kind string, quality int) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		buf.Reset()
		var err error
		if kind == "png" {
			err = (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(buf, img)
		} else {
			err = jpeg.Encode(buf, img, &jpeg.Options{Quality: quality})
		}
		if err != nil {
			return false, fmt.Errorf("claude: image encode: %w", err)
		}
		if int64(buf.Len()) > limits.MaxBytes {
			return false, nil
		}
		result.Data = bytes.Clone(buf.Bytes())
		result.MIMEType = "image/" + kind
		return true, nil
	}
	if format == "png" {
		ok, err := encode(current, "png", 0)
		if err != nil {
			return NormalizedImage{}, err
		}
		if ok {
			return result, nil
		}
	}
	for _, quality := range []int{80, 60, 40, 20} {
		ok, err := encode(current, "jpeg", quality)
		if err != nil {
			return NormalizedImage{}, err
		}
		if ok {
			return result, nil
		}
	}
	// The captured final fallback caps width at 1000 and uses JPEG quality 20.
	// Retain the fitted height if portrait, and never enlarge either dimension.
	fw := min(w, 1000)
	fh := max(1, int(math.Round(float64(h)*float64(fw)/float64(w))))
	if fw != w || fh != h {
		dst := image.NewRGBA(image.Rect(0, 0, fw, fh))
		draw.ApproxBiLinear.Scale(dst, dst.Bounds(), current, current.Bounds(), draw.Src, nil)
		current = dst
		result.Width, result.Height = fw, fh
	}
	ok, err := encode(current, "jpeg", 20)
	if err != nil {
		return NormalizedImage{}, err
	}
	if ok {
		return result, nil
	}
	return NormalizedImage{}, &ImageLimitError{Size: int64(buf.Len()), Limit: limits.MaxBytes}
}

func fit(w, h, mw, mh int) (int, int) {
	ratio := min(1, float64(mw)/float64(w), float64(mh)/float64(h))
	return max(1, min(mw, int(math.Round(float64(w)*ratio)))), max(1, min(mh, int(math.Round(float64(h)*ratio))))
}
