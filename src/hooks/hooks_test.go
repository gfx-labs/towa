package hooks_test

import (
	"context"
	"encoding/json/jsontext"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/hooks"
	"github.com/gfx-labs/towa/src/transcript"
)

// long is a timeout no passing test reaches.
const long = time.Minute

// script writes an executable shell script and returns its path.
func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700))
	return path
}

func session(t *testing.T) hooks.Session {
	t.Helper()
	return hooks.Session{ID: "sess-1", Cwd: t.TempDir()}
}

func hook(event string, command ...string) config.Hook {
	return config.Hook{Event: event, Command: command, Timeout: long}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func TestEachEventSendsItsJSONAndRunsInTheSessionDirectory(t *testing.T) {
	dir := t.TempDir()
	s := session(t)
	// The script keeps what it was sent and where it ran.
	record := script(t, dir, "record", `cat > "$1"; pwd > "$1.pwd"; echo '{"decision":"allow"}'`)
	input := jsontext.Value(`{"command":"ls"}`)
	parts := []transcript.Part{{Text: "hello"}}
	out := func(name string) string { return filepath.Join(dir, name+".json") }

	r := hooks.New([]config.Hook{
		hook(config.HookPreTool, record, out("pre")),
		hook(config.HookPostTool, record, out("post")),
		hook(config.HookPrompt, record, out("prompt")),
		hook(config.HookStop, record, out("stop")),
	})

	d, err := r.PreTool(t.Context(), s, "call-1", "Bash", input)
	require.NoError(t, err)
	require.Equal(t, hooks.Decision{Action: hooks.Allow, Input: input}, d)
	d, err = r.PostTool(t.Context(), s, "call-1", "Bash", input, transcript.OutcomeFailed, parts)
	require.NoError(t, err)
	require.Equal(t, hooks.Decision{Action: hooks.Allow}, d)
	d, err = r.Prompt(t.Context(), s, parts)
	require.NoError(t, err)
	require.Equal(t, hooks.Decision{Action: hooks.Allow}, d)
	d, err = r.Stop(t.Context(), s, "end_turn")
	require.NoError(t, err)
	require.Equal(t, hooks.Decision{Action: hooks.Allow}, d)

	head := `"session_id":"sess-1","cwd":` + strconv.Quote(s.Cwd)
	tool := `"tool":{"name":"Bash","call_id":"call-1","input":{"command":"ls"}}`
	require.JSONEq(t, `{"event":"pre_tool",`+head+`,`+tool+`}`, readFile(t, out("pre")))
	require.JSONEq(t, `{"event":"post_tool",`+head+`,`+tool+`,"result":{"outcome":"failed","parts":[{"text":"hello"}]}}`,
		readFile(t, out("post")))
	require.JSONEq(t, `{"event":"prompt",`+head+`,"prompt":[{"text":"hello"}]}`, readFile(t, out("prompt")))
	require.JSONEq(t, `{"event":"stop",`+head+`,"stop_reason":"end_turn"}`, readFile(t, out("stop")))

	ran, err := filepath.EvalSymlinks(strings.TrimSpace(readFile(t, out("pre")+".pwd")))
	require.NoError(t, err)
	want, err := filepath.EvalSymlinks(s.Cwd)
	require.NoError(t, err)
	require.Equal(t, want, ran)
}

func TestOnlyHooksForTheEventAndToolRun(t *testing.T) {
	dir := t.TempDir()
	mark := script(t, dir, "mark", `cat > /dev/null; echo ran >> "$1"; echo '{"decision":"allow"}'`)
	log := filepath.Join(dir, "log")
	path := func(name string) string { return filepath.Join(dir, name) }

	bash := hook(config.HookPreTool, mark, path("bash"))
	bash.Tools = []string{"Bash", "shell"}
	edit := hook(config.HookPreTool, mark, path("edit"))
	edit.Tools = []string{"Edit"}
	r := hooks.New([]config.Hook{
		bash, edit,
		hook(config.HookPreTool, mark, path("any")),
		hook(config.HookPostTool, mark, path("post")),
		hook(config.HookPrompt, mark, log),
	})

	_, err := r.PreTool(t.Context(), session(t), "c", "shell", jsontext.Value(`{}`))
	require.NoError(t, err)
	require.FileExists(t, path("bash"))
	require.FileExists(t, path("any"))
	require.NoFileExists(t, path("edit"))
	require.NoFileExists(t, path("post"))
	require.NoFileExists(t, log)

	// No hook is configured for stop: that is an allow with nothing run.
	d, err := r.Stop(t.Context(), session(t), "end_turn")
	require.NoError(t, err)
	require.Equal(t, hooks.Allow, d.Action)
}

func TestADenyStopsTheHooksAfterIt(t *testing.T) {
	dir := t.TempDir()
	deny := script(t, dir, "deny", `cat > /dev/null; echo '{"decision":"deny","reason":"rm is not allowed here"}'`)
	mark := script(t, dir, "mark", `cat > /dev/null; echo ran > "$1"; echo '{"decision":"allow"}'`)
	later := filepath.Join(dir, "later")
	r := hooks.New([]config.Hook{hook(config.HookPreTool, deny), hook(config.HookPreTool, mark, later)})

	d, err := r.PreTool(t.Context(), session(t), "c", "Bash", jsontext.Value(`{"command":"rm -rf /"}`))
	require.NoError(t, err)
	require.Equal(t, hooks.Decision{Action: hooks.Deny, Reason: "rm is not allowed here"}, d)
	require.NoFileExists(t, later)
}

func TestAChangedInputReachesTheNextHookAndTheCaller(t *testing.T) {
	dir := t.TempDir()
	change := script(t, dir, "change", `cat > /dev/null; echo '{"decision":"allow","input":{"command":"ls -la"}}'`)
	record := script(t, dir, "record", `cat > "$1"; echo '{"decision":"allow"}'`)
	seen := filepath.Join(dir, "seen.json")
	r := hooks.New([]config.Hook{hook(config.HookPreTool, change), hook(config.HookPreTool, record, seen)})

	d, err := r.PreTool(t.Context(), session(t), "c", "Bash", jsontext.Value(`{"command":"ls"}`))
	require.NoError(t, err)
	require.Equal(t, hooks.Allow, d.Action)
	require.JSONEq(t, `{"command":"ls -la"}`, string(d.Input))
	require.Contains(t, readFile(t, seen), `"input":{"command":"ls -la"}`)
}

// Every way a hook can go wrong denies, and says which way it was.
func TestAFailedHookDeniesAndNeverAllows(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		body string
		kind hooks.Kind
	}{
		// The answer on stdout says allow. The exit status wins.
		{"non-zero exit with an allow printed", `echo '{"decision":"allow"}'; echo broken >&2; exit 3`, hooks.KindExit},
		{"killed by a signal", `kill -TERM $$`, hooks.KindExit},
		{"no output", `cat > /dev/null`, hooks.KindOutput},
		{"not JSON", `echo allow`, hooks.KindOutput},
		{"two answers", `echo '{"decision":"allow"}{"decision":"allow"}'`, hooks.KindOutput},
		{"unknown decision", `echo '{"decision":"approve"}'`, hooks.KindOutput},
		{"no decision", `echo '{}'`, hooks.KindOutput},
		{"unknown member", `echo '{"decision":"allow","continue":true}'`, hooks.KindOutput},
		{"deny without a reason", `echo '{"decision":"deny"}'`, hooks.KindOutput},
		{"deny with an input", `echo '{"decision":"deny","reason":"no","input":{}}'`, hooks.KindOutput},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := hooks.New([]config.Hook{hook(config.HookPreTool, script(t, dir, "s"+strconv.Itoa(i), c.body))})
			d, err := r.PreTool(t.Context(), session(t), "c", "Bash", jsontext.Value(`{}`))
			var herr *hooks.Error
			require.ErrorAs(t, err, &herr)
			require.Equal(t, c.kind, herr.Kind)
			require.Equal(t, hooks.Deny, d.Action)
			require.Equal(t, herr.Error(), d.Reason)
			require.Nil(t, d.Input)
		})
	}

	t.Run("exit status and stderr are reported", func(t *testing.T) {
		r := hooks.New([]config.Hook{hook(config.HookStop, script(t, dir, "exit3", `echo broken >&2; exit 3`))})
		_, err := r.Stop(t.Context(), session(t), "end_turn")
		var herr *hooks.Error
		require.ErrorAs(t, err, &herr)
		require.Equal(t, 3, herr.ExitCode)
		require.Equal(t, "broken\n", herr.Stderr)
		require.Equal(t, 0, herr.Index)
		require.Equal(t, config.HookStop, herr.Event)
	})

	t.Run("a command that does not exist", func(t *testing.T) {
		r := hooks.New([]config.Hook{hook(config.HookPrompt, filepath.Join(dir, "no-such-command"))})
		d, err := r.Prompt(t.Context(), session(t), []transcript.Part{{Text: "hi"}})
		var herr *hooks.Error
		require.ErrorAs(t, err, &herr)
		require.Equal(t, hooks.KindStart, herr.Kind)
		require.ErrorIs(t, err, os.ErrNotExist)
		require.Equal(t, hooks.Deny, d.Action)
	})

	t.Run("an input from a hook that is not pre_tool", func(t *testing.T) {
		change := script(t, dir, "change", `echo '{"decision":"allow","input":{}}'`)
		r := hooks.New([]config.Hook{hook(config.HookPostTool, change)})
		d, err := r.PostTool(t.Context(), session(t), "c", "Bash", jsontext.Value(`{}`), transcript.OutcomeCompleted, nil)
		var herr *hooks.Error
		require.ErrorAs(t, err, &herr)
		require.Equal(t, hooks.KindOutput, herr.Kind)
		require.Equal(t, hooks.Deny, d.Action)
	})

	t.Run("output over the limit", func(t *testing.T) {
		// A valid answer padded with spaces past the limit: cutting it
		// off would leave something that still parses.
		big := script(t, dir, "big", `printf '{"decision":"allow"}'; printf '%4096s' ''`)
		h := hook(config.HookPreTool, big)
		h.MaxOutputBytes = 64
		d, err := hooks.New([]config.Hook{h}).PreTool(t.Context(), session(t), "c", "Bash", jsontext.Value(`{}`))
		var herr *hooks.Error
		require.ErrorAs(t, err, &herr)
		require.Equal(t, hooks.KindOutput, herr.Kind)
		require.Equal(t, hooks.Deny, d.Action)

		h.MaxOutputBytes = 8192
		d, err = hooks.New([]config.Hook{h}).PreTool(t.Context(), session(t), "c", "Bash", jsontext.Value(`{}`))
		require.NoError(t, err)
		require.Equal(t, hooks.Allow, d.Action)
	})
}

// alive reports whether the process exists.
func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// waitForPID reads the pid a script wrote, once it has been written.
func waitForPID(t *testing.T, path string) int {
	t.Helper()
	var pid int
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil
	}, long, 5*time.Millisecond)
	return pid
}

// The script starts a child that would outlive it and that holds its
// output. Killing only the script would leave the child running.
const spawnsChild = `sleep 600 & echo $! > "$1"; wait`

func TestATimeoutKillsTheWholeProcessGroupAndDenies(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	h := hook(config.HookPreTool, script(t, dir, "slow", spawnsChild), pidFile)
	h.Timeout = 300 * time.Millisecond

	start := time.Now()
	d, err := hooks.New([]config.Hook{h}).PreTool(t.Context(), session(t), "c", "Bash", jsontext.Value(`{}`))
	var herr *hooks.Error
	require.ErrorAs(t, err, &herr)
	require.Equal(t, hooks.KindTimeout, herr.Kind)
	require.Equal(t, hooks.Deny, d.Action)
	require.Less(t, time.Since(start), long/2)

	child := waitForPID(t, pidFile)
	require.Eventually(t, func() bool { return !alive(child) }, long, 5*time.Millisecond)
}

func TestCancellingKillsTheWholeProcessGroupAndDenies(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	r := hooks.New([]config.Hook{hook(config.HookStop, script(t, dir, "slow", spawnsChild), pidFile)})

	ctx, cancel := context.WithCancel(t.Context())
	type outcome struct {
		d   hooks.Decision
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		d, err := r.Stop(ctx, session(t), "end_turn")
		done <- outcome{d, err}
	}()
	child := waitForPID(t, pidFile)
	require.True(t, alive(child))
	cancel()

	got := <-done
	var herr *hooks.Error
	require.ErrorAs(t, got.err, &herr)
	require.Equal(t, hooks.KindCancelled, herr.Kind)
	require.ErrorIs(t, got.err, context.Canceled)
	require.Equal(t, hooks.Deny, got.d.Action)
	require.Eventually(t, func() bool { return !alive(child) }, long, 5*time.Millisecond)
}

// A hook that answers and exits, but leaves a process holding its output,
// has not finished: the answer could still grow. It ends at the timeout.
func TestAProcessLeftHoldingTheOutputIsATimeout(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	leaves := script(t, dir, "leaves", `sleep 600 & echo $! > "$1"; echo '{"decision":"allow"}'`)
	h := hook(config.HookPreTool, leaves, pidFile)
	h.Timeout = 300 * time.Millisecond

	d, err := hooks.New([]config.Hook{h}).PreTool(t.Context(), session(t), "c", "Bash", jsontext.Value(`{}`))
	var herr *hooks.Error
	require.ErrorAs(t, err, &herr)
	require.Equal(t, hooks.KindTimeout, herr.Kind)
	require.Equal(t, hooks.Deny, d.Action)
	child := waitForPID(t, pidFile)
	require.Eventually(t, func() bool { return !alive(child) }, long, 5*time.Millisecond)
}
