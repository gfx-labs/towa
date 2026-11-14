// Package jobs runs a session's background commands. Each writes its
// output to a file in a directory of the session's own under the system's
// temporary directory ($TMPDIR, or /tmp), so a command's output is on disk
// and not in memory, and the model is given its absolute path.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/shell"
	"github.com/gfx-labs/towa/src/transcript"
)

// Render turns how a command ended into what the model and the client are
// told about it.
type Render func(job *Job, run shell.Result, err error) (transcript.Outcome, string)

// Job is one background command.
type Job struct {
	// ID is the name the model uses for the job.
	ID string
	// CallID is the tool call that started it.
	CallID string
	// Label is the call's description, or its command when it has none.
	Label string
	// Path is the absolute path of the file the output is written to.
	Path string

	cancel  context.CancelFunc
	done    chan struct{}
	outcome transcript.Outcome
	report  string
	// reported is guarded by the Set's mutex.
	reported bool
}

// Done is closed when the command has ended and its report is ready.
func (j *Job) Done() <-chan struct{} { return j.done }

// Report is how the command ended. It is valid once Done is closed.
func (j *Job) Report() (transcript.Outcome, string) { return j.outcome, j.report }

// Stop stops the command and waits for it to end.
func (j *Job) Stop() {
	j.cancel()
	<-j.done
}

// Tail returns at most the last max bytes of the output written so far,
// with bytes that are not UTF-8 replaced, and the size of the whole.
func (j *Job) Tail(maxBytes int64) (tail string, size int64, err error) {
	f, err := os.Open(j.Path)
	if err != nil {
		return "", 0, fmt.Errorf("jobs: %w", err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return "", 0, fmt.Errorf("jobs: %w", err)
	}
	size = info.Size()
	start := max(size-maxBytes, 0)
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return "", 0, fmt.Errorf("jobs: %w", err)
	}
	return strings.ToValidUTF8(string(buf), "\uFFFD"), size, nil
}

// Set is the background commands of one session.
type Set struct {
	mu   sync.Mutex
	dir  string
	n    int
	list []*Job
	// ended is signalled when a job ends, so that Next wakes.
	ended  chan struct{}
	closed bool
}

// NewSet returns a set with no jobs. Its directory is made when the first
// job starts.
func NewSet() *Set { return &Set{ended: make(chan struct{}, 1)} }

// ErrClosed is a job started after the session closed.
var ErrClosed = errors.New("jobs: the session is closed")

// Start runs a command in the background. ctx gives the values the command
// runs with: it is not cancelled with ctx, only by Stop, StopAll and Close.
func (s *Set) Start(ctx context.Context, env tools.Env, cfg config.Tools, callID, label string, in shell.Input, render Render) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if s.dir == "" {
		dir, err := os.MkdirTemp("", "towa-"+string(env.SessionID)+"-")
		if err != nil {
			return nil, fmt.Errorf("jobs: %w", err)
		}
		if dir, err = filepath.Abs(dir); err != nil {
			return nil, errors.Join(fmt.Errorf("jobs: %w", err), os.RemoveAll(dir))
		}
		s.dir = dir
	}
	s.n++
	id := fmt.Sprintf("bg%d", s.n)
	path := filepath.Join(s.dir, id+".out")
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	j := &Job{ID: id, CallID: callID, Label: label, Path: path, cancel: cancel, done: make(chan struct{})}
	s.list = append(s.list, j)
	in.Output = out
	env.Progress = nil
	go func() {
		defer cancel()
		run, err := shell.Run(runCtx, env, cfg, in)
		if closeErr := out.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("jobs: closing the output file: %w", closeErr)
		}
		j.outcome, j.report = render(j, run, err)
		close(j.done)
		select {
		case s.ended <- struct{}{}:
		default:
		}
	}()
	return j, nil
}

// Get returns the job the model names.
func (s *Set) Get(id string) (*Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.list {
		if j.ID == id {
			return j, true
		}
	}
	return nil, false
}

// IDs returns the ids of all jobs, in the order they started.
func (s *Set) IDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, len(s.list))
	for i, j := range s.list {
		ids[i] = j.ID
	}
	return ids
}

// Unreported reports whether a job ended that the model was not told about.
func (s *Set) Unreported() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.list {
		if !j.reported && ended(j) {
			return true
		}
	}
	return false
}

// MarkReported records that the model was given how j ended, so Take does
// not give it again.
func (s *Set) MarkReported(j *Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j.reported = true
}

// Take returns the jobs that ended and were not reported, in the order
// they started, and marks them reported.
func (s *Set) Take() []*Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Job
	for _, j := range s.list {
		if !j.reported && ended(j) {
			j.reported = true
			out = append(out, j)
		}
	}
	return out
}

// Next waits until a job ended that was not reported, or no job runs.
func (s *Set) Next(ctx context.Context) error {
	for {
		if s.ready() {
			return nil
		}
		select {
		case <-s.ended:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

func (s *Set) ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	running := false
	for _, j := range s.list {
		switch {
		case !ended(j):
			running = true
		case !j.reported:
			return true
		}
	}
	return !running
}

// StopAll stops every job that runs and waits for them to end.
func (s *Set) StopAll() {
	s.mu.Lock()
	list := append([]*Job(nil), s.list...)
	s.mu.Unlock()
	for _, j := range list {
		j.cancel()
	}
	for _, j := range list {
		<-j.done
	}
}

// Close stops every job and removes the session's output directory. No job
// starts after it.
func (s *Set) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.StopAll()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == "" {
		return nil
	}
	if err := os.RemoveAll(s.dir); err != nil {
		return fmt.Errorf("jobs: %w", err)
	}
	return nil
}

func ended(j *Job) bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}
