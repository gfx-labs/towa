// Package core is the tool set every model gets: read, write, edit,
// apply_patch, bash, glob, grep and todo. Each tool's name, description and
// schema are in specs/, and each runs one capability from a sibling package.
package core

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

// Deps holds session-owned state and client operations. Reads must be kept
// across turns and never shared between sessions.
type Deps struct {
	Reads *ReadTracker
	// ShowPlan sends the checked list to the client. Nil omits todo.
	ShowPlan func(context.Context, tools.Env, []acp.PlanEntry) error
}

//go:embed specs/*.json
var specs embed.FS

// Tools returns the core tools, in the order the model is told about them.
func Tools(env tools.Env, cfg config.Tools, deps Deps) ([]tools.Tool, error) {
	if !filepath.IsAbs(env.Cwd) {
		return nil, fmt.Errorf("core: cwd %q is not absolute", env.Cwd)
	}
	if deps.Reads == nil {
		return nil, fmt.Errorf("core: session read tracker is required")
	}
	build := []struct {
		name string
		tool func(provider.Tool) tools.Tool
	}{
		{"read", func(spec provider.Tool) tools.Tool { return readTool(spec, env, cfg, deps.Reads) }},
		{"write", func(spec provider.Tool) tools.Tool { return writeTool(spec, env, cfg, deps.Reads) }},
		{"edit", func(spec provider.Tool) tools.Tool { return editTool(spec, env, cfg, deps.Reads) }},
		{"apply_patch", func(spec provider.Tool) tools.Tool { return patchTool(spec, env, cfg, deps.Reads) }},
		{"bash", func(spec provider.Tool) tools.Tool { return bashTool(spec, cfg) }},
		{"glob", func(spec provider.Tool) tools.Tool { return globTool(spec, env, cfg) }},
		{"grep", func(spec provider.Tool) tools.Tool { return grepTool(spec, env, cfg) }},
		{"todo", func(spec provider.Tool) tools.Tool { return planTool(spec, deps) }},
	}
	var result []tools.Tool
	for _, b := range build {
		if b.name == "todo" && deps.ShowPlan == nil {
			continue
		}
		spec, guidance, err := definition(b.name)
		if err != nil {
			return nil, err
		}
		t := b.tool(spec)
		t.Guidance = guidance
		result = append(result, t)
	}
	return result, nil
}

func definition(name string) (provider.Tool, []string, error) {
	raw, err := specs.ReadFile("specs/" + name + ".json")
	if err != nil {
		return provider.Tool{}, nil, err
	}
	var spec struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Guidance    []string       `json:"guidance"`
		InputSchema jsontext.Value `json:"input_schema"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return provider.Tool{}, nil, err
	}
	if spec.Name != name || len(spec.InputSchema) == 0 {
		return provider.Tool{}, nil, fmt.Errorf("core: specs/%s.json does not define %s", name, name)
	}
	return provider.Tool{Name: spec.Name, Description: spec.Description, InputSchema: spec.InputSchema}, spec.Guidance, nil
}

func textResult(text string, failed bool) tools.Result {
	outcome := transcript.OutcomeCompleted
	if failed {
		outcome = transcript.OutcomeFailed
	}
	return tools.Result{Outcome: outcome, Parts: []transcript.Part{{Text: text}}}
}
