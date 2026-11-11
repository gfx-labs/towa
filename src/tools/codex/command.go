package codex

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"math"
	"time"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/shell"
)

type commandArgs struct {
	Cmd           string   `json:"cmd"`
	Workdir       string   `json:"workdir"`
	Shell         string   `json:"shell"`
	Login         *bool    `json:"login"`
	TimeoutMS     *float64 `json:"timeout_ms"`
	MaxTokens     *float64 `json:"max_output_tokens"`
	Sandbox       string   `json:"sandbox_permissions"`
	Justification string   `json:"justification"`
	Prefix        []string `json:"prefix_rule"`
}

func commandTool(spec provider.Tool, cfg config.Tools) tools.Tool {
	return tools.Tool{
		Spec: spec, Effect: tools.Executes,
		Describe: func(input jsontext.Value) (tools.Description, error) {
			var args commandArgs
			if err := json.Unmarshal(input, &args); err != nil {
				return tools.Description{}, err
			}
			return tools.Description{Title: args.Cmd, Kind: acp.ToolKindExecute}, nil
		},
		Run: func(ctx context.Context, call tools.Call) (tools.Result, error) {
			var args commandArgs
			if err := json.Unmarshal(call.Input, &args, json.RejectUnknownMembers(true)); err != nil {
				return tools.Result{}, err
			}
			login := true
			if args.Login != nil {
				login = *args.Login
			}
			if args.Sandbox != "" && args.Sandbox != "use_default" {
				return textResult("exec_command: sandbox escalation is unavailable", true), nil
			}
			// core/src/exec.rs:63,71. Limits above these defaults come from config.
			timeout := 10 * time.Second
			if args.TimeoutMS != nil {
				n := *args.TimeoutMS
				if n <= 0 || n != math.Trunc(n) || n > float64(math.MaxInt64/int64(time.Millisecond)) {
					return textResult("exec_command: timeout_ms must be a positive whole number of milliseconds", true), nil
				}
				timeout = time.Duration(n) * time.Millisecond
			}
			tokens := defaultTokens
			if args.MaxTokens != nil {
				n := *args.MaxTokens
				if n < 0 || n != math.Trunc(n) || n > float64(math.MaxInt64/bytesPerToken) {
					return textResult("exec_command: max_output_tokens must be a nonnegative whole number", true), nil
				}
				tokens = min(int64(n), defaultTokens)
			}
			res, err := shell.Run(ctx, call.Env, cfg, shell.Input{Command: args.Cmd, Program: args.Shell, Login: login, Dir: args.Workdir, Timeout: timeout, CancelGrace: 50 * time.Millisecond})
			if err != nil {
				return tools.Result{}, err
			}
			if res.End == shell.EndCancelled {
				out := textResult(fmt.Sprintf("Wall time: %.1f seconds\naborted by user", res.Duration.Seconds()), true)
				out.Outcome = res.Outcome()
				return out, nil
			}
			code := res.ExitCode
			if res.SignalNumber > 0 {
				code = 128 + res.SignalNumber
			}
			if res.End == shell.EndTimedOut {
				code = 124
			}
			output := res.Output
			if res.DroppedBytes > 0 {
				output += fmt.Sprintf("\n... %d bytes omitted ...\n", res.DroppedBytes) + res.Tail
			}
			text := execText(output, res.Duration, code, tokens, res.DroppedBytes)
			if res.PipeHeld {
				text += "\nacpharness: output pipe remained open after the configured drain deadline"
			}
			result := textResult(text, code != 0)
			result.Outcome = res.Outcome()
			return result, nil
		},
	}
}
