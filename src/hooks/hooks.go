// Package hooks runs the commands that configuration attaches to events of
// a turn: before a tool call, after it, when a prompt arrives and when the
// turn is about to end.
//
// A hook's command is run as it is configured, with no shell, in the
// session's working directory and with the harness's environment. It gets
// one JSON object on its standard input:
//
//	{"event":"pre_tool","session_id":"…","cwd":"/abs/dir",
//	 "tool":{"name":"Bash","call_id":"…","input":{…}}}
//
// post_tool adds "result":{"outcome":"completed","parts":[…]}, prompt has
// "prompt":[…] and no tool, and stop has "stop_reason". Parts are encoded as
// the transcript encodes them.
//
// The command answers with one JSON object on its standard output and exits
// with status zero:
//
//	{"decision":"allow"}
//	{"decision":"deny","reason":"why"}
//	{"decision":"allow","input":{…}}    pre_tool only: the input to use
//
// Anything else is a failed hook, and a failed hook denies. That covers a
// non-zero exit status whatever was printed, a timeout, a cancelled turn, a
// command that cannot be started, output that is not such an object
// (including no output, an unknown member, a deny without a reason, and an
// input outside pre_tool) and output over the configured size. The decision
// is then Deny with the failure as its reason, and the error is an *Error
// that says which of these happened. A hook never allows by failing.
//
// What a deny means is the caller's to carry out, and is the same whether
// the hook answered it or failed:
//
//   - pre_tool: the call is not run. The model gets the reason as the
//     call's failed result.
//   - post_tool: the call has already run. The model gets the reason in
//     place of the result, as a failed call.
//   - prompt: the turn does not start. The client gets the reason.
//   - stop: the turn does not end. The model gets the reason and goes on.
package hooks

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// Action is what a hook decided.
type Action string

// Actions of a decision. The zero value is neither, so a Decision that was
// never filled in does not read as an allow.
const (
	Allow Action = "allow"
	Deny  Action = "deny"
)

// Decision is the outcome of the hooks of one event.
type Decision struct {
	Action Action
	// Reason says why, when Action is Deny.
	Reason string
	// Input is the tool input to run the call with, when PreTool allows:
	// the input a hook changed it to, or the one that was passed in. The
	// caller validates a changed input as it validates the model's.
	Input jsontext.Value
}

// Kind says how a hook failed.
type Kind string

// Kinds of a failed hook.
const (
	// KindStart: the command could not be started.
	KindStart Kind = "start"
	// KindExit: the command exited with a non-zero status or was killed by
	// a signal the harness did not send.
	KindExit Kind = "exit"
	// KindTimeout: the command, or a process it left holding its output,
	// was still there after the hook's timeout. The process group was
	// killed.
	KindTimeout Kind = "timeout"
	// KindCancelled: the context was cancelled. The process group was
	// killed.
	KindCancelled Kind = "cancelled"
	// KindOutput: the command exited with status zero and its output is not
	// a valid answer, or is over the hook's max_output_bytes.
	KindOutput Kind = "output"
)

// Error is a failed hook.
type Error struct {
	Kind Kind
	// Event is the event the hook ran for.
	Event string
	// Index is the hook's position in the configured list.
	Index   int
	Command []string
	// ExitCode is the exit status when Kind is KindExit, and -1 when a
	// signal ended the command.
	ExitCode int
	// Stderr is what the command wrote to its standard error, up to the
	// hook's max_output_bytes. StderrDropped is how many bytes came after.
	Stderr        string
	StderrDropped int64
	// Detail says what was wrong, for KindOutput.
	Detail string
	// Err is the underlying error, when there is one.
	Err error
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "hooks[%d] (%s, %s): ", e.Index, e.Event, e.Command[0])
	switch e.Kind {
	case KindStart:
		b.WriteString("the command could not be started")
	case KindExit:
		fmt.Fprintf(&b, "the command exited with status %d", e.ExitCode)
	case KindTimeout:
		b.WriteString("the command timed out")
	case KindCancelled:
		b.WriteString("the command was cancelled")
	case KindOutput:
		b.WriteString("the command's answer cannot be used")
	default:
		b.WriteString(string(e.Kind))
	}
	if e.Detail != "" {
		b.WriteString(": " + e.Detail)
	}
	if e.Err != nil {
		b.WriteString(": " + e.Err.Error())
	}
	if s := strings.TrimSpace(e.Stderr); s != "" {
		b.WriteString(": " + s)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// Session is the session an event belongs to.
type Session struct {
	ID string
	// Cwd is the session's working directory. The command runs in it.
	Cwd string
}

// Runner runs the configured hooks.
type Runner struct {
	hooks []config.Hook
}

// New returns a Runner for the hooks of a validated configuration.
func New(hooks []config.Hook) *Runner {
	return &Runner{hooks: hooks}
}

// event is what a command reads from its standard input.
type event struct {
	Event      string            `json:"event"`
	SessionID  string            `json:"session_id"`
	Cwd        string            `json:"cwd"`
	Tool       *toolEvent        `json:"tool,omitzero"`
	Result     *resultEvent      `json:"result,omitzero"`
	Prompt     []transcript.Part `json:"prompt,omitzero"`
	StopReason string            `json:"stop_reason,omitzero"`
}

type toolEvent struct {
	Name   string         `json:"name"`
	CallID string         `json:"call_id"`
	Input  jsontext.Value `json:"input"`
}

type resultEvent struct {
	Outcome transcript.Outcome `json:"outcome"`
	Parts   []transcript.Part  `json:"parts"`
}

// answer is what a command writes to its standard output.
type answer struct {
	Decision string         `json:"decision"`
	Reason   string         `json:"reason,omitzero"`
	Input    jsontext.Value `json:"input,omitzero"`
}

// PreTool runs the pre_tool hooks that match the tool, in config order,
// before a call is run. A hook that changes the input passes the changed
// input to the hooks after it. The first deny ends the run.
func (r *Runner) PreTool(ctx context.Context, s Session, callID, tool string, input jsontext.Value) (Decision, error) {
	current := input
	return r.run(ctx, s, config.HookPreTool, tool, func() event {
		return event{Tool: &toolEvent{Name: tool, CallID: callID, Input: current}}
	}, func(a answer) {
		if a.Input != nil {
			current = a.Input
		}
	}, func(d *Decision) { d.Input = current })
}

// PostTool runs the post_tool hooks that match the tool, in config order,
// after a call has run.
func (r *Runner) PostTool(
	ctx context.Context, s Session, callID, tool string, input jsontext.Value,
	outcome transcript.Outcome, parts []transcript.Part,
) (Decision, error) {
	ev := event{
		Tool:   &toolEvent{Name: tool, CallID: callID, Input: input},
		Result: &resultEvent{Outcome: outcome, Parts: parts},
	}
	return r.run(ctx, s, config.HookPostTool, tool, func() event { return ev }, nil, nil)
}

// Prompt runs the prompt hooks, in config order, when a prompt arrives.
func (r *Runner) Prompt(ctx context.Context, s Session, parts []transcript.Part) (Decision, error) {
	return r.run(ctx, s, config.HookPrompt, "", func() event { return event{Prompt: parts} }, nil, nil)
}

// Stop runs the stop hooks, in config order, when a turn is about to end.
// reason is why it is ending.
func (r *Runner) Stop(ctx context.Context, s Session, reason string) (Decision, error) {
	return r.run(ctx, s, config.HookStop, "", func() event { return event{StopReason: reason} }, nil, nil)
}

// run runs every hook of one event that matches. build makes the event for
// the next hook, accept sees each allow, and finish completes the decision
// when every hook allowed.
func (r *Runner) run(
	ctx context.Context, s Session, name, tool string,
	build func() event, accept func(answer), finish func(*Decision),
) (Decision, error) {
	for i, h := range r.hooks {
		if h.Event != name || (len(h.Tools) > 0 && !slices.Contains(h.Tools, tool)) {
			continue
		}
		ev := build()
		ev.Event, ev.SessionID, ev.Cwd = name, s.ID, s.Cwd
		a, herr := runOne(ctx, h, ev)
		if herr != nil {
			herr.Event, herr.Index, herr.Command = name, i, h.Command
			return Decision{Action: Deny, Reason: herr.Error()}, herr
		}
		if a.Decision == string(Deny) {
			return Decision{Action: Deny, Reason: a.Reason}, nil
		}
		if accept != nil {
			accept(a)
		}
	}
	d := Decision{Action: Allow}
	if finish != nil {
		finish(&d)
	}
	return d, nil
}

// capped keeps what is written to it up to a limit and counts the rest, so
// that a command that writes too much is not blocked and is not held in
// memory.
type capped struct {
	buf     bytes.Buffer
	limit   int64 // zero means no limit
	dropped int64
}

func (c *capped) Write(p []byte) (int, error) {
	keep := len(p)
	if c.limit > 0 {
		if room := c.limit - int64(c.buf.Len()); int64(keep) > room {
			keep = int(room)
		}
	}
	c.buf.Write(p[:keep])
	c.dropped += int64(len(p) - keep)
	return len(p), nil
}

// runOne runs one hook's command and checks its answer. The returned Error
// has its Kind and details set, and the caller adds which hook it was.
func runOne(ctx context.Context, h config.Hook, ev event) (answer, *Error) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return answer{}, &Error{Kind: KindStart, Detail: "the event cannot be encoded", Err: err}
	}

	// The pipes are ours, not os/exec's, so that Wait returns when the
	// command exits even if a process it started still holds one, and so
	// that closing our ends stops the reads.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return answer{}, &Error{Kind: KindStart, Err: err}
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return answer{}, &Error{Kind: KindStart, Err: errors.Join(err, stdinR.Close(), stdinW.Close())}
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		return answer{}, &Error{Kind: KindStart, Err: errors.Join(
			err, stdinR.Close(), stdinW.Close(), stdoutR.Close(), stdoutW.Close(),
		)}
	}

	cmd := exec.Command(h.Command[0], h.Command[1:]...) //nolint:gosec // the operator's configured command
	cmd.Dir = ev.Cwd
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdinR, stdoutW, stderrW
	cmd.SysProcAttr = groupAttr()
	startErr := cmd.Start()
	// The child has its own copies of these ends.
	closeErr := errors.Join(stdinR.Close(), stdoutW.Close(), stderrW.Close())
	if startErr != nil || closeErr != nil {
		err := errors.Join(startErr, closeErr, stdinW.Close(), stdoutR.Close(), stderrR.Close())
		if startErr == nil {
			err = errors.Join(err, killGroup(cmd.Process.Pid), cmd.Wait())
		}
		return answer{}, &Error{Kind: KindStart, Err: err}
	}

	var closeOnce sync.Once
	closeOurs := func() {
		// Errors are not kept: an end may already be closed, and a read
		// that fails is reported by the kind of failure that closed it.
		closeOnce.Do(func() { _, _, _ = stdinW.Close(), stdoutR.Close(), stderrR.Close() })
	}
	defer closeOurs()

	go func() {
		// A command need not read its input, so a broken pipe is not a
		// failure. A command that needed it fails on its own.
		_, _ = stdinW.Write(payload)
		_ = stdinW.Close()
	}()
	stdout, stderr := &capped{limit: h.MaxOutputBytes}, &capped{limit: h.MaxOutputBytes}
	var reads sync.WaitGroup
	reads.Go(func() { _, _ = io.Copy(stdout, stdoutR) })
	reads.Go(func() { _, _ = io.Copy(stderr, stderrR) })
	finished := make(chan error, 1)
	go func() {
		waitErr := cmd.Wait()
		reads.Wait()
		finished <- waitErr
	}()

	timer := time.NewTimer(h.Timeout)
	defer timer.Stop()
	var waitErr error
	failure := &Error{}
	select {
	case waitErr = <-finished:
	case <-timer.C:
		failure.Kind = KindTimeout
		failure.Detail = "after " + h.Timeout.String()
	case <-ctx.Done():
		failure.Kind = KindCancelled
		failure.Err = context.Cause(ctx)
	}
	if failure.Kind != "" {
		// Kill first, so nothing in the group writes after the close.
		if err := killGroup(cmd.Process.Pid); err != nil {
			failure.Err = errors.Join(failure.Err, err)
		}
		closeOurs()
		// The wait error is the kill we just sent, which Kind already says.
		waitErr = <-finished
	}
	failure.Stderr, failure.StderrDropped = stderr.buf.String(), stderr.dropped
	if failure.Kind != "" {
		return answer{}, failure
	}

	if waitErr != nil {
		failure.Kind = KindExit
		var exit *exec.ExitError
		if errors.As(waitErr, &exit) {
			failure.ExitCode = exit.ExitCode()
			if failure.ExitCode < 0 {
				failure.Detail = exit.String()
			}
		} else {
			failure.ExitCode, failure.Err = -1, waitErr
		}
		return answer{}, failure
	}

	failure.Kind = KindOutput
	if stdout.dropped > 0 {
		failure.Detail = fmt.Sprintf("%d bytes of output is over max_output_bytes = %d",
			int64(stdout.buf.Len())+stdout.dropped, h.MaxOutputBytes)
		return answer{}, failure
	}
	var a answer
	if err := json.Unmarshal(stdout.buf.Bytes(), &a, json.RejectUnknownMembers(true)); err != nil {
		failure.Detail, failure.Err = "not a JSON answer", err
		return answer{}, failure
	}
	switch {
	case a.Decision != string(Allow) && a.Decision != string(Deny):
		failure.Detail = fmt.Sprintf("decision %q is not %q or %q", a.Decision, Allow, Deny)
	case a.Decision == string(Deny) && strings.TrimSpace(a.Reason) == "":
		failure.Detail = "a deny needs a reason"
	case a.Decision == string(Allow) && a.Reason != "":
		failure.Detail = "an allow takes no reason"
	case a.Input != nil && ev.Event != config.HookPreTool:
		failure.Detail = "only a pre_tool hook can change the input"
	case a.Input != nil && a.Decision == string(Deny):
		failure.Detail = "a deny takes no input"
	default:
		return a, nil
	}
	return answer{}, failure
}
