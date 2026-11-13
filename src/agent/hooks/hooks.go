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
	"syscall"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/transcript"
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
	for _, part := range []string{e.Detail, errorText(e.Err), strings.TrimSpace(e.Stderr)} {
		if part != "" {
			b.WriteString(": ")
			b.WriteString(part)
		}
	}
	return b.String()
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
	Decision Action         `json:"decision"`
	Reason   string         `json:"reason,omitzero"`
	Input    jsontext.Value `json:"input,omitzero"`
}

// invalid says what is wrong with the answer to an event, or "" when it can
// be used.
func (a answer) invalid(event string) string {
	switch {
	case a.Decision != Allow && a.Decision != Deny:
		return fmt.Sprintf("decision %q is not %q or %q", a.Decision, Allow, Deny)
	case a.Decision == Deny && strings.TrimSpace(a.Reason) == "":
		return "a deny needs a reason"
	case a.Decision == Allow && a.Reason != "":
		return "an allow takes no reason"
	case a.Input != nil && event != config.HookPreTool:
		return "only a pre_tool hook can change the input"
	case a.Input != nil && a.Decision == Deny:
		return "a deny takes no input"
	}
	return ""
}

// PreTool runs the pre_tool hooks that match the tool, in config order,
// before a call is run. A hook that changes the input passes the changed
// input to the hooks after it. The first deny ends the run.
func (r *Runner) PreTool(ctx context.Context, s Session, callID, tool string, input jsontext.Value) (Decision, error) {
	return r.run(ctx, s, config.HookPreTool, tool, event{
		Tool: &toolEvent{Name: tool, CallID: callID, Input: input},
	})
}

// PostTool runs the post_tool hooks that match the tool, in config order,
// after a call has run.
func (r *Runner) PostTool(
	ctx context.Context, s Session, callID, tool string, input jsontext.Value,
	outcome transcript.Outcome, parts []transcript.Part,
) (Decision, error) {
	return r.run(ctx, s, config.HookPostTool, tool, event{
		Tool:   &toolEvent{Name: tool, CallID: callID, Input: input},
		Result: &resultEvent{Outcome: outcome, Parts: parts},
	})
}

// Prompt runs the prompt hooks, in config order, when a prompt arrives.
func (r *Runner) Prompt(ctx context.Context, s Session, parts []transcript.Part) (Decision, error) {
	return r.run(ctx, s, config.HookPrompt, "", event{Prompt: parts})
}

// Stop runs the stop hooks, in config order, when a turn is about to end.
// reason is why it is ending.
func (r *Runner) Stop(ctx context.Context, s Session, reason string) (Decision, error) {
	return r.run(ctx, s, config.HookStop, "", event{StopReason: reason})
}

// run sends ev to every hook of the named event that matches the tool, until
// one denies or fails.
func (r *Runner) run(ctx context.Context, s Session, name, tool string, ev event) (Decision, error) {
	ev.Event, ev.SessionID, ev.Cwd = name, s.ID, s.Cwd
	for i, h := range r.hooks {
		if h.Event != name || (len(h.Tools) > 0 && !slices.Contains(h.Tools, tool)) {
			continue
		}
		a, failure := ask(ctx, h, ev)
		if failure != nil {
			failure.Event, failure.Index, failure.Command = name, i, h.Command
			return Decision{Action: Deny, Reason: failure.Error()}, failure
		}
		if a.Decision == Deny {
			return Decision{Action: Deny, Reason: a.Reason}, nil
		}
		if a.Input != nil {
			ev.Tool.Input = a.Input
		}
	}
	d := Decision{Action: Allow}
	if name == config.HookPreTool {
		d.Input = ev.Tool.Input
	}
	return d, nil
}

// ask runs one hook's command with the event and returns its answer. The
// caller adds which hook it was to a failure.
func ask(ctx context.Context, h config.Hook, ev event) (answer, *Error) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return answer{}, &Error{Kind: KindStart, Detail: "the event cannot be encoded", Err: err}
	}
	stdout, stderr, failure := execute(ctx, h, ev.Cwd, payload)
	if failure == nil {
		var a answer
		if a, failure = decode(stdout, h.MaxOutputBytes, ev.Event); failure == nil {
			return a, nil
		}
	}
	if stderr != nil {
		failure.Stderr, failure.StderrDropped = stderr.buf.String(), stderr.dropped
	}
	return answer{}, failure
}

// decode reads the answer to an event from what a command printed.
func decode(stdout *capped, limit int64, event string) (answer, *Error) {
	if stdout.dropped > 0 {
		return answer{}, &Error{Kind: KindOutput, Detail: fmt.Sprintf(
			"%d bytes of output is over max_output_bytes = %d", int64(stdout.buf.Len())+stdout.dropped, limit,
		)}
	}
	var a answer
	if err := json.Unmarshal(stdout.buf.Bytes(), &a, json.RejectUnknownMembers(true)); err != nil {
		return answer{}, &Error{Kind: KindOutput, Detail: "not a JSON answer", Err: err}
	}
	if detail := a.invalid(event); detail != "" {
		return answer{}, &Error{Kind: KindOutput, Detail: detail}
	}
	return a, nil
}

// errTimeout is the cause of a context that ended at a hook's timeout.
var errTimeout = errors.New("hooks: the timeout passed")

// execute runs a hook's command in cwd with payload on its standard input,
// and returns what it wrote once it and everything holding its output have
// gone. stderr is nil when the command did not start.
func execute(ctx context.Context, h config.Hook, cwd string, payload []byte) (stdout, stderr *capped, _ *Error) {
	// The pipes are ours, not os/exec's, so that Wait returns when the
	// command exits even if a process it started still holds one, and so
	// that closing our ends stops the reads.
	child, ours, err := openPipes()
	if err != nil {
		return nil, nil, &Error{Kind: KindStart, Err: err}
	}
	cmd := exec.Command(h.Command[0], h.Command[1:]...) //nolint:gosec // the operator's configured command
	cmd.Dir = cwd
	cmd.Stdin, cmd.Stdout, cmd.Stderr = child[0], child[1], child[2]
	// The command leads a new process group, so that it and everything it
	// starts can be killed together.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	startErr := cmd.Start()
	// The child has its own copies of these ends.
	closeErr := closeAll(child[:])
	if startErr != nil || closeErr != nil {
		err := errors.Join(startErr, closeErr, closeAll(ours[:]))
		if startErr == nil {
			err = errors.Join(err, killGroup(cmd.Process.Pid), cmd.Wait())
		}
		return nil, nil, &Error{Kind: KindStart, Err: err}
	}
	// Errors are not kept: an end may already be closed, and a read that
	// fails is reported by the kind of failure that closed it.
	closeOurs := sync.OnceFunc(func() { _ = closeAll(ours[:]) })
	defer closeOurs()

	go func() {
		// A command need not read its input, so a broken pipe is not a
		// failure. A command that needed it fails on its own.
		_, _ = ours[0].Write(payload)
		_ = ours[0].Close()
	}()
	stdout, stderr = &capped{limit: h.MaxOutputBytes}, &capped{limit: h.MaxOutputBytes}
	var reads sync.WaitGroup
	reads.Go(func() { _, _ = io.Copy(stdout, ours[1]) })
	reads.Go(func() { _, _ = io.Copy(stderr, ours[2]) })
	var waitErr error
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		waitErr = cmd.Wait()
		reads.Wait()
	}()

	ctx, cancel := context.WithTimeoutCause(ctx, h.Timeout, errTimeout)
	defer cancel()
	select {
	case <-exited:
		return stdout, stderr, exitFailure(waitErr)
	case <-ctx.Done():
	}
	failure := &Error{Kind: KindCancelled, Err: context.Cause(ctx)}
	if errors.Is(failure.Err, errTimeout) {
		failure = &Error{Kind: KindTimeout, Detail: "after " + h.Timeout.String()}
	}
	// Kill first, so nothing in the group writes after the close.
	if err := killGroup(cmd.Process.Pid); err != nil {
		failure.Err = errors.Join(failure.Err, err)
	}
	closeOurs()
	// What Wait returns now is the kill we just sent, which Kind already says.
	<-exited
	return stdout, stderr, failure
}

// exitFailure is the failure of a command that Wait returned err for, or nil
// when it exited with status zero.
func exitFailure(err error) *Error {
	if err == nil {
		return nil
	}
	exit, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		return &Error{Kind: KindExit, ExitCode: -1, Err: err}
	}
	failure := &Error{Kind: KindExit, ExitCode: exit.ExitCode()}
	if failure.ExitCode < 0 {
		failure.Detail = exit.String()
	}
	return failure
}

// openPipes opens the pipes of a command's standard input, output and error,
// in that order. child holds the ends the command gets and ours the others.
func openPipes() (child, ours [3]*os.File, err error) {
	for i := range child {
		r, w, err := os.Pipe()
		if err != nil {
			return child, ours, errors.Join(err, closeAll(child[:i]), closeAll(ours[:i]))
		}
		if i == 0 {
			child[i], ours[i] = r, w
		} else {
			child[i], ours[i] = w, r
		}
	}
	return child, ours, nil
}

func closeAll(files []*os.File) error {
	var errs []error
	for _, f := range files {
		errs = append(errs, f.Close())
	}
	return errors.Join(errs...)
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
		keep = int(min(int64(keep), c.limit-int64(c.buf.Len())))
	}
	c.buf.Write(p[:keep])
	c.dropped += int64(len(p) - keep)
	return len(p), nil
}
