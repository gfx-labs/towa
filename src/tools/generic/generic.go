// Package generic offers the captured core tool shapes with harness-authored descriptions.
package generic

import (
	"fmt"

	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/claude"
)

// Dialect identifies this tool set.
const Dialect = "generic"

// Deps uses the same session tracker when switching between generic and Claude.
type Deps = claude.Deps

// Tools uses Claude's supported captured input schemas and capability runners.
// Only the top-level descriptions below are authored by this harness.
func Tools(env tools.Env, cfg config.Tools, deps Deps) ([]tools.Tool, error) {
	result, err := claude.Tools(env, cfg, deps)
	if err != nil {
		return nil, err
	}
	for i := range result {
		switch result[i].Spec.Name {
		case "Read":
			result[i].Spec.Description = "Read text lines or a PNG, JPEG, GIF or WebP image from a file. Text lines are numbered. Successful reads authorize edits in this session until the file changes. Large images are normalized to the configured byte limit and 2000-pixel dimensions. PDF pages and notebook reads are unsupported."
		case "Bash":
			result[i].Spec.Description = "Run a shell command in the session working directory and wait for it to finish. timeout is in milliseconds and must be positive and within the configured maximum. Output is limited. Timeouts stop the process group. Background commands and sandbox selection are unsupported. Commands run without an operating-system sandbox."
		case "Edit":
			result[i].Spec.Description = "Replace exact text in a file. Read the file in this session first. The old text must occur once unless replace_all is true. An empty old_string creates a missing or empty file."
		case "Write":
			result[i].Spec.Description = "Create a file or replace its full content. Read an existing file in this session before overwriting it. Successful writes authorize subsequent edits in this session."
		case "Glob":
			result[i].Spec.Description = "Find files matching a glob pattern under a directory. Results are ordered by modification time, newest first. The default directory is the session working directory."
		case "Grep":
			result[i].Spec.Description = "Search file contents with ripgrep regular expressions. Return matching paths by default, or select content or count output. Filter by glob or file type. head_limit defaults to 250 and zero disables that result-count limit. The -o content option is currently unsupported."
		case "TodoWrite":
			result[i].Spec.Description = "Replace the session's task list with the supplied todos. Each item has content, status and an activeForm label. The client displays the list as the session plan."
		default:
			return nil, fmt.Errorf("generic: no neutral description for %q", result[i].Spec.Name)
		}
	}
	return result, nil
}
