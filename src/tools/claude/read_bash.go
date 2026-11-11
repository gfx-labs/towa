package claude

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/fsops"
	"github.com/gfx-labs/acpharness/src/tools/shell"
)

type readArgs struct {
	Path   string  `json:"file_path"`
	Offset int     `json:"offset"`
	Limit  int     `json:"limit"`
	Pages  *string `json:"pages"`
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
				return textResult("Invalid Read input: "+err.Error(), true), nil
			}
			extension := strings.ToLower(filepath.Ext(args.Path))
			if args.Pages != nil || extension == ".pdf" || extension == ".ipynb" {
				return textResult("PDF pages and notebook reads are not supported by this harness.", true), nil
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
	Command        string   `json:"command"`
	Timeout        *float64 `json:"timeout"`
	Description    string   `json:"description"`
	Background     bool     `json:"run_in_background"`
	DisableSandbox *bool    `json:"dangerouslyDisableSandbox"`
}

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
				return textResult("Invalid Bash input: "+err.Error(), true), nil
			}
			if args.Background {
				return textResult("Background commands are not supported by this harness.", true), nil
			}
			if args.DisableSandbox != nil {
				return textResult("Sandbox selection is not supported by this harness. Commands run without an operating-system sandbox.", true), nil
			}
			// 2.1.280 chunk-1xqpf2j8.js:19 uses 1500 ms before the final kill.
			in := shell.Input{Command: args.Command, CancelGrace: 1500 * time.Millisecond, TimeoutGrace: 1500 * time.Millisecond}
			if args.Timeout != nil {
				ms := *args.Timeout
				if math.IsNaN(ms) || math.IsInf(ms, 0) || ms <= 0 || ms > float64(cfg.Shell.MaxTimeout)/float64(time.Millisecond) {
					return textResult("Bash timeout must be positive and within the configured maximum.", true), nil
				}
				ns := ms * float64(time.Millisecond)
				if ns < 1 || ns >= float64(math.MaxInt64) {
					return textResult("Bash timeout is outside the supported duration range.", true), nil
				}
				in.Timeout = time.Duration(ns)
			}
			result, err := shell.Run(ctx, call.Env, cfg, in)
			if err != nil {
				return textResult(err.Error(), true), nil
			}
			return bashResult(result), nil
		},
	}
}
