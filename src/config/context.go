package config

// Context configures what is loaded into a session's system prompt: agent
// instruction files, skills, extra files and directories. src/agentctx reads
// it. A path may start with "~/", which stands for the home directory. Any
// other relative path is taken from the session's working directory.
//
// The two size limits default to none: no size has been measured to be the
// right one, and a file over a limit fails the session.
type Context struct {
	// AgentsFiles turns on reading AGENTS.md from the global directory and
	// from the working directory up to the repository root.
	AgentsFiles bool `name:"agents_files" default:"true" help:"Read AGENTS.md from the global directory and from the working directory up to the repository root. On by default."`
	// GlobalDir is the directory that holds the global AGENTS.md. Empty
	// means there is no global file.
	GlobalDir string `name:"global_dir" default:"~/.agents" placeholder:"DIR" help:"Directory that holds the global AGENTS.md. Empty means there is no global file."`
	// RulesDirs are read after the instruction files. Every .md file under
	// each directory is read, in path order. A directory that does not
	// exist holds no rules.
	RulesDirs []string `name:"rules_dirs" default:"~/.agents/rules" placeholder:"DIR" validate:"dive,required" help:"Directories whose .md files are read after the instruction files, in path order. One that does not exist is skipped."`
	// ExtraFiles are read after the rules, in this order. Each
	// must exist.
	ExtraFiles []string `name:"extra_files" placeholder:"FILE" validate:"dive,required" help:"Files read after the rules, in this order. Each must exist."`
	// ExtraDirs are read after ExtraFiles. Every .md file under each
	// directory is read, in path order. Each directory must exist.
	ExtraDirs []string `name:"extra_dirs" placeholder:"DIR" validate:"dive,required" help:"Directories whose .md files are read after extra_files, in path order. Each must exist."`
	// SkillsEnabled exposes the skill-loading extra to the model.
	SkillsEnabled bool `name:"skills_enabled" default:"true" help:"Give the model the tool that loads a skill. On by default."`
	// SkillsDirs are searched for directories that hold a SKILL.md. When
	// two skills have the same name, the one from the earlier directory is
	// used.
	SkillsDirs []string `name:"skills_dirs" default:"~/.agents/skills" placeholder:"DIR" validate:"dive,required" help:"Directories searched for directories that hold a SKILL.md. The earlier directory wins a name."`
	// MaxFileBytes is the largest context file or SKILL.md that is
	// accepted. A larger one is an error, not a truncated file. Zero means
	// no limit.
	MaxFileBytes int64 `name:"max_file_bytes" default:"0" validate:"gte=0" help:"Largest context file or SKILL.md that is accepted. A larger one is an error. 0 is no limit."`
	// MaxTotalBytes is the largest sum of the context files' sizes that is
	// accepted. Zero means no limit.
	MaxTotalBytes int64 `name:"max_total_bytes" default:"0" validate:"gte=0" help:"Largest sum of the context files' sizes that is accepted. 0 is no limit."`
	// Sections are added to the end of the system prompt, in this order.
	Sections []ContextSection `name:"sections" type:"table" placeholder:"YAML" validate:"unique=Name,dive" help:"Sections added to the end of the system prompt: name, then text or file. Written in a config file."`
}

// ContextSection is one system prompt section added by configuration. It
// has either Text or File.
type ContextSection struct {
	// Name identifies the section. It must differ from every other
	// section's name, including the built-in ones.
	Name string `json:"name" validate:"required"`
	Text string `json:"text" validate:"required_without=File,excluded_with=File"`
	// File is the path of a file whose content is the section's text.
	File string `json:"file"`
}

func (c Context) validate() error {
	return validateFields("context", c)
}
