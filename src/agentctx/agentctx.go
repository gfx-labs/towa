// Package agentctx loads what a session's system prompt is built from
// (instruction files, skills, sections added by configuration) and builds
// the prompt from named sections in a fixed order.
package agentctx

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/provider"
)

// DialectClaude is the dialect in which CLAUDE.md is read beside AGENTS.md.
// It is the value of tools.Set.Dialect for Claude models.
const DialectClaude = "claude"

// Names of the instruction files.
const (
	agentsFile = "AGENTS.md"
	claudeFile = "CLAUDE.md"
)

// Request says what to load for one session.
type Request struct {
	Config config.Context
	// WorkDir is the session's working directory. It must be absolute.
	WorkDir string
	// HomeDir replaces a leading "~" in a configured path. It may be empty
	// when no configured path starts with "~".
	HomeDir string
	// Dialect is the tool dialect of the session's model.
	Dialect string
}

// Loaded is what Load read.
type Loaded struct {
	// Files are the context files in the order they go into the prompt.
	Files []File
	// Skills are the skills found, in the order they are listed.
	Skills []Skill
	// Sections are the sections added by configuration, in config order.
	Sections []provider.SystemBlock
}

// File is one context file.
type File struct {
	// Path is the absolute path the file was read from.
	Path string
	Text string
}

// LimitError reports a file, or the context files together, larger than a
// configured limit.
type LimitError struct {
	// Path is the file that crossed the limit.
	Path string
	// Key is the config key of the limit.
	Key string
	// Size is the size in bytes that was measured against Limit.
	Size  int64
	Limit int64
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("agentctx: %s: %d bytes is over %s = %d", e.Path, e.Size, e.Key, e.Limit)
}

// Load reads the context files, lists the skills and resolves the
// configured sections.
func Load(req Request) (*Loaded, error) {
	files, err := LoadFiles(req)
	if err != nil {
		return nil, err
	}
	skills, err := ListSkills(req)
	if err != nil {
		return nil, err
	}
	sections, err := loadSections(req)
	if err != nil {
		return nil, err
	}
	return &Loaded{Files: files, Skills: skills, Sections: sections}, nil
}

// LoadFiles reads the context files in this order, which is the order they
// take in the prompt:
//
//  1. the global directory's AGENTS.md, then its CLAUDE.md;
//  2. each directory from the repository root down to the working
//     directory, AGENTS.md then CLAUDE.md, so that the file nearest the
//     working directory comes last;
//  3. the configured extra files, in config order;
//  4. the .md files under each configured extra directory, in path order.
//
// CLAUDE.md is read only in the claude dialect. Steps 1 and 2 are skipped
// when context.agents_files is off. The repository root is the nearest
// directory, from the working directory upwards, that holds a ".git" entry.
// When there is none, only the working directory is searched.
//
// An AGENTS.md or CLAUDE.md that does not exist is not an error. Any other
// failure is, including an extra file or directory that does not exist. A
// file reached twice, such as a CLAUDE.md that is a symlink to AGENTS.md, is
// read once, at its first position.
func LoadFiles(req Request) ([]File, error) {
	workDir, err := checkWorkDir(req.WorkDir)
	if err != nil {
		return nil, err
	}
	l := &fileLoader{cfg: req.Config, seen: map[string]bool{}}

	if req.Config.AgentsFiles {
		names := []string{agentsFile}
		if req.Dialect == DialectClaude {
			names = append(names, claudeFile)
		}
		var dirs []string
		if req.Config.GlobalDir != "" {
			global, err := resolvePath(req.Config.GlobalDir, workDir, req.HomeDir)
			if err != nil {
				return nil, fmt.Errorf("agentctx: context.global_dir: %w", err)
			}
			dirs = append(dirs, global)
		}
		chain, err := dirsFromRepoRoot(workDir)
		if err != nil {
			return nil, err
		}
		for _, dir := range append(dirs, chain...) {
			for _, name := range names {
				if err := l.add(filepath.Join(dir, name), true); err != nil {
					return nil, err
				}
			}
		}
	}

	for _, extra := range req.Config.ExtraFiles {
		path, err := resolvePath(extra, workDir, req.HomeDir)
		if err != nil {
			return nil, fmt.Errorf("agentctx: context.extra_files: %w", err)
		}
		if err := l.add(path, false); err != nil {
			return nil, err
		}
	}
	for _, extra := range req.Config.ExtraDirs {
		dir, err := resolvePath(extra, workDir, req.HomeDir)
		if err != nil {
			return nil, fmt.Errorf("agentctx: context.extra_dirs: %w", err)
		}
		// WalkDir visits in lexical order and does not follow symlinks
		// below dir, so a link cannot make the walk loop.
		var paths []string
		err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && filepath.Ext(path) == ".md" {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("agentctx: %w", pathError(dir, err))
		}
		for _, path := range paths {
			if err := l.add(path, false); err != nil {
				return nil, err
			}
		}
	}
	return l.files, nil
}

type fileLoader struct {
	cfg   config.Context
	files []File
	// seen holds the resolved path of every file read, so that a file
	// reached twice is read once.
	seen  map[string]bool
	total int64
}

// add reads one context file. With optional set, a file that does not exist
// is left out.
func (l *fileLoader) add(path string, optional bool) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if optional && errors.Is(err, fs.ErrNotExist) {
			// A dangling symlink also reports ErrNotExist. That is a
			// file that was meant to be there, so it is an error.
			if _, lerr := os.Lstat(path); lerr != nil && errors.Is(lerr, fs.ErrNotExist) {
				return nil
			}
		}
		return fmt.Errorf("agentctx: %w", pathError(path, err))
	}
	if l.seen[resolved] {
		return nil
	}
	text, err := readText(path, l.cfg.MaxFileBytes)
	if err != nil {
		return err
	}
	l.seen[resolved] = true
	l.total += int64(len(text))
	if limit := l.cfg.MaxTotalBytes; limit > 0 && l.total > limit {
		return &LimitError{Path: path, Key: "context.max_total_bytes", Size: l.total, Limit: limit}
	}
	l.files = append(l.files, File{Path: path, Text: text})
	return nil
}

// pathError makes sure the error names the path the caller asked for.
func pathError(path string, err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) && pe.Path == path {
		return err
	}
	return &fs.PathError{Op: "read", Path: path, Err: err}
}

// readText reads a whole file as UTF-8 text. limit is the largest size
// accepted, and zero means no limit.
func readText(path string, limit int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("agentctx: %w", err)
	}
	defer f.Close() //nolint:errcheck // read only, nothing to flush
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("agentctx: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("agentctx: %s: not a regular file", path)
	}
	// Checked before reading so that an oversized file is not read at all.
	if limit > 0 && info.Size() > limit {
		return "", &LimitError{Path: path, Key: "context.max_file_bytes", Size: info.Size(), Limit: limit}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("agentctx: %w", pathError(path, err))
	}
	// The file may have grown since Stat.
	if limit > 0 && int64(len(data)) > limit {
		return "", &LimitError{Path: path, Key: "context.max_file_bytes", Size: int64(len(data)), Limit: limit}
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("agentctx: %s: not valid UTF-8", path)
	}
	return string(data), nil
}

func checkWorkDir(workDir string) (string, error) {
	if !filepath.IsAbs(workDir) {
		return "", fmt.Errorf("agentctx: working directory %q is not an absolute path", workDir)
	}
	info, err := os.Stat(workDir)
	if err != nil {
		return "", fmt.Errorf("agentctx: working directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("agentctx: working directory %s is not a directory", workDir)
	}
	return filepath.Clean(workDir), nil
}

// dirsFromRepoRoot returns the directories from the repository root down to
// workDir, or workDir alone when it is in no repository.
func dirsFromRepoRoot(workDir string) ([]string, error) {
	var up []string
	for dir := workDir; ; {
		up = append(up, dir)
		switch _, err := os.Lstat(filepath.Join(dir, ".git")); {
		case err == nil:
			down := make([]string, 0, len(up))
			for i := len(up) - 1; i >= 0; i-- {
				down = append(down, up[i])
			}
			return down, nil
		case !errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("agentctx: %w", err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return []string{workDir}, nil
		}
		dir = parent
	}
}

// resolvePath turns a configured path into an absolute one.
func resolvePath(path, workDir, homeDir string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if homeDir == "" {
			return "", fmt.Errorf("%q needs a home directory and none is known", path)
		}
		return filepath.Join(homeDir, strings.TrimPrefix(path, "~")), nil
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	return filepath.Join(workDir, path), nil
}

// loadSections resolves the sections added by configuration.
func loadSections(req Request) ([]provider.SystemBlock, error) {
	var out []provider.SystemBlock
	for _, s := range req.Config.Sections {
		text := s.Text
		if s.File != "" {
			path, err := resolvePath(s.File, req.WorkDir, req.HomeDir)
			if err != nil {
				return nil, fmt.Errorf("agentctx: context.sections %q: %w", s.Name, err)
			}
			if text, err = readText(path, req.Config.MaxFileBytes); err != nil {
				return nil, err
			}
		}
		out = append(out, provider.SystemBlock{Name: s.Name, Text: text})
	}
	return out, nil
}
