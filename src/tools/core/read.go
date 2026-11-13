package core

import (
	"context"
	"fmt"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/tools/fsops"
	"github.com/gfx-labs/towa/src/transcript"
)

// ReadFile reads a file for the read tool, before the result is formatted.
// The session records a full-file fingerprint only after the read and any image
// normalization succeed. Source images have no separate encoded-byte limit.
func ReadFile(ctx context.Context, roots fsops.Roots, cfg config.ToolsFiles, tracker *ReadTracker, in fsops.ReadInput) (fsops.ReadResult, error) {
	if tracker == nil {
		return fsops.ReadResult{}, fmt.Errorf("core: read tracker is required")
	}
	path, err := roots.Resolve(in.Path)
	if err != nil {
		return fsops.ReadResult{}, err
	}
	var result fsops.ReadResult
	err = tracker.Read(ctx, path, func() error {
		var readErr error
		result, readErr = fsops.Read(ctx, roots, cfg, in, func(ctx context.Context, source transcript.Blob) (transcript.Blob, error) {
			// Captured 2.1.280 chunk-6bcrsx06.js:11 sets both dimensions to 2000.
			image, imageErr := NormalizeImage(ctx, source.Data, ImageLimits{MaxWidth: 2000, MaxHeight: 2000, MaxBytes: cfg.ImageMaxBytes})
			if imageErr != nil {
				return transcript.Blob{}, imageErr
			}
			return transcript.Blob{MimeType: image.MIMEType, Data: image.Data}, nil
		})
		return readErr
	})
	if err != nil {
		return fsops.ReadResult{}, err
	}
	return result, nil
}
