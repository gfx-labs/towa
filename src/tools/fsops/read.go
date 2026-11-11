package fsops

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// sniffBytes is how much of a file is looked at to tell text from binary.
// git looks for a NUL byte in the same first 8000 bytes.
const sniffBytes = 8000

// imageTypes are the image formats returned as an image. They are the four
// both the Anthropic and the OpenAI APIs accept.
var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// ReadInput names the file and the lines to read.
type ReadInput struct {
	Path string
	// Offset is the first line to return, counted from 1. Zero means 1.
	Offset int
	// Limit is the most lines to return. Zero means the configured default.
	Limit int
}

// ReadResult is a file's text or its image.
type ReadResult struct {
	// Path is the absolute path that was read, with links followed.
	Path string
	// Size is the file's size in bytes.
	Size int64
	// Text holds the lines that were read, with their line endings.
	Text string
	// FirstLine is the number of the first line in Text.
	FirstLine int
	// Lines is how many lines Text holds.
	Lines int
	// More is set when the file has lines after the ones in Text.
	More bool
	// Image is set when the file is an image. Text is then empty.
	Image *transcript.Blob
}

// Read returns a range of a text file's lines, or an image whole.
func Read(ctx context.Context, roots Roots, cfg config.ToolsFiles, in ReadInput) (ReadResult, error) {
	if in.Offset < 0 || in.Limit < 0 {
		return ReadResult{}, &Error{Kind: KindInvalidRange, Path: in.Path}
	}
	first, limit := max(in.Offset, 1), in.Limit
	if limit == 0 {
		limit = cfg.ReadDefaultLines
	}
	loc, err := roots.locate(in.Path)
	if err != nil {
		return ReadResult{}, err
	}
	root, err := loc.open()
	if err != nil {
		return ReadResult{}, err
	}
	defer root.Close() //nolint:errcheck // nothing was written through it
	// O_NONBLOCK keeps the open from waiting on a pipe nobody writes to. It
	// has no effect on a regular file.
	file, err := root.OpenFile(loc.rel, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ReadResult{}, classify(in.Path, err)
	}
	defer file.Close() //nolint:errcheck // read only
	info, err := file.Stat()
	if err != nil {
		return ReadResult{}, classify(in.Path, err)
	}
	if err := checkRegular(in.Path, info); err != nil {
		return ReadResult{}, err
	}
	res := ReadResult{Path: loc.abs(), Size: info.Size()}

	sniff := make([]byte, sniffBytes)
	n, err := io.ReadFull(file, sniff)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return ReadResult{}, classify(in.Path, err)
	}
	sniff = sniff[:n]
	mimeType := http.DetectContentType(sniff)
	if imageTypes[mimeType] {
		if info.Size() > cfg.ImageMaxBytes {
			return ReadResult{}, &Error{Kind: KindTooLarge, Path: in.Path, Size: info.Size(), Limit: cfg.ImageMaxBytes}
		}
		data, err := io.ReadAll(io.MultiReader(bytes.NewReader(sniff), file))
		if err != nil {
			return ReadResult{}, classify(in.Path, err)
		}
		res.Image = &transcript.Blob{MimeType: mimeType, Data: data}
		return res, nil
	}
	if bytes.IndexByte(sniff, 0) >= 0 {
		return ReadResult{}, &Error{Kind: KindBinary, Path: in.Path, MimeType: mimeType}
	}

	var text strings.Builder
	reader := bufio.NewReader(io.MultiReader(bytes.NewReader(sniff), file))
	line, kept, lineBytes := 1, 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return ReadResult{}, err
		}
		// ReadSlice returns a long line in pieces, so a line is never held
		// whole unless it is one that is kept.
		chunk, err := reader.ReadSlice('\n')
		if len(chunk) > 0 && line >= first {
			if kept == limit {
				res.More = true
				break
			}
			if int64(text.Len()+len(chunk)) > cfg.ReadMaxBytes {
				return ReadResult{}, &Error{
					Kind: KindTooLarge, Path: in.Path, Size: int64(text.Len() + len(chunk)), Limit: cfg.ReadMaxBytes,
				}
			}
			text.Write(chunk)
		}
		lineBytes += len(chunk)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return ReadResult{}, classify(in.Path, err)
		}
		if lineBytes > 0 {
			if line >= first {
				kept++
			}
			line++
			lineBytes = 0
		}
		if err != nil {
			break
		}
	}
	if total := line - 1; first > 1 && first > total {
		return ReadResult{}, &Error{Kind: KindPastEnd, Path: in.Path, Lines: total}
	}
	res.Text, res.FirstLine, res.Lines = text.String(), first, kept
	if !utf8.ValidString(res.Text) {
		return ReadResult{}, &Error{Kind: KindBinary, Path: in.Path, MimeType: mimeType}
	}
	return res, nil
}
