package core

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"math"
	"time"

	acp "github.com/gfx-labs/go-acp"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/fsops"
	"github.com/gfx-labs/towa/src/tools/shell"
	"github.com/gfx-labs/towa/src/transcript"
)

type readArgs struct {
	Path   string `json:"file_path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

func readTool(spec provider.Tool, env tools.Env, cfg config.Tools, tracker *ReadTracker) tools.Tool {
	return tools.Tool{
		Spec: spec, Effect: tools.ReadOnly,
		Describe: func(input jsontext.Value) (tools.Description, error) {
			desc := tools.Description{Title: "Read file", Kind: acp.ToolKindRead}
			var args readArgs
			if err := json.Unmarshal(input, &args); err != nil {
				return desc, nil
			}
			path, err := fsops.RootsOf(env, cfg.Files).Resolve(args.Path)
			if err == nil {
				desc.Locations = []acp.ToolCallLocation{{Path: path}}
			}
			return desc, nil
		},
		Run: func(ctx context.Context, call tools.Call) (tools.Result, error) {
			var args readArgs
			if err := json.Unmarshal(call.Input, &args, json.RejectUnknownMembers(true)); err != nil {
				return tools.TextResult(transcript.OutcomeFailed, "Invalid read input: "+err.Error()), nil
			}
			result, err := ReadFile(ctx, fsops.RootsOf(call.Env, cfg.Files), cfg.Files, tracker, fsops.ReadInput{Path: args.Path, Offset: args.Offset, Limit: args.Limit})
			if err != nil {
				return fileFailure(err), nil
			}
			return readResult(result), nil
		},
	}
}

type bashArgs struct {
	Command     string   `json:"command"`
	Timeout     *float64 `json:"timeout"`
	Description string   `json:"description"`
}

// stopGrace is how long a cancelled or timed out command has to exit after
// SIGTERM before it is killed. Claude Code 2.1.280 uses the same 1500 ms.
const stopGrace = 1500 * time.Millisecond

func bashTool(spec provider.Tool, cfg config.Tools) tools.Tool {
	return tools.Tool{
		Spec: spec, Effect: tools.Executes,
		Describe: func(input jsontext.Value) (tools.Description, error) {
			desc := tools.Description{Title: "Run command", Kind: acp.ToolKindExecute}
			var args bashArgs
			if err := json.Unmarshal(input, &args); err == nil && args.Description != "" {
				desc.Title = args.Description
			}
			return desc, nil
		},
		Run: func(ctx context.Context, call tools.Call) (tools.Result, error) {
			var args bashArgs
			if err := json.Unmarshal(call.Input, &args, json.RejectUnknownMembers(true)); err != nil {
				return tools.TextResult(transcript.OutcomeFailed, "Invalid bash input: "+err.Error()), nil
			}
			in := shell.Input{Command: args.Command, CancelGrace: stopGrace, TimeoutGrace: stopGrace}
			if args.Timeout != nil {
				ms := *args.Timeout
				if math.IsNaN(ms) || math.IsInf(ms, 0) || ms <= 0 || ms > float64(cfg.Shell.MaxTimeout)/float64(time.Millisecond) {
					return tools.TextResult(transcript.OutcomeFailed, "timeout must be positive and within the configured maximum."), nil
				}
				ns := ms * float64(time.Millisecond)
				if ns < 1 || ns >= float64(math.MaxInt64) {
					return tools.TextResult(transcript.OutcomeFailed, "timeout is outside the supported duration range."), nil
				}
				in.Timeout = time.Duration(ns)
			}
			result, err := shell.Run(ctx, call.Env, cfg, in)
			if err != nil {
				return tools.TextResult(transcript.OutcomeFailed, err.Error()), nil
			}
			return bashResult(result), nil
		},
	}
}
