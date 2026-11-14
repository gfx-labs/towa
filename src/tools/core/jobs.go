package core

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	acp "github.com/gfx-labs/go-acp"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/jobs"
	"github.com/gfx-labs/towa/src/tools/shell"
	"github.com/gfx-labs/towa/src/transcript"
)

// jobTailBytes is the most of a job's output read back for the model: the
// model is given at most bashOutputCharacters UTF-16 units, and a unit is
// at most four bytes of UTF-8.
const jobTailBytes = 4 * bashOutputCharacters

func startJob(ctx context.Context, call tools.Call, cfg config.Tools, jobSet *jobs.Set, args bashArgs, in shell.Input) tools.Result {
	label := args.Description
	if label == "" {
		label = args.Command
	}
	// A background command is not waited on, so it gets the longest
	// timeout unless the call names one.
	if in.Timeout == 0 {
		in.Timeout = cfg.Shell.MaxTimeout
	}
	job, err := jobSet.Start(ctx, call.Env, cfg, call.ID, label, in, renderJob)
	if err != nil {
		return tools.TextResult(transcript.OutcomeFailed, "The command did not run: "+err.Error())
	}
	return tools.TextResult(transcript.OutcomeCompleted, fmt.Sprintf(
		"Command running in background with ID: %s. Output is being written to: %s. You will be told when it ends.",
		job.ID, job.Path))
}

// renderJob says how a job ended, with the end of its output.
func renderJob(job *jobs.Job, run shell.Result, err error) (transcript.Outcome, string) {
	if err != nil {
		return transcript.OutcomeFailed, fmt.Sprintf("Background command %s (%q) could not be run: %v", job.ID, job.Label, err)
	}
	var how string
	switch run.End {
	case shell.EndCancelled:
		how = "was stopped"
	case shell.EndTimedOut:
		how = fmt.Sprintf("timed out after %d milliseconds and was stopped", run.Timeout.Milliseconds())
	default:
		code := run.ExitCode
		if run.SignalNumber != 0 {
			code = 128 + run.SignalNumber
		}
		how = fmt.Sprintf("exited with code %d", code)
	}
	text := fmt.Sprintf("Background command %s (%q) %s. Full output: %s", job.ID, job.Label, how, job.Path)
	if run.OutputErr != nil {
		text += fmt.Sprintf("\n[Writing the output file failed, so it is incomplete: %v]", run.OutputErr)
	}
	tail, size, tailErr := job.Tail(jobTailBytes)
	switch {
	case tailErr != nil:
		text += fmt.Sprintf("\n[The output could not be read back: %v]", tailErr)
	case size == 0:
		text += "\n[No output.]"
	default:
		text += "\n" + outputTail(tail, size)
	}
	if run.PipeHeld {
		text += "\n[Output pipe remained open after the configured drain deadline.]"
	}
	outcome := run.Outcome()
	if outcome == transcript.OutcomeUnknown {
		outcome = transcript.OutcomeFailed
	}
	return outcome, text
}

// outputTail is the end of an output of size bytes, cut to what the model
// is given, saying how much came before it.
func outputTail(tail string, size int64) string {
	kept := lastUnits(strings.TrimRight(tail, " \t\r\n"), bashOutputCharacters)
	if skipped := size - int64(len(kept)); skipped > 0 {
		return fmt.Sprintf("[%d earlier bytes not shown]\n%s", skipped, kept)
	}
	return kept
}

// lastUnits is the end of s that fits in n UTF-16 units.
func lastUnits(s string, n int) string {
	units := 0
	for i := len(s); i > 0; {
		r, size := utf8.DecodeLastRuneInString(s[:i])
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units+width > n {
			return s[i:]
		}
		units += width
		i -= size
	}
	return s
}

type jobArgs struct {
	JobID   string   `json:"job_id"`
	Block   bool     `json:"block"`
	Timeout *float64 `json:"timeout"`
}

func jobOutputTool(spec provider.Tool, cfg config.Tools, jobSet *jobs.Set) tools.Tool {
	return tools.Tool{
		Spec: spec, Effect: tools.ReadOnly,
		Describe: func(input jsontext.Value) (tools.Description, error) {
			desc := tools.Description{Title: "Background command output", Kind: acp.ToolKindRead}
			var args jobArgs
			if err := json.Unmarshal(input, &args); err == nil && args.JobID != "" {
				desc.Title += ": " + args.JobID
			}
			return desc, nil
		},
		Run: func(ctx context.Context, call tools.Call) (tools.Result, error) {
			var args jobArgs
			if err := json.Unmarshal(call.Input, &args, json.RejectUnknownMembers(true)); err != nil {
				return tools.TextResult(transcript.OutcomeFailed, "Invalid bash_output input: "+err.Error()), nil
			}
			job, refusal := lookupJob(jobSet, args.JobID)
			if job == nil {
				return tools.TextResult(transcript.OutcomeFailed, refusal), nil
			}
			if args.Block {
				wait := cfg.Shell.Timeout
				if args.Timeout != nil {
					timeout, refusal := millis(*args.Timeout, cfg)
					if refusal != "" {
						return tools.TextResult(transcript.OutcomeFailed, refusal), nil
					}
					wait = timeout
				}
				timer := time.NewTimer(wait)
				defer timer.Stop()
				select {
				case <-job.Done():
				case <-timer.C:
				case <-ctx.Done():
					return tools.TextResult(transcript.OutcomeCancelled, "The wait was cancelled. The command still runs."), nil
				}
			}
			select {
			case <-job.Done():
				jobSet.MarkReported(job)
				_, report := job.Report()
				return tools.TextResult(transcript.OutcomeCompleted, report), nil
			default:
			}
			tail, size, err := job.Tail(jobTailBytes)
			if err != nil {
				return tools.TextResult(transcript.OutcomeFailed, err.Error()), nil
			}
			text := fmt.Sprintf("Background command %s (%q) is still running. Full output: %s", job.ID, job.Label, job.Path)
			if size == 0 {
				text += "\n[No output yet.]"
			} else {
				text += "\n" + outputTail(tail, size)
			}
			return tools.TextResult(transcript.OutcomeCompleted, text), nil
		},
	}
}

func jobStopTool(spec provider.Tool, jobSet *jobs.Set) tools.Tool {
	return tools.Tool{
		Spec: spec, Effect: tools.Executes,
		Describe: func(input jsontext.Value) (tools.Description, error) {
			desc := tools.Description{Title: "Stop background command", Kind: acp.ToolKindExecute}
			var args jobArgs
			if err := json.Unmarshal(input, &args); err == nil && args.JobID != "" {
				desc.Title += ": " + args.JobID
			}
			return desc, nil
		},
		Run: func(_ context.Context, call tools.Call) (tools.Result, error) {
			var args struct {
				JobID string `json:"job_id"`
			}
			if err := json.Unmarshal(call.Input, &args, json.RejectUnknownMembers(true)); err != nil {
				return tools.TextResult(transcript.OutcomeFailed, "Invalid bash_stop input: "+err.Error()), nil
			}
			job, refusal := lookupJob(jobSet, args.JobID)
			if job == nil {
				return tools.TextResult(transcript.OutcomeFailed, refusal), nil
			}
			job.Stop()
			jobSet.MarkReported(job)
			_, report := job.Report()
			return tools.TextResult(transcript.OutcomeCompleted, report), nil
		},
	}
}

func lookupJob(jobSet *jobs.Set, id string) (*jobs.Job, string) {
	if job, ok := jobSet.Get(id); ok {
		return job, ""
	}
	ids := jobSet.IDs()
	if len(ids) == 0 {
		return nil, fmt.Sprintf("There is no background command %q. None was started in this session.", id)
	}
	return nil, fmt.Sprintf("There is no background command %q. The ids are: %s.", id, strings.Join(ids, ", "))
}
