// Package shell runs a command for a session and reports how it ended.
package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/fsops"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// readChunk is the size of one read from the command's output pipe. It is
// the capacity of a Linux pipe, so one read can empty it.
const readChunk = 64 << 10

// Input is one command.
type Input struct {
	// Command is the text given to the configured shell.
	Command string
	// Dir is where the command runs. An empty Dir is the working directory,
	// and a relative one is taken from it.
	Dir string
	// Timeout is zero for the configured default.
	Timeout time.Duration
	// CancelGrace and TimeoutGrace say how the command is stopped when the
	// context is cancelled and when the timeout passes. Zero kills the
	// process group at once. Otherwise the group is sent SIGTERM, and
	// SIGKILL when the command has not exited by the end of the period.
	// There is no default: the caller passes what its upstream tool does.
	CancelGrace  time.Duration
	TimeoutGrace time.Duration
	// MaxOutputBytes is the most output to keep, when the caller wants less
	// than the configured tools.max_output_bytes. Zero means the configured
	// size. More than the configured size is an error.
	MaxOutputBytes int64
}

// End says how a run ended.
type End string

// Ways a run ends.
const (
	// EndExited: the command ran to its end. ExitCode or Signal is set.
	EndExited End = "exited"
	// EndTimedOut: the timeout passed and the process group was killed.
	EndTimedOut End = "timed_out"
	// EndCancelled: the context was cancelled. The process group was killed
	// if the command had started.
	EndCancelled End = "cancelled"
)

// Result is what a command did.
type Result struct {
	End End
	// Started says the command was running when the run ended, so it may
	// have had an effect. It is false only when the run was cancelled
	// before the command was started.
	Started bool
	// ExitCode is the command's exit status when it exited on its own.
	ExitCode int
	// Signal names the signal that ended the command, when one did.
	Signal string
	// SignalNumber is that signal's number on this system, and zero when
	// no signal ended the command. A shell reports it as 128 plus this.
	SignalNumber int
	// StopSignal names the signal that stopped the command after a cancel
	// or a timeout: "terminated" when it exited within the grace period
	// after SIGTERM, whether it died of the signal or caught it and exited,
	// and "killed" when it was sent SIGKILL. It is empty when the command
	// ended on its own.
	StopSignal string
	// Output is standard output and standard error as the command wrote
	// them, through one pipe so their order is kept. Bytes that are not
	// UTF-8 are replaced with U+FFFD.
	Output string
	// DroppedBytes is how much output was left out between Output and Tail
	// because the whole was over the configured size.
	DroppedBytes int64
	// Tail is the end of the output when DroppedBytes is not zero.
	Tail string
	// PipeHeld says a process the command left behind still held the output
	// open when reading stopped, so later output was not read.
	PipeHeld bool
	// Timeout is the timeout that applied.
	Timeout  time.Duration
	Duration time.Duration
	// ProgressErr is the error that stopped output being sent to the
	// client. The command was not stopped for it.
	ProgressErr error
}

// Outcome is what the loop records for the call. A cancelled command that
// had started may have changed anything, so nothing is known about it.
func (r Result) Outcome() transcript.Outcome {
	switch {
	case r.End == EndCancelled && r.Started:
		return transcript.OutcomeUnknown
	case r.End == EndCancelled:
		return transcript.OutcomeCancelled
	case r.End == EndExited && r.ExitCode == 0 && r.Signal == "":
		return transcript.OutcomeCompleted
	default:
		return transcript.OutcomeFailed
	}
}

// Kind says what is wrong with an Input.
type Kind string

// Kinds of an Error.
const (
	// KindEmptyCommand: there is no command.
	KindEmptyCommand Kind = "empty_command"
	// KindTimeout: the timeout is negative or over Limit.
	KindTimeout Kind = "timeout"
	// KindGrace: a grace period is negative.
	KindGrace Kind = "grace"
	// KindOutputLimit: the output limit is negative or over LimitBytes.
	KindOutputLimit Kind = "output_limit"
)

// Error is an Input that has to be corrected. A Dir that cannot be used is
// an *fsops.Error.
type Error struct {
	Kind Kind
	// Limit is the longest timeout allowed.
	Limit time.Duration
	// LimitBytes is the largest output limit allowed.
	LimitBytes int64
}

func (e *Error) Error() string { return "shell: " + string(e.Kind) }

// Run runs a command in its own process group and waits for it. Output goes
// to env.Progress as it arrives. When the context is cancelled or the
// timeout passes, the whole group is stopped as the Input's grace periods
// say. An error means the command could not be run at all.
func Run(ctx context.Context, env tools.Env, cfg config.Tools, in Input) (Result, error) {
	if strings.TrimSpace(in.Command) == "" {
		return Result{}, &Error{Kind: KindEmptyCommand}
	}
	timeout := in.Timeout
	if timeout == 0 {
		timeout = cfg.Shell.Timeout
	}
	if timeout < 0 || timeout > cfg.Shell.MaxTimeout {
		return Result{}, &Error{Kind: KindTimeout, Limit: cfg.Shell.MaxTimeout}
	}
	if in.CancelGrace < 0 || in.TimeoutGrace < 0 {
		return Result{}, &Error{Kind: KindGrace}
	}
	maxOutput := cfg.MaxOutputBytes
	if in.MaxOutputBytes != 0 {
		if in.MaxOutputBytes < 0 || in.MaxOutputBytes > cfg.MaxOutputBytes {
			return Result{}, &Error{Kind: KindOutputLimit, LimitBytes: cfg.MaxOutputBytes}
		}
		maxOutput = in.MaxOutputBytes
	}
	dir, err := workDir(env, cfg.Files, in.Dir)
	if err != nil {
		return Result{}, err
	}
	res := Result{Timeout: timeout}
	if ctx.Err() != nil {
		res.End = EndCancelled
		return res, nil
	}

	read, write, err := os.Pipe()
	if err != nil {
		return Result{}, fmt.Errorf("shell: %w", err)
	}
	cmd := exec.Command(cfg.Shell.Program, "-c", in.Command)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = write, write
	cmd.SysProcAttr = groupAttr()
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{}, errors.Join(fmt.Errorf("shell: start %s: %w", cfg.Shell.Program, err), read.Close(), write.Close())
	}
	res.Started = true
	// The command holds its own copy. Ours has to go, or the read never
	// ends.
	if err := write.Close(); err != nil {
		return Result{}, errors.Join(fmt.Errorf("shell: %w", err), killGroup(cmd.Process.Pid), cmd.Wait(), read.Close())
	}

	out := &collector{limit: maxOutput, progress: env.Progress}
	drained := make(chan error, 1)
	// The reader keeps sending progress after a cancel, so that the output
	// up to the kill reaches the client.
	go func() { drained <- out.drain(context.WithoutCancel(ctx), read) }()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var waitErr, killErr error
	select {
	case waitErr = <-exited:
		res.End = EndExited
	case <-timer.C:
		res.End = EndTimedOut
		res.StopSignal, waitErr, killErr = stop(cmd.Process.Pid, exited, in.TimeoutGrace)
	case <-ctx.Done():
		res.End = EndCancelled
		res.StopSignal, waitErr, killErr = stop(cmd.Process.Pid, exited, in.CancelGrace)
	}
	res.Duration = time.Since(start)

	// The pipe reaches its end when the last process holding it is gone. A
	// process that left the group, or was left running in the background,
	// can hold it for as long as it likes.
	var readErr error
	wait := time.NewTimer(cfg.Shell.PipeWait)
	defer wait.Stop()
	select {
	case readErr = <-drained:
		readErr = errors.Join(readErr, read.Close())
	case <-wait.C:
		res.PipeHeld = true
		// Closing our end wakes the reader.
		closeErr := read.Close()
		if err := <-drained; !errors.Is(err, os.ErrClosed) {
			readErr = err
		}
		readErr = errors.Join(readErr, closeErr)
	}
	res.Output, res.Tail, res.DroppedBytes = out.result()
	res.ProgressErr = out.progressErr

	var exit *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exit) {
		return Result{}, fmt.Errorf("shell: wait: %w", waitErr)
	}
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		res.Signal = status.Signal().String()
		res.SignalNumber = int(status.Signal())
	}
	res.ExitCode = cmd.ProcessState.ExitCode()
	if killErr != nil {
		return Result{}, fmt.Errorf("shell: kill process group: %w", killErr)
	}
	if readErr != nil {
		return Result{}, fmt.Errorf("shell: read output: %w", readErr)
	}
	return res, nil
}

// stop ends the process group that pid leads and waits for the command. It
// returns the name of the signal that stopped the command.
func stop(pid int, exited <-chan error, grace time.Duration) (signal string, waitErr, killErr error) {
	if grace == 0 {
		killErr = killGroup(pid)
		return syscall.SIGKILL.String(), <-exited, killErr
	}
	if err := terminateGroup(pid); err != nil {
		// The polite signal could not be sent. The group is still killed.
		killErr = errors.Join(err, killGroup(pid))
		return syscall.SIGKILL.String(), <-exited, killErr
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case waitErr = <-exited:
		// The command is gone. What it started may have ignored the signal
		// and is still in the group. By now the command has been waited
		// for, so its process id is free: the kill reaches another group
		// only if the kernel gave the id out again and that process made
		// itself a group leader in the time between.
		return syscall.SIGTERM.String(), waitErr, killGroup(pid)
	case <-timer.C:
		killErr = killGroup(pid)
		return syscall.SIGKILL.String(), <-exited, killErr
	}
}

// workDir returns the directory a command runs in.
func workDir(env tools.Env, cfg config.ToolsFiles, dir string) (string, error) {
	if dir == "" {
		dir = env.Cwd
	}
	resolved, err := fsops.RootsOf(env, cfg).Resolve(dir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", &fsops.Error{Kind: fsops.KindNotFound, Path: dir, Err: err}
	case err != nil:
		return "", fmt.Errorf("shell: %w", err)
	case !info.IsDir():
		return "", &fsops.Error{Kind: fsops.KindNotRegular, Path: dir, Err: errors.New("not a directory")}
	}
	return resolved, nil
}

// collector keeps the start and the end of a command's output and passes
// all of it on as progress.
type collector struct {
	limit       int64
	progress    func(ctx context.Context, content []acp.ToolCallContent) error
	progressErr error

	head  []byte
	tail  []byte
	total int64
	// partial holds the bytes of a character whose end has not arrived.
	partial []byte
}

func (c *collector) drain(ctx context.Context, r io.Reader) error {
	buf := make([]byte, readChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			c.keep(buf[:n])
			c.send(ctx, buf[:n])
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// keep stores the first half of the limit and the newest half.
func (c *collector) keep(p []byte) {
	half := int(c.limit / 2)
	c.total += int64(len(p))
	if room := half - len(c.head); room > 0 {
		n := min(room, len(p))
		c.head = append(c.head, p[:n]...)
		p = p[n:]
	}
	c.tail = append(c.tail, p...)
	// Trimming only at twice the size keeps the copying linear.
	if len(c.tail) > 2*half {
		c.tail = append(c.tail[:0], c.tail[len(c.tail)-half:]...)
	}
}

func (c *collector) result() (output, tail string, dropped int64) {
	half := int(c.limit / 2)
	kept := c.tail[max(len(c.tail)-half, 0):]
	dropped = c.total - int64(len(c.head)+len(kept))
	if dropped == 0 {
		return valid(c.head) + valid(kept), "", 0
	}
	return valid(c.head), valid(kept), dropped
}

func valid(b []byte) string { return strings.ToValidUTF8(string(b), "\uFFFD") }

// send passes output to the client. While the client is slow the pipe
// fills and the next read brings more at once, so a command that writes a
// line at a time does not cost a message a line.
func (c *collector) send(ctx context.Context, p []byte) {
	if c.progress == nil || c.progressErr != nil {
		return
	}
	text := p
	if len(c.partial) > 0 {
		text = slices.Concat(c.partial, p)
		c.partial = nil
	}
	cut := len(text)
	for i := 1; i < utf8.UTFMax && i <= len(text); i++ {
		if tail := text[len(text)-i:]; utf8.RuneStart(tail[0]) {
			if !utf8.FullRune(tail) {
				cut = len(text) - i
			}
			break
		}
	}
	if cut < len(text) {
		c.partial = append([]byte(nil), text[cut:]...)
	}
	if cut == 0 {
		return
	}
	c.progressErr = c.progress(ctx, []acp.ToolCallContent{tools.TextContent(valid(text[:cut]))})
}
