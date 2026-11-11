// Package codex adapts shared capabilities to openai/codex ce0323f2.
package codex

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
const Dialect = "codex"

// Deps supplies session operations not performed by filesystem capabilities.
type Deps struct {
	// ShowPlan sends the checked list to the client. Nil omits update_plan.
	ShowPlan func(context.Context, tools.Env, []acp.PlanEntry) error
}

//go:embed specs/*.json
var specs embed.FS

// Tools returns the supported direct-mode tools. Describe resolves paths in env,
// while Run uses the call's environment, including its progress callback.
func Tools(env tools.Env, cfg config.Tools, deps Deps) ([]tools.Tool, error) {
	if !filepath.IsAbs(env.Cwd) {
		return nil, fmt.Errorf("codex: cwd %q is not absolute", env.Cwd)
	}
	execSpec, err := definition("exec_command")
	if err != nil {
		return nil, err
	}
	// Upstream core/src/tools/handlers/unified_exec/exec_command.rs:463-491.
	execSpec.Description = "Runs a command to completion and returns its output. The process is terminated on timeout or cancellation and cannot be resumed."
	var schema struct {
		Type                 string                    `json:"type"`
		Properties           map[string]jsontext.Value `json:"properties"`
		Required             []string                  `json:"required"`
		AdditionalProperties bool                      `json:"additionalProperties"`
	}
	if err := json.Unmarshal(execSpec.InputSchema, &schema); err != nil {
		return nil, err
	}
	delete(schema.Properties, "tty")
	delete(schema.Properties, "yield_time_ms")
	schema.Properties["timeout_ms"] = jsontext.Value(`{"description":"Maximum command runtime. Defaults to 10000 ms.","type":"number"}`)
	execSpec.InputSchema, err = json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	patchSpec, err := definition("apply_patch")
	if err != nil {
		return nil, err
	}
	out := []tools.Tool{commandTool(execSpec, cfg), patchTool(patchSpec, env, cfg)}
	if deps.ShowPlan != nil {
		spec, err := definition("update_plan")
		if err != nil {
			return nil, err
		}
		out = append(out, planTool(spec, deps))
	}
	return out, nil
}

// Captured specs cite core/src/tools/handlers/{shell_spec.rs:89,
// apply_patch_spec.rs:19,plan_spec.rs:43} in prompts/codex/ce0323f2/manifest.json.
func definition(name string) (provider.Tool, error) {
	raw, err := specs.ReadFile("specs/" + name + ".json")
	if err != nil {
		return provider.Tool{}, err
	}
	var spec struct {
		Type        string         `json:"type"`
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  jsontext.Value `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return provider.Tool{}, err
	}
	tool := provider.Tool{Name: spec.Name, Description: spec.Description, InputSchema: spec.Parameters}
	if spec.Type == "custom" {
		tool.Native = jsontext.Value(raw)
	}
	return tool, nil
}

func textResult(text string, failed bool) tools.Result {
	outcome := transcript.OutcomeCompleted
	if failed {
		outcome = transcript.OutcomeFailed
	}
	return tools.Result{Outcome: outcome, Parts: []transcript.Part{{Text: text}}}
}
