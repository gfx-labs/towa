package config

// Context configures what is loaded into a session's system prompt: agent
// instruction files, skills, configured sections and the prompt itself.
// src/prompt/agentctx reads it. A path may start with "~/", which stands for the
// home directory. Any other relative path is taken from the session's
// working directory.
//
// The two size limits default to none: no size has been measured to be the
// right one, and a file over a limit fails the session.
type Context struct {
	// DefaultAgents turns on the default instruction files: ~/AGENTS.md and
	// the .md files under ~/.agents/rules, which are global instructions,
	// and AGENTS.md in each directory from the repository root down to the
	// working directory, which are the project's. A default file that does
	// not exist is left out.
	DefaultAgents bool `name:"default_agents" default:"true" help:"Read ~/AGENTS.md, ~/.agents/rules/*.md and AGENTS.md from the repository root down to the working directory. On by default."`
	// Agents are global instruction files and directories, read after
	// ~/.agents/rules in this order. A directory contributes every .md file
	// under it, in path order. Each must exist.
	Agents []string `name:"agents" placeholder:"PATH" validate:"dive,required" help:"Global instruction files or directories of .md files, read after ~/.agents/rules in this order. Each must exist."`
	// AgentsText are global instructions given inline, read after Agents in
	// this order. A flag value is one whole entry, so text from a command
	// such as --context.agents_text="$(cmd)" keeps its commas and newlines.
	AgentsText []string `name:"agents_text" sep:"none" placeholder:"TEXT" validate:"dive,required" help:"Global instruction text read after agents, in this order (repeatable). Each flag value is one entry."`
	// SkillsEnabled exposes the skill-loading extra to the model.
	SkillsEnabled bool `name:"skills_enabled" default:"true" help:"Give the model the tool that loads a skill. On by default."`
	// DefaultSkills turns on ~/.agents/skills, searched before SkillsDirs.
	// It holds no skills when it does not exist.
	DefaultSkills bool `name:"default_skills" default:"true" help:"Search ~/.agents/skills before skills_dirs. On by default."`
	// SkillsDirs are searched after the default directory for directories
	// that hold a SKILL.md. When two skills have the same name, the one from
	// the earlier directory is used. Each must exist.
	SkillsDirs []string `name:"skills_dirs" placeholder:"DIR" validate:"dive,required" help:"Directories searched after the default one for directories that hold a SKILL.md. The earlier directory wins a name. Each must exist."`
	// SystemPromptFile replaces the built-in instructions with the file's
	// text. Empty means the built-in instructions.
	SystemPromptFile string `name:"system_prompt_file" placeholder:"FILE" help:"File whose text replaces the built-in instructions section of the system prompt."`
	// MaxFileBytes is the largest context file or SKILL.md that is
	// accepted. A larger one is an error, not a truncated file. Zero means
	// no limit.
	MaxFileBytes int64 `name:"max_file_bytes" default:"0" validate:"gte=0" help:"Largest context file or SKILL.md that is accepted. A larger one is an error. 0 is no limit."`
	// MaxTotalBytes is the largest sum of the context files' sizes that is
	// accepted. Zero means no limit.
	MaxTotalBytes int64 `name:"max_total_bytes" default:"0" validate:"gte=0" help:"Largest sum of the context files' sizes that is accepted. 0 is no limit."`
	// Sections are added to the end of the system prompt, in this order.
	Sections []ContextSection `name:"sections" type:"table" placeholder:"YAML" validate:"unique=Name,dive" help:"Sections added to the end of the system prompt: name, then text or file. The name is the tag the text is sent in, so a lower-case word. Written in a config file."`
}

// ContextSection is one system prompt section added by configuration. It
// has either Text or File.
type ContextSection struct {
	// Name identifies the section and is the tag its text is sent in, so
	// it is a lower-case word. It must differ from every other section's
	// name, including the built-in ones.
	Name string `json:"name" validate:"required,section_name"`
	Text string `json:"text" validate:"required_without=File,excluded_with=File"`
	// File is the path of a file whose content is the section's text.
	File string `json:"file"`
}

func (c Context) validate() error {
	return validateFields("context", c)
}
