package loop_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/pkg/acp"
	"github.com/gfx-labs/towa/src/agent/hooks"
	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/batch"
	"github.com/gfx-labs/towa/src/transcript"
)

// took is how long each test tool works before it touches its file.
const took = time.Second

// span is when one call ran, on the test's clock.
type span struct{ start, end time.Duration }

// bench is a small tool set that reads and writes real files and records
// when each call ran.
type bench struct {
	dir   string
	began time.Time

	mu      sync.Mutex
	spans   map[string]span
	runs    map[string]int
	updates []acp.SessionToolCallUpdate
}

type benchInput struct {
	Path string `json:"path,omitzero"`
	Text string `json:"text,omitzero"`
}

func newBench(t *testing.T) *bench {
	return &bench{dir: t.TempDir(), began: time.Now(), spans: map[string]span{}, runs: map[string]int{}}
}

func (b *bench) path(name string) string { return filepath.Join(b.dir, name) }

// work waits as a tool that honours its context does, and records the call.
func (b *bench) work(ctx context.Context, id string, do func() (tools.Result, error)) (tools.Result, error) {
	start := time.Since(b.began)
	b.mu.Lock()
	b.runs[id]++
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.spans[id] = span{start, time.Since(b.began)}
		b.mu.Unlock()
	}()
	select {
	case <-time.After(took):
	case <-ctx.Done():
		return tools.Result{}, context.Cause(ctx)
	}
	return do()
}

func (b *bench) set() tools.Set {
	schema := func(required string) jsontext.Value {
		return jsontext.Value(`{"type":"object","properties":{"path":{"type":"string"},"text":{"type":"string"}},"required":["` + required + `"],"additionalProperties":false}`)
	}
	parse := func(input jsontext.Value) (benchInput, error) {
		var in benchInput
		err := json.Unmarshal(input, &in)
		return in, err
	}
	done := func(text string) (tools.Result, error) {
		return tools.Result{Outcome: transcript.OutcomeCompleted, Parts: []transcript.Part{{Text: text}}}, nil
	}
	return tools.Set{Tools: []tools.Tool{
		{
			Spec:   provider.Tool{Name: "read", InputSchema: schema("path")},
			Effect: tools.ReadOnly,
			Describe: func(input jsontext.Value) (tools.Description, error) {
				in, err := parse(input)
				return tools.Description{Title: "Read " + in.Path, Kind: acp.ToolKindRead, Locations: []acp.ToolCallLocation{{Path: in.Path}}}, err
			},
			Run: func(ctx context.Context, call tools.Call) (tools.Result, error) {
				return b.work(ctx, call.ID, func() (tools.Result, error) {
					in, _ := parse(call.Input)
					data, err := os.ReadFile(in.Path)
					if err != nil {
						return tools.Result{Outcome: transcript.OutcomeFailed, Parts: []transcript.Part{{Text: "no such file"}}}, nil
					}
					return done(string(data))
				})
			},
		},
		{
			Spec:   provider.Tool{Name: "write", InputSchema: schema("path")},
			Effect: tools.Writes,
			Describe: func(input jsontext.Value) (tools.Description, error) {
				in, err := parse(input)
				return tools.Description{Title: "Write " + in.Path, Kind: acp.ToolKindEdit, WritePaths: []string{in.Path}}, err
			},
			Run: func(ctx context.Context, call tools.Call) (tools.Result, error) {
				return b.work(ctx, call.ID, func() (tools.Result, error) {
					in, _ := parse(call.Input)
					return appendTo(in.Path, in.Text)
				})
			},
		},
		{
			Spec:   provider.Tool{Name: "exec", InputSchema: schema("text")},
			Effect: tools.Executes,
			Describe: func(jsontext.Value) (tools.Description, error) {
				return tools.Description{Title: "Run", Kind: acp.ToolKindExecute}, nil
			},
			Run: func(ctx context.Context, call tools.Call) (tools.Result, error) {
				if err := call.Env.Progress(ctx, []acp.ToolCallContent{tools.TextContent("one")}); err != nil {
					return tools.Result{}, err
				}
				if err := call.Env.Progress(ctx, []acp.ToolCallContent{tools.TextContent("two")}); err != nil {
					return tools.Result{}, err
				}
				return b.work(ctx, call.ID, func() (tools.Result, error) {
					in, _ := parse(call.Input)
					return appendTo(filepath.Join(call.Env.Cwd, "exec.log"), in.Text)
				})
			},
		},
	}}
}

func appendTo(path, text string) (tools.Result, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return tools.Result{}, err
	}
	if _, err := f.WriteString(text); err != nil {
		return tools.Result{}, errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Outcome: transcript.OutcomeCompleted, Parts: []transcript.Part{{Text: "ok"}}}, nil
}

func (b *bench) dispatcher(set tools.Set) loop.Dispatcher {
	schemas, err := loop.Schemas(set)
	if err != nil {
		panic(err)
	}
	return loop.Dispatcher{
		Tools: set, Schemas: schemas, Repair: true,
		Env: tools.Env{SessionID: "s", Cwd: b.dir},
		Update: func(u acp.SessionToolCallUpdate) {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.updates = append(b.updates, u)
		},
		PutBlob: func(context.Context, string, io.Reader) (transcript.Stored, error) {
			return transcript.Stored{}, errors.New("no test tool stores a blob")
		},
		Logger: slog.New(slog.DiscardHandler),
	}
}

func call(id, name string, in benchInput) transcript.ToolCall {
	raw, err := json.Marshal(in)
	if err != nil {
		panic(err)
	}
	return transcript.ToolCall{ID: id, Name: name, Input: raw}
}

func outcomes(d loop.Dispatched) map[string]transcript.Outcome {
	out := map[string]transcript.Outcome{}
	for _, r := range d.Results {
		out[r.CallID] = r.Outcome
	}
	return out
}

func TestBatchUsesTheTurnsPermissionValidationAndBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newBench(t)
		set := b.set()
		var items []batch.Item
		set.Tools = append(set.Tools, tools.Tool{
			Spec: provider.Tool{Name: "batch", InputSchema: jsontext.Value(`{"type":"object"}`)}, Effect: tools.Executes,
			Describe: func(jsontext.Value) (tools.Description, error) { return tools.Description{Title: "Batch"}, nil },
			Run: func(ctx context.Context, _ tools.Call) (tools.Result, error) {
				dispatch, ok := loop.BatchDispatcher(ctx)
				if !ok {
					return tools.Result{}, errors.New("loop did not supply the batch dispatcher")
				}
				var err error
				items, err = batch.Run(ctx, config.ToolsBatch{MaxCalls: 8}, dispatch, []batch.Call{
					{Name: "write", Input: call("", "", benchInput{Path: b.path("denied"), Text: "no"}).Input},
					{Name: "write", Input: call("", "", benchInput{Path: b.path("x"), Text: "1"}).Input},
					{Name: "read", Input: call("", "", benchInput{Path: b.path("x")}).Input},
					{Name: "write", Input: call("", "", benchInput{Path: b.path("x"), Text: "2"}).Input},
					{Name: "batch", Input: jsontext.Value(`{}`)},
					{Name: "write", Input: jsontext.Value(`{"path":3}`)},
					{Name: "write", Input: call("", "", benchInput{Path: b.path("limit"), Text: "no"}).Input},
				})
				return tools.Result{Outcome: transcript.OutcomeCompleted}, err
			},
		})
		d := b.dispatcher(set)
		d.Limited, d.Left = true, 4
		d.Permit = func(_ context.Context, _ string, _ tools.Effect, update acp.ToolCallUpdate) (bool, error) {
			return update.Title == nil || *update.Title != "Write "+b.path("denied"), nil
		}
		done := d.Run(t.Context(), []transcript.ToolCall{{ID: "b", Name: "batch", Input: jsontext.Value(`{}`)}})
		require.Len(t, items, 7)
		assert.Equal(t, "1", items[2].Result.Parts[0].Text)
		data, err := os.ReadFile(b.path("x"))
		require.NoError(t, err)
		assert.Equal(t, "12", string(data))
		for _, i := range []int{0, 4, 5, 6} {
			assert.False(t, items[i].Started)
			assert.Error(t, items[i].Err)
		}
		assert.EqualValues(t, 4, done.Ran)
		assert.True(t, done.OverLimit)
	})
}

func TestCallsRunTogetherUnlessTheyConflict(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newBench(t)
		require.NoError(t, os.WriteFile(b.path("a"), []byte("A"), 0o600))
		require.NoError(t, os.WriteFile(b.path("b"), []byte("B"), 0o600))
		done := b.dispatcher(b.set()).Run(t.Context(), []transcript.ToolCall{
			call("r1", "read", benchInput{Path: b.path("a")}),
			call("r2", "read", benchInput{Path: b.path("b")}),
			call("w1", "write", benchInput{Path: b.path("x"), Text: "1"}),
			call("w2", "write", benchInput{Path: b.path("x"), Text: "2"}),
			call("w3", "write", benchInput{Path: b.path("y"), Text: "3"}),
			call("e1", "exec", benchInput{Text: "e"}),
			call("r3", "read", benchInput{Path: b.path("x")}),
		})

		assert.Equal(t, map[string]span{
			// Reads and writes of different paths start at once.
			"r1": {0, took}, "r2": {0, took}, "w1": {0, took}, "w3": {0, took},
			// The second write of x waits for the first.
			"w2": {took, 2 * took},
			// The command waits for everything before it and runs alone.
			"e1": {2 * took, 3 * took},
			"r3": {3 * took, 4 * took},
		}, b.spans)
		x, err := os.ReadFile(b.path("x"))
		require.NoError(t, err)
		assert.Equal(t, "12", string(x), "the writes of one path landed in the model's order")
		assert.EqualValues(t, 7, done.Ran)
		for id, outcome := range outcomes(done) {
			assert.Equal(t, transcript.OutcomeCompleted, outcome, id)
		}
		assert.Equal(t, "12", done.Results[6].Parts[0].Text, "the read after the writes saw both")

		// A tool's progress is sent accumulated, since an update's content
		// replaces what the client has.
		var contents []int
		for _, u := range b.updates {
			if u.ToolCallId == "e1" && u.Status == nil {
				contents = append(contents, len(u.Content))
			}
		}
		assert.Equal(t, []int{1, 2}, contents)
	})
}

func TestEveryCallHasAResultAfterACancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newBench(t)
		require.NoError(t, os.WriteFile(b.path("a"), []byte("A"), 0o600))
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(took/2, cancel)
		done := b.dispatcher(b.set()).Run(ctx, []transcript.ToolCall{
			call("r1", "read", benchInput{Path: b.path("a")}),
			call("w1", "write", benchInput{Path: b.path("x"), Text: "1"}),
			call("w2", "write", benchInput{Path: b.path("x"), Text: "2"}),
			call("e1", "exec", benchInput{Text: "e"}),
		})

		assert.Equal(t, map[string]transcript.Outcome{
			// Started and only reads: it cannot have changed anything.
			"r1": transcript.OutcomeCancelled,
			// Started, and it did not say whether it wrote.
			"w1": transcript.OutcomeUnknown,
			// Never started.
			"w2": transcript.OutcomeCancelled,
			"e1": transcript.OutcomeCancelled,
		}, outcomes(done))
		assert.Equal(t, map[string]int{"r1": 1, "w1": 1}, b.runs, "a call is handed to its tool at most once, and not after the cancel")
		assert.NoFileExists(t, b.path("x"))
		assert.NoFileExists(t, b.path("exec.log"))

		// Every call's last update ends it.
		last := map[acp.ToolCallId]acp.ToolCallStatus{}
		for _, u := range b.updates {
			if u.Status != nil {
				last[u.ToolCallId] = *u.Status
			}
		}
		assert.Equal(t, map[acp.ToolCallId]acp.ToolCallStatus{
			"r1": acp.ToolCallStatusFailed, "w1": acp.ToolCallStatusFailed,
			"w2": acp.ToolCallStatusFailed, "e1": acp.ToolCallStatusFailed,
		}, last)
	})
}

func TestAToolThatFinishesDespiteACancelKeepsItsOutcome(t *testing.T) {
	b := newBench(t)
	ctx, cancel := context.WithCancel(t.Context())
	set := tools.Set{Tools: []tools.Tool{{
		Spec:     provider.Tool{Name: "stubborn"},
		Effect:   tools.Writes,
		Describe: func(jsontext.Value) (tools.Description, error) { return tools.Description{Title: "Write"}, nil },
		Run: func(context.Context, tools.Call) (tools.Result, error) {
			cancel()
			return appendTo(b.path("x"), "written")
		},
	}}}
	done := b.dispatcher(set).Run(ctx, []transcript.ToolCall{{ID: "c", Name: "stubborn", Input: jsontext.Value(`{}`)}})
	assert.Equal(t, transcript.OutcomeCompleted, done.Results[0].Outcome)
	assert.FileExists(t, b.path("x"))
}

func TestCallsThatDoNotRunStillGetAResult(t *testing.T) {
	b := newBench(t)
	d := b.dispatcher(b.set())
	d.Left, d.Limited, d.LimitKey = 2, true, config.KeyMaxToolCalls
	d.Permit = func(_ context.Context, _ string, _ tools.Effect, c acp.ToolCallUpdate) (bool, error) {
		return c.ToolCallId != "rejected", nil
	}
	done := d.Run(t.Context(), []transcript.ToolCall{
		call("nosuch", "delete_everything", benchInput{}),
		{ID: "badinput", Name: "write", Input: jsontext.Value(`{"text":"no path"}`)},
		call("rejected", "write", benchInput{Path: b.path("r"), Text: "r"}),
		call("w1", "write", benchInput{Path: b.path("x"), Text: "1"}),
		// The model wrote the object as a string, which repair undoes.
		{ID: "repaired", Name: "write", Input: jsontext.Value(`"{\"path\":\"` + b.path("y") + `\",\"text\":\"2\"}"`)},
		call("over", "write", benchInput{Path: b.path("z"), Text: "3"}),
	})

	assert.Equal(t, map[string]transcript.Outcome{
		"nosuch":   transcript.OutcomeFailed,
		"badinput": transcript.OutcomeFailed,
		"rejected": transcript.OutcomeCancelled,
		"w1":       transcript.OutcomeCompleted,
		"repaired": transcript.OutcomeCompleted,
		"over":     transcript.OutcomeCancelled,
	}, outcomes(done))
	assert.EqualValues(t, 2, done.Ran, "calls that were refused do not count against the limit")
	assert.True(t, done.OverLimit)
	assert.Contains(t, done.Results[5].Parts[0].Text, config.KeyMaxToolCalls)
	assert.NoFileExists(t, b.path("r"))
	assert.NoFileExists(t, b.path("z"))
	y, err := os.ReadFile(b.path("y"))
	require.NoError(t, err)
	assert.Equal(t, "2", string(y))
}

func TestAFailedPermissionRequestStopsTheBatch(t *testing.T) {
	b := newBench(t)
	d := b.dispatcher(b.set())
	failure := errors.New("client went away")
	d.Permit = func(_ context.Context, _ string, _ tools.Effect, c acp.ToolCallUpdate) (bool, error) {
		if c.ToolCallId == "w2" {
			return false, failure
		}
		return true, nil
	}
	done := d.Run(t.Context(), []transcript.ToolCall{
		call("w1", "write", benchInput{Path: b.path("x"), Text: "1"}),
		call("w2", "write", benchInput{Path: b.path("y"), Text: "2"}),
	})
	require.ErrorIs(t, done.Err, failure)
	assert.Equal(t, map[string]transcript.Outcome{
		"w1": transcript.OutcomeCancelled, "w2": transcript.OutcomeCancelled,
	}, outcomes(done))
	assert.Empty(t, b.runs, "the call allowed before the failure did not start")
}

func TestToolHooksDenyAndChangeACall(t *testing.T) {
	b := newBench(t)
	answer := func(event, tool, json string) config.Hook {
		return config.Hook{Event: event, Tools: []string{tool}, Command: []string{"/bin/sh", "-c", "cat >/dev/null; printf '%s' '" + json + "'"}, Timeout: 10 * time.Second}
	}
	d := b.dispatcher(b.set())
	d.Hooks = hooks.New([]config.Hook{
		answer(config.HookPreTool, "exec", `{"decision":"deny","reason":"no commands here"}`),
		// The changed input has no path, which the schema requires.
		answer(config.HookPreTool, "write", `{"decision":"allow","input":{"text":"hooked"}}`),
		answer(config.HookPostTool, "read", `{"decision":"deny","reason":"this file is private"}`),
	})
	require.NoError(t, os.WriteFile(b.path("a"), []byte("A"), 0o600))
	done := d.Run(t.Context(), []transcript.ToolCall{
		call("e1", "exec", benchInput{Text: "e"}),
		call("w1", "write", benchInput{Path: b.path("x"), Text: "1"}),
		call("r1", "read", benchInput{Path: b.path("a")}),
	})

	for _, r := range done.Results {
		assert.Equal(t, transcript.OutcomeFailed, r.Outcome, r.CallID)
	}
	assert.Equal(t, "no commands here", done.Results[0].Parts[0].Text)
	assert.Equal(t, "this file is private", done.Results[2].Parts[0].Text, "the result the tool returned was replaced")
	assert.Equal(t, map[string]int{"r1": 1}, b.runs, "a denied call and a call whose changed input was refused did not run")
	assert.NoFileExists(t, b.path("x"), "the model's input was not run in place of the hook's")
}
