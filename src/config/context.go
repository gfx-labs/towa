package config

import "fmt"

// Context configures what is loaded into a session's system prompt: agent
// instruction files, skills, extra files and directories. src/agentctx reads
// it. A path may start with "~/", which stands for the home directory. Any
// other relative path is taken from the session's working directory.
type Context struct {
	// AgentsFiles turns on reading AGENTS.md, and CLAUDE.md in the claude
	// dialect, from the global directory and from the working directory up
	// to the repository root.
	AgentsFiles bool `koanf:"agents_files"`
	// GlobalDir is the directory that holds the global AGENTS.md and
	// CLAUDE.md. Empty means there is no global file.
	GlobalDir string `koanf:"global_dir"`
	// ExtraFiles are read after the instruction files, in this order. Each
	// must exist.
	ExtraFiles []string `koanf:"extra_files"`
	// ExtraDirs are read after ExtraFiles. Every .md file under each
	// directory is read, in path order. Each directory must exist.
	ExtraDirs []string `koanf:"extra_dirs"`
	// SkillsEnabled exposes the skill-loading extra to the model.
	SkillsEnabled bool `koanf:"skills_enabled"`
	// SkillsDirs are searched for directories that hold a SKILL.md. When
	// two skills have the same name, the one from the earlier directory is
	// used.
	SkillsDirs []string `koanf:"skills_dirs"`
	// MaxFileBytes is the largest context file or SKILL.md that is
	// accepted. A larger one is an error, not a truncated file. Zero means
	// no limit.
	MaxFileBytes int64 `koanf:"max_file_bytes"`
	// MaxTotalBytes is the largest sum of the context files' sizes that is
	// accepted. Zero means no limit.
	MaxTotalBytes int64 `koanf:"max_total_bytes"`
	// Sections are added to the end of the system prompt, in this order.
	Sections []ContextSection `koanf:"sections"`
}

// ContextSection is one system prompt section added by configuration. It
// has either Text or File.
type ContextSection struct {
	// Name identifies the section. It must differ from every other
	// section's name, including the built-in ones.
	Name string `koanf:"name"`
	Text string `koanf:"text"`
	// File is the path of a file whose content is the section's text.
	File string `koanf:"file"`
}

// The limits default to none: no size has been measured to be the right
// one, and a file over a limit fails the session.
var contextDefaults = map[string]any{
	"context.agents_files":    true,
	"context.global_dir":      "~/.agents",
	"context.skills_enabled":  true,
	"context.skills_dirs":     []string{"~/.agents/skills"},
	"context.max_file_bytes":  0,
	"context.max_total_bytes": 0,
}

func (c Context) validate() error {
	if c.MaxFileBytes < 0 {
		return fmt.Errorf("context.max_file_bytes: %d is negative", c.MaxFileBytes)
	}
	if c.MaxTotalBytes < 0 {
		return fmt.Errorf("context.max_total_bytes: %d is negative", c.MaxTotalBytes)
	}
	for key, paths := range map[string][]string{
		"context.extra_files": c.ExtraFiles,
		"context.extra_dirs":  c.ExtraDirs,
		"context.skills_dirs": c.SkillsDirs,
	} {
		for i, path := range paths {
			if path == "" {
				return fmt.Errorf("%s[%d]: the path is empty", key, i)
			}
		}
	}
	seen := map[string]bool{}
	for i, s := range c.Sections {
		switch {
		case s.Name == "":
			return fmt.Errorf("context.sections[%d].name: the name is empty", i)
		case seen[s.Name]:
			return fmt.Errorf("context.sections[%d].name: %q is used twice", i, s.Name)
		case (s.Text == "") == (s.File == ""):
			return fmt.Errorf("context.sections[%d] (%q): set exactly one of text and file", i, s.Name)
		}
		seen[s.Name] = true
	}
	return nil
}
