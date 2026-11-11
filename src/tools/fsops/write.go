package fsops

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/config"
)

// Change is what a write or an edit did to one file.
type Change struct {
	// Path is the absolute path that was written, with links followed.
	Path string
	// Existed says the file was there before.
	Existed bool
	// OldText is the content before. It is empty when the file did not
	// exist or OldBinary is set.
	OldText string
	// OldBinary says the content before was not text, so it is not kept.
	OldBinary bool
	NewText   string
}

// Diff returns the change as ACP shows a file modification: the old and the
// new text, which the client diffs itself. A file that was new or was not
// text has no old text.
func (c Change) Diff() acp.ToolCallContent {
	diff := &acp.ToolCallContentDiff{Path: c.Path, NewText: c.NewText, Type: "diff"}
	if c.Existed && !c.OldBinary {
		diff.OldText = &c.OldText
	}
	return acp.ToolCallContent{Diff: diff}
}

// WriteInput is a file and its whole new content.
type WriteInput struct {
	Path    string
	Content string
}

// Write replaces a file's content, or creates the file and the directories
// above it.
func Write(ctx context.Context, roots Roots, cfg config.ToolsFiles, in WriteInput) (Change, error) {
	if err := ctx.Err(); err != nil {
		return Change{}, err
	}
	if size := int64(len(in.Content)); size > cfg.EditMaxBytes {
		return Change{}, &Error{Kind: KindTooLarge, Path: in.Path, Size: size, Limit: cfg.EditMaxBytes}
	}
	loc, err := roots.locate(in.Path)
	if err != nil {
		return Change{}, err
	}
	root, err := loc.open()
	if err != nil {
		return Change{}, err
	}
	defer root.Close() //nolint:errcheck // each write is checked where it happens

	change := Change{Path: loc.abs(), NewText: in.Content}
	mode := fs.FileMode(0o644)
	old, info, err := readWhole(root, loc.rel, in.Path, cfg.EditMaxBytes)
	switch {
	case err == nil:
		change.Existed, mode = true, info.Mode().Perm()
		if utf8.Valid(old) {
			change.OldText = string(old)
		} else {
			change.OldBinary = true
		}
	case isKind(err, KindNotFound):
		if dir := filepath.Dir(loc.rel); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return Change{}, classify(in.Path, err)
			}
		}
	default:
		return Change{}, err
	}
	if err := replaceFile(root, loc.rel, in.Content, mode, change.Existed); err != nil {
		return Change{}, classify(in.Path, err)
	}
	return change, nil
}

// Edit is one exact replacement.
type Edit struct {
	// Old is the text to replace. It must occur exactly once unless All is
	// set.
	Old string
	New string
	// All replaces every occurrence.
	All bool
}

// EditInput is a file and the replacements to make in it, in order.
type EditInput struct {
	Path  string
	Edits []Edit
}

// EditResult is the change and how many replacements each edit made.
type EditResult struct {
	Change
	Replaced []int
}

// ApplyEdits changes a file by exact string replacement. Each edit sees the
// result of the ones before it. When any edit fails nothing is written.
func ApplyEdits(ctx context.Context, roots Roots, cfg config.ToolsFiles, in EditInput) (EditResult, error) {
	if err := ctx.Err(); err != nil {
		return EditResult{}, err
	}
	if len(in.Edits) == 0 {
		return EditResult{}, &Error{Kind: KindNoEdits, Path: in.Path}
	}
	loc, err := roots.locate(in.Path)
	if err != nil {
		return EditResult{}, err
	}
	root, err := loc.open()
	if err != nil {
		return EditResult{}, err
	}
	defer root.Close() //nolint:errcheck // each write is checked where it happens

	old, info, err := readWhole(root, loc.rel, in.Path, cfg.EditMaxBytes)
	if err != nil {
		return EditResult{}, err
	}
	if !utf8.Valid(old) {
		return EditResult{}, &Error{Kind: KindBinary, Path: in.Path}
	}
	text := string(old)
	replaced := make([]int, len(in.Edits))
	for i, edit := range in.Edits {
		switch count := strings.Count(text, edit.Old); {
		case edit.Old == "":
			return EditResult{}, &Error{Kind: KindEmptyOld, Path: in.Path, Edit: i}
		case edit.Old == edit.New:
			return EditResult{}, &Error{Kind: KindSameText, Path: in.Path, Edit: i}
		case count == 0:
			return EditResult{}, &Error{Kind: KindNoMatch, Path: in.Path, Edit: i}
		case count > 1 && !edit.All:
			return EditResult{}, &Error{Kind: KindNotUnique, Path: in.Path, Edit: i, Count: count}
		default:
			text = strings.ReplaceAll(text, edit.Old, edit.New)
			replaced[i] = count
		}
	}
	if text == string(old) {
		return EditResult{}, &Error{Kind: KindNoChange, Path: in.Path}
	}
	if size := int64(len(text)); size > cfg.EditMaxBytes {
		return EditResult{}, &Error{Kind: KindTooLarge, Path: in.Path, Size: size, Limit: cfg.EditMaxBytes}
	}
	if err := replaceFile(root, loc.rel, text, info.Mode().Perm(), true); err != nil {
		return EditResult{}, classify(in.Path, err)
	}
	return EditResult{
		Change:   Change{Path: loc.abs(), Existed: true, OldText: string(old), NewText: text},
		Replaced: replaced,
	}, nil
}

// readWhole reads a regular file of at most limit bytes.
func readWhole(root *os.Root, rel, path string, limit int64) ([]byte, fs.FileInfo, error) {
	info, err := root.Stat(rel)
	if err != nil {
		return nil, nil, classify(path, err)
	}
	if err := checkRegular(path, info); err != nil {
		return nil, nil, err
	}
	if info.Size() > limit {
		return nil, nil, &Error{Kind: KindTooLarge, Path: path, Size: info.Size(), Limit: limit}
	}
	data, err := root.ReadFile(rel)
	if err != nil {
		return nil, nil, classify(path, err)
	}
	return data, info, nil
}

// replaceFile writes content beside the file and renames it into place, so
// a reader sees the old content or the new and never a part of either. The
// rename gives the file a new inode: other hard links keep the old content.
func replaceFile(root *os.Root, rel, content string, mode fs.FileMode, existed bool) (err error) {
	tmp := filepath.Join(filepath.Dir(rel), fmt.Sprintf(".%s.%s.tmp", filepath.Base(rel), rand.Text()))
	file, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, root.Remove(tmp))
		}
	}()
	if _, err := file.WriteString(content); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return err
	}
	if existed {
		// The umask applied to the create. An existing file keeps its mode.
		if err := root.Chmod(tmp, mode); err != nil {
			return err
		}
	}
	return root.Rename(tmp, rel)
}

func isKind(err error, kind Kind) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == kind
}
