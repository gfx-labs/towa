package patch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/tools/fsops"
)

// Applied is a committed filesystem change and its patch operation.
type Applied struct {
	Kind   Kind
	Path   string
	Change fsops.Change
}

// Result keeps the committed prefix even when Apply returns an error.
type Result struct{ Changes []Applied }

// Summary uses codex-rs/apply-patch/src/lib.rs:825's A/M/D ordering.
func (r Result) Summary() string {
	var out strings.Builder
	out.WriteString("Success. Updated the following files:\n")
	for _, kind := range []Kind{Add, Update, Delete} {
		for _, change := range r.Changes {
			if change.Kind != kind {
				continue
			}
			letter := map[Kind]string{Add: "A", Update: "M", Delete: "D"}[kind]
			fmt.Fprintf(&out, "%s %s\n", letter, change.Path)
		}
	}
	return out.String()
}

// Apply commits hunks in order without rollback, as upstream lib.rs:438 does.
// A move writes its destination before removing its source.
//
//nolint:revive,staticcheck // Upstream's model-visible errors start with capitals.
func Apply(ctx context.Context, roots fsops.Roots, cfg config.ToolsFiles, p Patch) (Result, error) {
	var result Result
	if p.EnvironmentID != "" {
		return result, errors.New("apply_patch environment selection is unavailable for this turn")
	}
	if len(p.Hunks) == 0 {
		return result, errors.New("No files were modified.")
	}
	for _, h := range p.Hunks {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		abs := h.Path
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(roots.Cwd, abs)
		}
		switch h.Kind {
		case Add:
			change, err := fsops.Write(ctx, roots, cfg, fsops.WriteInput{Path: h.Path, Content: h.Content})
			if err != nil {
				return result, fmt.Errorf("Failed to write file %s: %w", abs, err)
			}
			result.Changes = append(result.Changes, Applied{Add, h.Path, change})
		case Delete:
			change, err := fsops.Remove(ctx, roots, cfg, h.Path)
			if err != nil {
				return result, fmt.Errorf("Failed to delete file %s: %w", abs, err)
			}
			result.Changes = append(result.Changes, Applied{Delete, h.Path, change})
		case Update:
			_, text, err := fsops.ReadText(ctx, roots, cfg, h.Path)
			if err != nil {
				return result, fmt.Errorf("Failed to read file to update %s: %w", abs, err)
			}
			text, err = Derive(abs, text, h.Chunks)
			if err != nil {
				return result, err
			}
			dest := h.Path
			if h.Move != "" {
				dest = h.Move
			}
			change, err := fsops.Write(ctx, roots, cfg, fsops.WriteInput{Path: dest, Content: text})
			if err != nil {
				return result, fmt.Errorf("Failed to write file %s: %w", dest, err)
			}
			move := h.Move != ""
			if move {
				move, err = movedAway(roots, h.Path, change.Path)
				if err != nil {
					result.Changes = append(result.Changes, Applied{Add, dest, change})
					return result, fmt.Errorf("Failed to compare original %s with %s, so it was kept: %w", abs, dest, err)
				}
			}
			if move {
				// Record the destination before Remove, which can fail independently.
				result.Changes = append(result.Changes, Applied{Add, dest, change})
				removed, err := fsops.Remove(ctx, roots, cfg, h.Path)
				if err != nil {
					return result, fmt.Errorf("Failed to remove original %s: %w", abs, err)
				}
				result.Changes[len(result.Changes)-1].Kind = Update
				// The source removal is a diff, not a second summary entry.
				result.Changes = append(result.Changes, Applied{"", h.Path, removed})
			} else {
				result.Changes = append(result.Changes, Applied{Update, dest, change})
			}
		default:
			return result, fmt.Errorf("unknown patch operation %q", h.Kind)
		}
	}
	return result, nil
}

// movedAway reports whether the source of a move is still a file apart from
// the destination just written. The write put a new inode in place of the
// destination, so the source names that inode when the destination was the
// source under another spelling: a symbolic link, or a case alias on a file
// system that ignores case. A hard link to the source keeps the old inode
// and is a separate name.
func movedAway(roots fsops.Roots, source, written string) (bool, error) {
	from, err := roots.Resolve(source)
	if err != nil {
		return false, err
	}
	fromInfo, err := os.Stat(from)
	if err != nil {
		return false, err
	}
	toInfo, err := os.Stat(written)
	if err != nil {
		return false, err
	}
	return !os.SameFile(fromInfo, toInfo), nil
}
