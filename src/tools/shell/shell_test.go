package shell_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/fsops"
	"github.com/gfx-labs/acpharness/src/tools/shell"
	"github.com/gfx-labs/acpharness/src/transcript"
)

func toolsConfig(t *testing.T) config.Tools {
	t.Helper()
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	return loaded.Config.Tools
}

// stream collects what a command sends as progress.
type stream struct {
	mu   sync.Mutex
	text strings.Builder
}

func (s *stream) progress(_ context.Context, content []acp.ToolCallContent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range content {
		s.text.WriteString(c.Content.Content.Text.Text)
	}
	return nil
}

func (s *stream) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text.String()
}

func env(t *testing.T, s *stream) tools.Env {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	e := tools.Env{Cwd: dir}
	if s != nil {
		e.Progress = s.progress
	}
	return e
}

func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func TestRunReportsOutputAndExitStatus(t *testing.T) {
	cfg, s := toolsConfig(t), &stream{}
	e := env(t, s)
	require.NoError(t, os.Mkdir(filepath.Join(e.Cwd, "sub"), 0o755))

	res, err := shell.Run(context.Background(), e, cfg, shell.Input{Command: "echo out; echo err >&2; pwd; exit 3", Dir: "sub"})
	require.NoError(t, err)
	require.Equal(t, shell.EndExited, res.End)
	require.Equal(t, 3, res.ExitCode)
	require.Equal(t, "out\nerr\n"+filepath.Join(e.Cwd, "sub")+"\n", res.Output)
	require.Equal(t, res.Output, s.String())
	require.Equal(t, transcript.OutcomeFailed, res.Outcome())

	res, err = shell.Run(context.Background(), e, cfg, shell.Input{Command: "kill -TERM $$"})
	require.NoError(t, err)
	require.Equal(t, "terminated", res.Signal)
	require.Equal(t, transcript.OutcomeFailed, res.Outcome())

	res, err = shell.Run(context.Background(), e, cfg, shell.Input{Command: "true"})
	require.NoError(t, err)
	require.Equal(t, transcript.OutcomeCompleted, res.Outcome())

	_, err = shell.Run(context.Background(), e, cfg, shell.Input{Command: "true", Dir: "/"})
	var outside *fsops.Error
	require.ErrorAs(t, err, &outside)
	require.Equal(t, fsops.KindOutside, outside.Kind)

	_, err = shell.Run(context.Background(), e, cfg, shell.Input{Command: "true", Timeout: cfg.Shell.MaxTimeout + time.Second})
	var bad *shell.Error
	require.ErrorAs(t, err, &bad)
	require.Equal(t, shell.KindTimeout, bad.Kind)
}

// The command starts a child and waits for it. Cancelling must kill the
// child too, and the result must say the command had started.
func TestCancelKillsTheWholeProcessGroup(t *testing.T) {
	cfg, s := toolsConfig(t), &stream{}
	e := env(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		res shell.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := shell.Run(ctx, e, cfg, shell.Input{Command: "sleep 300 & echo $!; wait"})
		done <- outcome{res, err}
	}()
	require.Eventually(t, func() bool { return strings.HasSuffix(s.String(), "\n") }, 10*time.Second, 5*time.Millisecond)
	child, err := strconv.Atoi(strings.TrimSpace(s.String()))
	require.NoError(t, err)
	require.True(t, alive(child))

	cancel()
	got := <-done
	require.NoError(t, got.err)
	require.Equal(t, shell.EndCancelled, got.res.End)
	require.True(t, got.res.Started)
	require.Equal(t, "killed", got.res.Signal)
	require.Equal(t, transcript.OutcomeUnknown, got.res.Outcome())
	require.False(t, got.res.PipeHeld)
	require.Eventually(t, func() bool { return !alive(child) }, 10*time.Second, 5*time.Millisecond)
}

func TestCancelBeforeStartRunsNothing(t *testing.T) {
	cfg := toolsConfig(t)
	e := env(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := shell.Run(ctx, e, cfg, shell.Input{Command: "touch ran"})
	require.NoError(t, err)
	require.False(t, res.Started)
	require.Equal(t, transcript.OutcomeCancelled, res.Outcome())
	require.NoFileExists(t, filepath.Join(e.Cwd, "ran"))
}

func TestTimeoutKillsTheGroup(t *testing.T) {
	cfg, s := toolsConfig(t), &stream{}
	e := env(t, s)
	res, err := shell.Run(context.Background(), e, cfg, shell.Input{
		Command: "sleep 300 & echo $!; wait", Timeout: 300 * time.Millisecond,
	})
	require.NoError(t, err)
	require.Equal(t, shell.EndTimedOut, res.End)
	require.Equal(t, transcript.OutcomeFailed, res.Outcome())
	child, err := strconv.Atoi(strings.TrimSpace(res.Output))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return !alive(child) }, 10*time.Second, 5*time.Millisecond)
}

// A process that leaves the group keeps the pipe open after the command
// exits. The run must return and say so instead of waiting for it.
func TestAProcessLeftBehindDoesNotHoldTheRun(t *testing.T) {
	cfg := toolsConfig(t)
	cfg.Shell.PipeWait = 200 * time.Millisecond
	e := env(t, nil)
	res, err := shell.Run(context.Background(), e, cfg, shell.Input{Command: "setsid sleep 300 & echo $!"})
	require.NoError(t, err)
	child, err := strconv.Atoi(strings.TrimSpace(res.Output))
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	require.Equal(t, shell.EndExited, res.End)
	require.True(t, res.PipeHeld)
	require.Less(t, res.Duration, 10*time.Second)
}

func TestOutputOverTheLimitKeepsBothEnds(t *testing.T) {
	cfg, s := toolsConfig(t), &stream{}
	cfg.MaxOutputBytes = 2000
	e := env(t, s)
	// 10000 lines of 6 bytes, "00001\n" to "10000\n".
	const total = 10000 * 6
	res, err := shell.Run(context.Background(), e, cfg, shell.Input{Command: "seq -w 1 10000"})
	require.NoError(t, err)
	require.Len(t, res.Output, 1000)
	require.Len(t, res.Tail, 1000)
	require.True(t, strings.HasPrefix(res.Output, "00001\n"))
	require.True(t, strings.HasSuffix(res.Tail, "\n10000\n"))
	require.Equal(t, int64(total-2000), res.DroppedBytes)
	// The client still gets all of it.
	require.Len(t, s.String(), total)
}

// A multi-byte character split across two reads must reach the client
// whole.
func TestProgressDoesNotSplitACharacter(t *testing.T) {
	cfg, s := toolsConfig(t), &stream{}
	e := env(t, s)
	res, err := shell.Run(context.Background(), e, cfg, shell.Input{
		Command: `printf 'a\xc3'; sleep 0.2; printf '\xa9b\xff'`,
	})
	require.NoError(t, err)
	require.Equal(t, "aéb\uFFFD", res.Output)
	require.Equal(t, "aéb\uFFFD", s.String())
}
