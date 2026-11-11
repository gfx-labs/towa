package batch_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/batch"
	"github.com/gfx-labs/acpharness/src/tools/fsops"
	"github.com/gfx-labs/acpharness/src/tools/shell"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// session dispatches batch calls to the real capabilities, the way the loop
// will through the dialect adapters.
type session struct {
	cfg config.Tools
	env tools.Env
	// meet is how many read calls must be running at once before any of
	// them returns.
	meet    int
	arrived chan struct{}
}

type arg struct {
	Path    string `json:"path"`
	Old     string `json:"old"`
	New     string `json:"new"`
	Command string `json:"command"`
}

func newSession(t *testing.T) *session {
	t.Helper()
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return &session{cfg: loaded.Config.Tools, env: tools.Env{Cwd: dir}, arrived: make(chan struct{}, 16)}
}

func (s *session) dispatcher() batch.Dispatcher {
	roots := fsops.RootsOf(s.env, s.cfg.Files)
	return batch.Dispatcher{
		Plan: func(_ int, call batch.Call) (batch.Plan, error) {
			var a arg
			if err := json.Unmarshal(call.Input, &a); err != nil {
				return batch.Plan{}, err
			}
			switch call.Name {
			case "read":
				return batch.Plan{Effect: tools.ReadOnly}, nil
			case "edit":
				path, err := roots.Resolve(a.Path)
				return batch.Plan{Effect: tools.Writes, WritePaths: []string{path}}, err
			case "shell":
				return batch.Plan{Effect: tools.Executes}, nil
			default:
				return batch.Plan{}, fmt.Errorf("no tool named %q", call.Name)
			}
		},
		Run: func(ctx context.Context, _ int, call batch.Call) (tools.Result, error) {
			var a arg
			if err := json.Unmarshal(call.Input, &a); err != nil {
				return tools.Result{}, err
			}
			switch call.Name {
			case "read":
				s.arrived <- struct{}{}
				for len(s.arrived) < s.meet {
					select {
					case <-ctx.Done():
						return tools.Result{}, ctx.Err()
					case <-time.After(time.Millisecond):
					}
				}
				res, err := fsops.Read(ctx, roots, s.cfg.Files, fsops.ReadInput{Path: a.Path})
				if err != nil {
					return tools.Result{}, err
				}
				return tools.Result{Outcome: transcript.OutcomeCompleted, Parts: []transcript.Part{{Text: res.Text}}}, nil
			case "edit":
				_, err := fsops.ApplyEdits(ctx, roots, s.cfg.Files, fsops.EditInput{Path: a.Path, Edits: []fsops.Edit{{Old: a.Old, New: a.New}}})
				if err != nil {
					return tools.Result{}, err
				}
				return tools.Result{Outcome: transcript.OutcomeCompleted}, nil
			default:
				res, err := shell.Run(ctx, s.env, s.cfg, shell.Input{Command: a.Command})
				if err != nil {
					return tools.Result{}, err
				}
				return tools.Result{Outcome: res.Outcome(), Parts: []transcript.Part{{Text: res.Output}}}, nil
			}
		},
	}
}

func call(name string, a arg) batch.Call {
	input, err := json.Marshal(a)
	if err != nil {
		panic(err)
	}
	return batch.Call{Name: name, Input: jsontext.Value(input)}
}

func (s *session) put(t *testing.T, name, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(s.env.Cwd, name), []byte(body), 0o644))
}

func (s *session) get(t *testing.T, name string) string {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(s.env.Cwd, name))
	require.NoError(t, err)
	return string(got)
}

// Each read waits until all three are running. Run one after the other,
// they would wait until the deadline.
func TestReadOnlyCallsRunAtTheSameTime(t *testing.T) {
	s := newSession(t)
	s.meet = 3
	s.put(t, "a.txt", "a\n")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	items, err := batch.Run(ctx, s.cfg.Batch, s.dispatcher(), []batch.Call{
		call("read", arg{Path: "a.txt"}), call("read", arg{Path: "a.txt"}), call("read", arg{Path: "a.txt"}),
	})
	require.NoError(t, err)
	for _, item := range items {
		require.NoError(t, item.Err)
		require.Equal(t, "a\n", item.Result.Parts[0].Text)
	}
	require.Equal(t, transcript.OutcomeCompleted, batch.Outcome(items))
}

// The edits to one file build on each other, so they only all succeed when
// they run in the order given. Run many times, any other order would show.
func TestWritesToOnePathRunInOrder(t *testing.T) {
	s := newSession(t)
	for range 20 {
		s.put(t, "chain.txt", "v0\n")
		s.put(t, "other.txt", "x\n")
		items, err := batch.Run(context.Background(), s.cfg.Batch, s.dispatcher(), []batch.Call{
			call("edit", arg{Path: "chain.txt", Old: "v0", New: "v1"}),
			call("edit", arg{Path: "other.txt", Old: "x", New: "y"}),
			call("edit", arg{Path: "chain.txt", Old: "v1", New: "v2"}),
			call("edit", arg{Path: "./chain.txt", Old: "v2", New: "v3"}),
		})
		require.NoError(t, err)
		for i, item := range items {
			require.NoError(t, item.Err, i)
		}
		require.Equal(t, "v3\n", s.get(t, "chain.txt"))
		require.Equal(t, "y\n", s.get(t, "other.txt"))
	}
}

// Each command fails if another is running, and the edit between them only
// applies once the first command has written the file it changes.
func TestACommandRunsAloneAndInItsPlace(t *testing.T) {
	s := newSession(t)
	alone := "mkdir lock && sleep 0.05 && rmdir lock"
	items, err := batch.Run(context.Background(), s.cfg.Batch, s.dispatcher(), []batch.Call{
		call("shell", arg{Command: alone + " && echo made > made.txt"}),
		call("edit", arg{Path: "made.txt", Old: "made", New: "edited"}),
		call("shell", arg{Command: alone + " && cat made.txt"}),
		call("shell", arg{Command: alone}),
	})
	require.NoError(t, err)
	for i, item := range items {
		require.NoError(t, item.Err, i)
		require.Equal(t, transcript.OutcomeCompleted, item.Result.Outcome, i)
	}
	require.Equal(t, "edited\n", items[2].Result.Parts[0].Text)
}

func TestOneFailedCallDoesNotStopTheOthers(t *testing.T) {
	s := newSession(t)
	s.put(t, "a.txt", "a\n")
	items, err := batch.Run(context.Background(), s.cfg.Batch, s.dispatcher(), []batch.Call{
		call("nosuchtool", arg{}),
		call("read", arg{Path: "absent.txt"}),
		call("shell", arg{Command: "exit 2"}),
		call("read", arg{Path: "a.txt"}),
	})
	require.NoError(t, err)
	require.Error(t, items[0].Err)
	require.False(t, items[0].Started)
	var missing *fsops.Error
	require.ErrorAs(t, items[1].Err, &missing)
	require.Equal(t, transcript.OutcomeFailed, items[1].Result.Outcome)
	require.NoError(t, items[2].Err)
	require.Equal(t, transcript.OutcomeFailed, items[2].Result.Outcome)
	require.Equal(t, "a\n", items[3].Result.Parts[0].Text)
	require.Equal(t, transcript.OutcomeFailed, batch.Outcome(items))
}

// The first command is killed by the cancel, so nothing is known about it.
// The second never starts, so it is known not to have run.
func TestCancelStopsCallsThatHaveNotStarted(t *testing.T) {
	s := newSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			if _, err := os.Stat(filepath.Join(s.env.Cwd, "started")); err == nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	items, err := batch.Run(ctx, s.cfg.Batch, s.dispatcher(), []batch.Call{
		call("shell", arg{Command: "touch started; sleep 300"}),
		call("shell", arg{Command: "touch second"}),
	})
	require.NoError(t, err)
	require.True(t, items[0].Started)
	require.Equal(t, transcript.OutcomeUnknown, items[0].Result.Outcome)
	require.False(t, items[1].Started)
	require.Equal(t, transcript.OutcomeCancelled, items[1].Result.Outcome)
	require.NoFileExists(t, filepath.Join(s.env.Cwd, "second"))
	require.Equal(t, transcript.OutcomeUnknown, batch.Outcome(items))
}

func TestABatchOutsideTheLimitsRunsNothing(t *testing.T) {
	s := newSession(t)
	s.cfg.Batch.MaxCalls = 2
	touch := call("shell", arg{Command: "touch ran"})
	var berr *batch.Error

	_, err := batch.Run(context.Background(), s.cfg.Batch, s.dispatcher(), nil)
	require.ErrorAs(t, err, &berr)
	require.Equal(t, batch.KindEmpty, berr.Kind)

	_, err = batch.Run(context.Background(), s.cfg.Batch, s.dispatcher(), []batch.Call{touch, touch, touch})
	require.ErrorAs(t, err, &berr)
	require.Equal(t, batch.KindTooMany, berr.Kind)
	require.Equal(t, 2, berr.Limit)
	require.NoFileExists(t, filepath.Join(s.env.Cwd, "ran"))
}
