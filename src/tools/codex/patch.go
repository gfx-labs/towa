package codex

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/fsops"
	"github.com/gfx-labs/acpharness/src/tools/patch"
)

func patchTool(spec provider.Tool, env tools.Env, cfg config.Tools) tools.Tool {
	return tools.Tool{
		Spec: spec, Effect: tools.Writes,
		Describe: func(input jsontext.Value) (tools.Description, error) {
			desc := tools.Description{Title: "Apply patch", Kind: acp.ToolKindEdit}
			var text string
			// Let Run return upstream's parse error instead of a harness refusal.
			if err := json.Unmarshal(input, &text); err != nil {
				return desc, nil
			}
			p, err := patch.Parse(text)
			if err != nil {
				return desc, nil
			}
			roots := fsops.RootsOf(env, cfg.Files)
			onlyDelete := len(p.Hunks) > 0
			for _, h := range p.Hunks {
				onlyDelete = onlyDelete && h.Kind == patch.Delete
				for _, path := range []string{h.Path, h.Move} {
					if path == "" {
						continue
					}
					abs, err := roots.Resolve(path)
					if err != nil {
						return tools.Description{Title: "Apply patch", Kind: acp.ToolKindEdit}, nil
					}
					desc.Locations = append(desc.Locations, acp.ToolCallLocation{Path: abs})
					desc.WritePaths = append(desc.WritePaths, abs)
				}
			}
			if onlyDelete {
				desc.Title = "Delete files"
				desc.Kind = acp.ToolKindDelete
			}
			return desc, nil
		},
		Run: func(ctx context.Context, call tools.Call) (tools.Result, error) {
			var text string
			if err := json.Unmarshal(call.Input, &text); err != nil {
				return textResult("apply_patch handler received unsupported payload", true), nil
			}
			p, err := patch.Parse(text)
			if err != nil {
				return textResult("apply_patch verification failed: "+err.Error(), true), nil
			}
			start := time.Now()
			applied, err := patch.Apply(ctx, fsops.RootsOf(call.Env, cfg.Files), cfg.Files, p)
			code := 0
			output := applied.Summary()
			if err != nil {
				code = 1
				output = err.Error() + "\n"
				if len(applied.Changes) > 0 {
					// Upstream keeps the delta but does not print it on failure.
					output += "Changes applied before the failure:\n" + strings.TrimPrefix(applied.Summary(), "Success. Updated the following files:\n")
				}
			}
			// core/src/tools/mod.rs:91-115, used by ToolEmitter for apply_patch.
			seconds := float64(int64(time.Since(start).Seconds()*10+0.5)) / 10
			result := textResult(fmt.Sprintf("Exit code: %d\nWall time: %g seconds\nOutput:\n%s", code, seconds, output), err != nil)
			for _, change := range applied.Changes {
				result.Content = append(result.Content, change.Change.Diff())
			}
			if len(result.Content) > 0 && err != nil {
				result.Content = append(result.Content, tools.TextContent(result.Parts[0].Text))
			}
			return result, nil
		},
	}
}
