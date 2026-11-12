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
// does through the tools.
type session struct {
	cfg config.Tools
	env tools.Env
}

type arg struct {
	Path    string `json:"path"`
	Command string `json:"command"`
}

func newSession(t *testing.T) *session {
	t.Helper()
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return &session{cfg: loaded.Config.Tools, env: tools.Env{Cwd: dir}}
}

func (s *session) dispatcher() batch.Dispatcher {
	roots := fsops.RootsOf(s.env, s.cfg.Files)
	return batch.Dispatcher{
		Plan: func(_ int, call batch.Call) (batch.Plan, error) {
			switch call.Name {
			case "read":
				return batch.Plan{Effect: tools.ReadOnly}, nil
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
				res, err := fsops.Read(ctx, roots, s.cfg.Files, fsops.ReadInput{Path: a.Path})
				if err != nil {
					return tools.Result{}, err
				}
				return tools.Result{Outcome: transcript.OutcomeCompleted, Parts: []transcript.Part{{Text: res.Text}}}, nil
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

// The scheduling order is the loop's rule and is tested there
// (TestConflicts, TestCallsRunTogetherUnlessTheyConflict). These tests cover
// what Run adds: a failed call, a cancel and the limits.
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
