package claude

import (
	"context"
	"embed"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"path/filepath"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// Dialect identifies this tool set.
const Dialect = "claude"

// Deps holds session-owned state and client operations. Reads must be retained
// across turns and dialect switches, not shared between sessions.
type Deps struct {
	Reads *ReadTracker
	// ShowPlan sends the checked list to the client. Nil omits TodoWrite.
	ShowPlan func(context.Context, tools.Env, []acp.PlanEntry) error
}

//go:generate go run ./specgen

//go:embed specs/*.json
var specs embed.FS

// Tools returns the supported captured core tools.
func Tools(env tools.Env, cfg config.Tools, deps Deps) ([]tools.Tool, error) {
	if !filepath.IsAbs(env.Cwd) {
		return nil, fmt.Errorf("claude: cwd %q is not absolute", env.Cwd)
	}
	if deps.Reads == nil {
		return nil, fmt.Errorf("claude: session read tracker is required")
	}
	var result []tools.Tool
	for _, name := range []string{"Edit", "Write", "Glob", "Grep", "TodoWrite", "Read", "Bash"} {
		if name == "TodoWrite" && deps.ShowPlan == nil {
			continue
		}
		spec, err := definition(name)
		if err != nil {
			return nil, err
		}
		switch name {
		case "Read":
			result = append(result, readTool(spec, env, cfg, deps.Reads))
		case "Bash":
			result = append(result, bashTool(spec, cfg))
		case "Edit":
			result = append(result, editTool(spec, env, cfg, deps.Reads))
		case "Write":
			result = append(result, writeTool(spec, env, cfg, deps.Reads))
		case "Glob":
			result = append(result, globTool(spec, env, cfg))
		case "Grep":
			result = append(result, grepTool(spec, env, cfg))
		case "TodoWrite":
			result = append(result, planTool(spec, deps))
		}
	}
	return result, nil
}

func definition(name string) (provider.Tool, error) {
	raw, err := specs.ReadFile("specs/" + name + ".json")
	if err != nil {
		return provider.Tool{}, err
	}
	var captured struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema jsontext.Value `json:"input_schema"`
	}
	if err := json.Unmarshal(raw, &captured); err != nil {
		return provider.Tool{}, err
	}
	if captured.Name != name || len(captured.InputSchema) == 0 {
		return provider.Tool{}, fmt.Errorf("claude: invalid capture for %s", name)
	}
	return provider.Tool{Name: captured.Name, Description: captured.Description, InputSchema: captured.InputSchema}, nil
}

func textResult(text string, failed bool) tools.Result {
	outcome := transcript.OutcomeCompleted
	if failed {
		outcome = transcript.OutcomeFailed
	}
	return tools.Result{Outcome: outcome, Parts: []transcript.Part{{Text: text}}}
}
