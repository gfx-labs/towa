// Package core is the tool set every model gets: read, write, edit,
// apply_patch, bash, bash_output, bash_stop and todo. Each tool's name, description and schema are
// in specs/, and each runs one capability from a sibling package. There is
// no tool for finding files or searching them: the model runs rg, find and
// ls through bash.
package core

import (
	"context"
	"embed"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	acp "github.com/gfx-labs/go-acp"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/jobs"
)

// Deps holds session-owned state and client operations. Reads must be kept
// across turns and never shared between sessions.
type Deps struct {
	Reads *ReadTracker
	// Jobs are the session's background commands. Like Reads, they are
	// kept across turns and never shared between sessions.
	Jobs *jobs.Set
	// ShowPlan sends the checked list to the client. Nil omits todo, as
	// does tools.todo.enabled when it is off.
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
	if deps.Jobs == nil {
		return nil, fmt.Errorf("core: session job set is required")
	}
	build := []struct {
		name string
		tool func(provider.Tool) tools.Tool
	}{
		{"read", func(spec provider.Tool) tools.Tool { return readTool(spec, env, cfg, deps.Reads) }},
		{"write", func(spec provider.Tool) tools.Tool { return writeTool(spec, env, cfg, deps.Reads) }},
		{"edit", func(spec provider.Tool) tools.Tool { return editTool(spec, env, cfg, deps.Reads) }},
		{"apply_patch", func(spec provider.Tool) tools.Tool { return patchTool(spec, env, cfg, deps.Reads) }},
		{"bash", func(spec provider.Tool) tools.Tool { return bashTool(spec, cfg, deps.Jobs) }},
		{"bash_output", func(spec provider.Tool) tools.Tool { return jobOutputTool(spec, cfg, deps.Jobs) }},
		{"bash_stop", func(spec provider.Tool) tools.Tool { return jobStopTool(spec, deps.Jobs) }},
		{"todo", func(spec provider.Tool) tools.Tool { return planTool(spec, deps) }},
	}
	var result []tools.Tool
	for _, b := range build {
		if b.name == "todo" && (deps.ShowPlan == nil || !cfg.Todo.Enabled) {
			continue
		}
		def := definition(b.name)
		if def.err != nil {
			return nil, def.err
		}
		spec, err := fill(def.spec, limits(cfg))
		if err != nil {
			return nil, err
		}
		t := b.tool(spec)
		t.Summary, t.Guidance = def.summary, def.guidance
		result = append(result, t)
	}
	return result, nil
}

type parsedSpec struct {
	spec     provider.Tool
	summary  string
	guidance []string
	err      error
}

// parsed holds each spec file as it was decoded, by tool name. The files
// are embedded and the same for every session, so they are decoded once.
var parsed sync.Map

func definition(name string) parsedSpec {
	if p, ok := parsed.Load(name); ok {
		return p.(parsedSpec)
	}
	p := decodeSpec(name)
	parsed.Store(name, p)
	return p
}

func decodeSpec(name string) parsedSpec {
	raw, err := specs.ReadFile("specs/" + name + ".json")
	if err != nil {
		return parsedSpec{err: err}
	}
	var spec struct {
		Name        string         `json:"name"`
		Summary     string         `json:"summary"`
		Description string         `json:"description"`
		Guidance    []string       `json:"guidance"`
		InputSchema jsontext.Value `json:"input_schema"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return parsedSpec{err: err}
	}
	if spec.Name != name || spec.Summary == "" || len(spec.InputSchema) == 0 {
		return parsedSpec{err: fmt.Errorf("core: specs/%s.json does not define %s with a summary and a schema", name, name)}
	}
	return parsedSpec{
		spec:    provider.Tool{Name: spec.Name, Description: spec.Description, InputSchema: spec.InputSchema},
		summary: spec.Summary, guidance: spec.Guidance,
	}
}

// limits are the configured values a spec names as {key} in its
// description and schema, so the model is told the numbers it is held to.
func limits(cfg config.Tools) map[string]string {
	return map[string]string{
		"{shell_timeout_ms}":     strconv.FormatInt(cfg.Shell.Timeout.Milliseconds(), 10),
		"{shell_max_timeout_ms}": strconv.FormatInt(cfg.Shell.MaxTimeout.Milliseconds(), 10),
		"{shell_output_chars}":   strconv.Itoa(bashOutputCharacters),
		"{read_default_lines}":   strconv.Itoa(cfg.Files.ReadDefaultLines),
	}
}

// fill puts the values in place of their keys in a spec's description and
// schema. A key left in braces that has no value is an error, so a spec
// never reaches a model with a placeholder in it.
func fill(spec provider.Tool, values map[string]string) (provider.Tool, error) {
	pairs := make([]string, 0, 2*len(values))
	for key, value := range values {
		pairs = append(pairs, key, value)
	}
	r := strings.NewReplacer(pairs...)
	spec.Description = r.Replace(spec.Description)
	spec.InputSchema = jsontext.Value(r.Replace(string(spec.InputSchema)))
	for _, text := range []string{spec.Description, string(spec.InputSchema)} {
		if key := unfilled.FindString(text); key != "" {
			return provider.Tool{}, fmt.Errorf("core: spec of %s names %s, which has no value", spec.Name, key)
		}
	}
	return spec, nil
}

var unfilled = regexp.MustCompile(`\{[a-z_]+\}`)
