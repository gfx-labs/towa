package agentctx

import (
	"fmt"
	"strings"

	"github.com/gfx-labs/towa/pkg/acp"
	"github.com/gfx-labs/towa/src/provider"
)

// Names of the built-in sections, in the order they take in the prompt.
// Sections added by configuration follow them, and a system prompt the
// client appended comes last. Every section after the first is sent inside
// a tag of its name.
const (
	// SectionPrompt is the harness's instructions. It opens the prompt and
	// has no tag.
	SectionPrompt = "prompt"
	// SectionTools lists the tools, one line each.
	SectionTools = "tools"
	// SectionRules is the guidance about the tools.
	SectionRules = "rules"
	// SectionProjectContext holds the context files.
	SectionProjectContext = "project_context"
	// SectionSkills lists the skills.
	SectionSkills = "skills"
	// SectionWorkDir names the working directory.
	SectionWorkDir = "working_directory"
	// SectionClient is the system prompt the ACP client sent.
	SectionClient = "client"
)

var builtinSections = []string{
	SectionPrompt, SectionTools, SectionRules, SectionProjectContext, SectionSkills, SectionWorkDir, SectionClient,
}

// ToolLine is one tool in the prompt's list of tools.
type ToolLine struct {
	Name string
	// Summary says in a few words what the tool does.
	Summary string
}

// Prompt is what the system prompt is built from. A section whose text is
// empty is left out.
type Prompt struct {
	// Instructions is the text of the prompt section.
	Instructions string
	// Tools are the tools this request offers, in the tools' order.
	Tools []ToolLine
	// Rules are the guidance lines of those tools, then the lines that
	// hold for every tool.
	Rules []string
	// Loaded is what Load returned. It may be nil.
	Loaded *Loaded
	// WorkDir is the session's working directory.
	WorkDir string
	// AdditionalDirs are the session's other workspace roots.
	AdditionalDirs []string
	// SkillTool is the name of the tool that loads a skill. When it is
	// empty the skills are listed with the path of their SKILL.md only.
	SkillTool string
	// Client is the systemPrompt of session/new, or nil.
	Client *acp.SystemPrompt
}

// UnsupportedModeError reports a systemPrompt mode other than append and
// override.
type UnsupportedModeError struct {
	Mode acp.SystemPromptMode
}

func (e *UnsupportedModeError) Error() string {
	return fmt.Sprintf("agentctx: systemPrompt mode %q is not supported", string(e.Mode))
}

// ContentError reports systemPrompt content that cannot be used: no blocks,
// or a block that is not text.
type ContentError struct {
	// Index is the block's position, or -1 when there are no blocks.
	Index int
	// Kind is the block's type, when it is known.
	Kind string
}

func (e *ContentError) Error() string {
	if e.Index < 0 {
		return "agentctx: systemPrompt content is empty"
	}
	return fmt.Sprintf("agentctx: systemPrompt content[%d] is a %s block, and only text is supported", e.Index, e.Kind)
}

// Capabilities is what the agent advertises under
// sessionCapabilities.systemPrompt: append, which the capability implies,
// and override.
func Capabilities() *acp.SessionSystemPromptCapabilities {
	return &acp.SessionSystemPromptCapabilities{
		Mode: &acp.SessionSystemPromptModeCapabilities{
			Override: &acp.SessionSystemPromptOverrideCapabilities{},
		},
	}
}

// Build returns the system prompt's sections in order: the instructions,
// tools, rules, project context, skills, working directory, the sections
// added by configuration, and the client's appended prompt.
//
// The instructions are plain text. Every section after them is wrapped in
// a tag of its name, as pi's prompt is, so that the model can tell where a
// context file or a skill list ends and the next section begins.
//
// A client prompt in append mode is the last section. In override mode it
// takes the place of the prompt, tools and rules sections, untagged as the
// instructions are, and everything after them stays. Neither mode touches
// the tool definitions, the MCP servers or the permissions, which are not
// part of the prompt.
func Build(p Prompt) ([]provider.SystemBlock, error) {
	client, err := clientText(p.Client)
	if err != nil {
		return nil, err
	}
	override := p.Client != nil && p.Client.Mode == acp.SystemPromptModeOverride

	var blocks []provider.SystemBlock
	opening := func(name, text string) {
		if strings.TrimSpace(text) != "" {
			blocks = append(blocks, provider.SystemBlock{Name: name, Text: text})
		}
	}
	add := func(name, text string) {
		if text = strings.Trim(text, "\n"); strings.TrimSpace(text) != "" {
			blocks = append(blocks, provider.SystemBlock{Name: name, Text: "<" + name + ">\n" + text + "\n</" + name + ">"})
		}
	}

	if override {
		opening(SectionClient, client)
	} else {
		opening(SectionPrompt, p.Instructions)
		add(SectionTools, toolList(p.Tools))
		add(SectionRules, list(p.Rules))
	}
	var loaded Loaded
	if p.Loaded != nil {
		loaded = *p.Loaded
	}
	add(SectionProjectContext, projectContext(loaded.Files))
	add(SectionSkills, skillsText(loaded.Skills, p.SkillTool))
	add(SectionWorkDir, workDirText(p.WorkDir, p.AdditionalDirs))
	for _, section := range loaded.Sections {
		for _, name := range builtinSections {
			if section.Name == name {
				return nil, fmt.Errorf("agentctx: context.sections: %q is the name of a built-in section", name)
			}
		}
		add(section.Name, section.Text)
	}
	if !override {
		add(SectionClient, client)
	}
	return blocks, nil
}

// clientText checks the client's systemPrompt and returns its text.
func clientText(sp *acp.SystemPrompt) (string, error) {
	if sp == nil {
		return "", nil
	}
	switch sp.Mode {
	case acp.SystemPromptModeAppend, acp.SystemPromptModeOverride:
	default:
		return "", &UnsupportedModeError{Mode: sp.Mode}
	}
	if len(sp.Content) == 0 {
		return "", &ContentError{Index: -1}
	}
	parts := make([]string, 0, len(sp.Content))
	for i, block := range sp.Content {
		if block.Text == nil {
			return "", &ContentError{Index: i, Kind: blockKind(block)}
		}
		parts = append(parts, block.Text.Text)
	}
	text := strings.Join(parts, "\n\n")
	if strings.TrimSpace(text) == "" {
		return "", &ContentError{Index: -1}
	}
	return text, nil
}

func blockKind(b acp.ContentBlock) string {
	switch {
	case b.Image != nil:
		return "image"
	case b.Audio != nil:
		return "audio"
	case b.ResourceLink != nil:
		return "resource_link"
	case b.Resource != nil:
		return "resource"
	default:
		return "unknown"
	}
}

// toolList is one list item per tool: its name and what it does.
func toolList(tools []ToolLine) string {
	lines := make([]string, len(tools))
	for i, t := range tools {
		lines[i] = t.Name + ": " + t.Summary
	}
	return list(lines)
}

// list is one list item per line.
func list(lines []string) string {
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("- " + line)
	}
	return b.String()
}

// escapeAttribute keeps a path from ending the attribute it is in.
var escapeAttribute = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")

// projectContext joins the context files, each in a tag that names its
// path.
func projectContext(files []File) string {
	var b strings.Builder
	for _, f := range files {
		if strings.TrimSpace(f.Text) == "" {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("Instructions from the context files:")
		}
		fmt.Fprintf(&b, "\n\n<project_instructions path=\"%s\">\n%s\n</project_instructions>", escapeAttribute.Replace(f.Path), strings.Trim(f.Text, "\n"))
	}
	return b.String()
}

func skillsText(skills []Skill, tool string) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	if tool != "" {
		fmt.Fprintf(&b, "Skills hold instructions for particular kinds of task. When a task matches a skill's description, load the skill with the %s tool before starting.\n", tool)
	} else {
		b.WriteString("Skills hold instructions for particular kinds of task. When a task matches a skill's description, read the skill's file before starting.\n")
	}
	for _, s := range skills {
		// A description may span lines in its front matter. One line
		// per skill keeps the list readable.
		fmt.Fprintf(&b, "\n- %s: %s (%s)", s.Name, strings.Join(strings.Fields(s.Description), " "), s.Path)
	}
	return b.String()
}

func workDirText(workDir string, additional []string) string {
	if workDir == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Working directory: %s", workDir)
	for _, dir := range additional {
		fmt.Fprintf(&b, "\nAdditional directory: %s", dir)
	}
	return b.String()
}
