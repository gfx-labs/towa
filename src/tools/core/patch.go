package core

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"

	acp "github.com/gfx-labs/go-acp"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/fsops"
	"github.com/gfx-labs/towa/src/tools/patch"
	"github.com/gfx-labs/towa/src/transcript"
)

type patchArgs struct {
	Text string `json:"patch_text"`
}

// patchTool needs no earlier read of a file: a hunk's context and removed
// lines must match the file as it is, which is the same check.
func patchTool(spec provider.Tool, env tools.Env, cfg config.Tools, tracker *ReadTracker) tools.Tool {
	return tools.Tool{
		Spec: spec, Effect: tools.Writes,
		Describe: func(input jsontext.Value) (tools.Description, error) {
			desc := tools.Description{Title: "Apply patch", Kind: acp.ToolKindEdit}
			var args patchArgs
			// A patch that cannot be read is reported by Run, to the model.
			if err := json.Unmarshal(input, &args); err != nil {
				return desc, nil
			}
			p, err := patch.Parse(args.Text)
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
				desc.Title, desc.Kind = "Delete files", acp.ToolKindDelete
			}
			return desc, nil
		},
		Run: func(ctx context.Context, call tools.Call) (tools.Result, error) {
			var args patchArgs
			if err := json.Unmarshal(call.Input, &args, json.RejectUnknownMembers(true)); err != nil {
				return tools.TextResult(transcript.OutcomeFailed, "Invalid apply_patch input: "+err.Error()), nil
			}
			p, err := patch.Parse(args.Text)
			if err != nil {
				return tools.TextResult(transcript.OutcomeFailed, "The patch could not be read: "+err.Error()), nil
			}
			applied, applyErr := patch.Apply(ctx, fsops.RootsOf(call.Env, cfg.Files), cfg.Files, p)
			text := applied.Summary()
			if applyErr != nil {
				text = applyErr.Error() + "\n"
				if len(applied.Changes) > 0 {
					text += "Changes applied before the failure:\n" + strings.TrimPrefix(applied.Summary(), "Success. Updated the following files:\n")
				}
			}
			outcome := transcript.OutcomeCompleted
			if applyErr != nil {
				outcome = transcript.OutcomeFailed
			}
			result := tools.TextResult(outcome, text)
			// A later hunk can move or delete a file an earlier one wrote, so
			// only the last change to a path says whether the file is there.
			last := make(map[string]int, len(applied.Changes))
			for i, change := range applied.Changes {
				last[change.Change.Path] = i
			}
			for i, change := range applied.Changes {
				result.Content = append(result.Content, change.Change.Diff())
				if change.Change.Removed || last[change.Change.Path] != i {
					continue
				}
				// The session wrote this content, so edit may follow without a read.
				if err := tracker.Write(ctx, change.Change.Path, func() error { return nil }); err != nil {
					return tools.Result{}, err
				}
			}
			if len(result.Content) > 0 && applyErr != nil {
				result.Content = append(result.Content, tools.TextContent(text))
			}
			return result, nil
		},
	}
}
