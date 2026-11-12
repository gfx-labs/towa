package agentctx

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-yaml"
)

// skillFile is the file that makes a directory a skill.
const skillFile = "SKILL.md"

// Skill is one skill as the prompt lists it.
type Skill struct {
	Name        string
	Description string
	// Dir is the skill's directory. Relative paths in the skill's text are
	// relative to it.
	Dir string
	// Path is the skill's SKILL.md.
	Path string
}

// SkillContent is a skill loaded in full.
type SkillContent struct {
	Skill
	// Body is the text of SKILL.md after its front matter.
	Body string
}

// UnknownSkillError reports a skill name that no listed skill has.
type UnknownSkillError struct {
	Name string
}

func (e *UnknownSkillError) Error() string {
	return fmt.Sprintf("agentctx: no skill is named %q", e.Name)
}

// SkillCache keeps what ListSkills read from each SKILL.md, so that the
// sessions of one process share one copy and a file is parsed once. Every
// listing still walks the directories and stats each SKILL.md: a file whose
// modification time or size has changed is read again, and a file that is
// gone is not listed. An edit that keeps both is not seen. A file that
// fails is not kept, so its error is reported each time.
//
// An entry stays for the life of the process, one per SKILL.md path seen.
type SkillCache struct {
	mu    sync.Mutex
	files map[string]cachedSkill
}

type cachedSkill struct {
	modTime time.Time
	size    int64
	// limit is the file size limit the file was read under.
	limit int64
	skill Skill
}

func (c *SkillCache) read(dir, path string, info fs.FileInfo, limit int64) (Skill, error) {
	if c == nil {
		return readSkill(dir, path, limit)
	}
	c.mu.Lock()
	cached, ok := c.files[path]
	c.mu.Unlock()
	if ok && cached.modTime.Equal(info.ModTime()) && cached.size == info.Size() && cached.limit == limit && cached.skill.Dir == dir {
		return cached.skill, nil
	}
	skill, err := readSkill(dir, path, limit)
	if err != nil {
		return Skill{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.files == nil {
		c.files = map[string]cachedSkill{}
	}
	c.files[path] = cachedSkill{modTime: info.ModTime(), size: info.Size(), limit: limit, skill: skill}
	return skill, nil
}

// ListSkills finds the skills under the configured skills directories. A
// skill is a directory that holds a SKILL.md with a name and a description
// in its front matter. The search goes into subdirectories, follows
// symlinks, and does not go into a skill's own directory or a directory
// whose name starts with a dot.
//
// Skills are listed directory by directory in config order, and by path
// within a directory. A name found again in a later directory is left out:
// the earlier directory wins. The same name twice within one directory is
// an error. A skills directory that does not exist holds no skills, since
// the default directory is absent on most machines. A SKILL.md that cannot
// be read or has no usable front matter is an error.
func ListSkills(req Request) ([]Skill, error) {
	var out []Skill
	byName := map[string]Skill{}
	for _, configured := range req.Config.SkillsDirs {
		root, err := resolvePath(configured, req.WorkDir, req.HomeDir)
		if err != nil {
			return nil, fmt.Errorf("agentctx: context.skills_dirs: %w", err)
		}
		w := &skillWalker{limit: req.Config.MaxFileBytes, cache: req.Skills, visited: map[string]bool{}}
		if err := w.walk(root, true); err != nil {
			return nil, err
		}
		inThisRoot := map[string]string{}
		for _, skill := range w.found {
			if other, ok := inThisRoot[skill.Name]; ok {
				return nil, fmt.Errorf("agentctx: skill %q is defined by both %s and %s", skill.Name, other, skill.Path)
			}
			inThisRoot[skill.Name] = skill.Path
			if _, ok := byName[skill.Name]; ok {
				continue
			}
			byName[skill.Name] = skill
			out = append(out, skill)
		}
	}
	return out, nil
}

// LoadSkill reads the named skill in full. It is the function a skill tool
// calls. maxBytes is context.max_file_bytes, and zero means no limit.
func LoadSkill(skills []Skill, name string, maxBytes int64) (*SkillContent, error) {
	for _, skill := range skills {
		if skill.Name != name {
			continue
		}
		text, err := readText(skill.Path, maxBytes)
		if err != nil {
			return nil, err
		}
		// The file is read again because it may have changed since it
		// was listed. The listed name and description are kept.
		_, body, err := splitFrontMatter(skill.Path, text)
		if err != nil {
			return nil, err
		}
		return &SkillContent{Skill: skill, Body: body}, nil
	}
	return nil, &UnknownSkillError{Name: name}
}

type skillWalker struct {
	limit int64
	cache *SkillCache
	found []Skill
	// visited holds resolved directories, so that a symlink cycle ends.
	visited map[string]bool
}

func (w *skillWalker) walk(dir string, root bool) error {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		if root && errors.Is(err, fs.ErrNotExist) {
			if _, lerr := os.Lstat(dir); lerr != nil && errors.Is(lerr, fs.ErrNotExist) {
				return nil
			}
		}
		return fmt.Errorf("agentctx: %w", pathError(dir, err))
	}
	if w.visited[resolved] {
		return nil
	}
	w.visited[resolved] = true

	path := filepath.Join(dir, skillFile)
	switch info, err := os.Stat(path); {
	case err == nil:
		skill, err := w.cache.read(dir, path, info, w.limit)
		if err != nil {
			return err
		}
		w.found = append(w.found, skill)
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("agentctx: %w", err)
	}

	// ReadDir returns the entries sorted by name.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("agentctx: %w", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		child := filepath.Join(dir, entry.Name())
		// Stat follows a symlink: skills are often linked into place.
		info, err := os.Stat(child)
		if err != nil {
			return fmt.Errorf("agentctx: %w", err)
		}
		if !info.IsDir() {
			continue
		}
		if err := w.walk(child, false); err != nil {
			return err
		}
	}
	return nil
}

func readSkill(dir, path string, limit int64) (Skill, error) {
	text, err := readText(path, limit)
	if err != nil {
		return Skill{}, err
	}
	front, _, err := splitFrontMatter(path, text)
	if err != nil {
		return Skill{}, err
	}
	var meta struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(front), &meta); err != nil {
		// The library's own text repeats the source over several lines.
		return Skill{}, fmt.Errorf("agentctx: %s: front matter: %s", path, yaml.FormatError(err, false, false))
	}
	name, description := strings.TrimSpace(meta.Name), strings.TrimSpace(meta.Description)
	if name == "" {
		return Skill{}, fmt.Errorf("agentctx: %s: front matter has no name", path)
	}
	if description == "" {
		return Skill{}, fmt.Errorf("agentctx: %s: front matter has no description", path)
	}
	return Skill{Name: name, Description: description, Dir: dir, Path: path}, nil
}

// splitFrontMatter splits a SKILL.md into its YAML front matter and the
// text after it. The front matter is between a first line of "---" and the
// next line of "---".
func splitFrontMatter(path, text string) (front, body string, err error) {
	text = strings.TrimPrefix(text, "\ufeff")
	first, rest, _ := strings.Cut(text, "\n")
	if strings.TrimRight(first, "\r") != "---" {
		return "", "", fmt.Errorf("agentctx: %s: does not start with front matter", path)
	}
	for remaining := rest; ; {
		line, after, more := strings.Cut(remaining, "\n")
		if strings.TrimRight(line, "\r") == "---" {
			front = rest[:len(rest)-len(remaining)]
			return front, strings.TrimLeft(after, "\r\n"), nil
		}
		if !more {
			return "", "", fmt.Errorf("agentctx: %s: front matter is not closed", path)
		}
		remaining = after
	}
}
