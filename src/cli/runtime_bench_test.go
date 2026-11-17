package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/config/configtest"
	"github.com/gfx-labs/towa/src/support/telemetry"
)

// BenchmarkOpenSessions reports the heap one open, idle session holds:
// sessions in one working directory with a context file and skills, as
// a pool of agents on one repository has.
func BenchmarkOpenSessions(b *testing.B) {
	const sessions = 200
	loaded, err := configtest.Load(config.Sources{})
	require.NoError(b, err)
	cfg := loaded.Config
	cfg.Providers = []config.Provider{{ID: "other", Kind: config.ProviderResponses, BaseURL: "http://127.0.0.1:1/v1"}}
	cfg.Accounts = []config.Account{{ID: "local", Provider: "other", Kind: config.AccountNone}}
	const model = "other:local/gpt-5.5"
	cfg.Models = []string{model}
	cfg.ModelContextWindow, cfg.ModelMaxOutputTokens = 100_000, 1024
	home := b.TempDir()
	require.NoError(b, os.WriteFile(filepath.Join(home, "AGENTS.md"), []byte(strings.Repeat("A rule for this repository.\n", 1000)), 0o600))
	for i := range 20 {
		dir := filepath.Join(home, ".agents", "skills", "skill"+string(rune('a'+i)))
		require.NoError(b, os.MkdirAll(dir, 0o700))
		require.NoError(b, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: skill"+string(rune('a'+i))+"\ndescription: "+strings.Repeat("does a thing ", 20)+"\n---\nbody\n"), 0o600))
	}
	host := Host{Stdout: io.Discard, Stderr: io.Discard, Environ: []string{"HOME=" + home, "XDG_STATE_HOME=" + filepath.Join(home, "state")}}
	tel, err := telemetry.Start(b.Context(), cfg.Telemetry, version(), io.Discard)
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, tel.Shutdown(context.Background())) })
	factory, err := newRuntimeFactory(b.Context(), cfg, host, tel)
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, factory.Close()) })

	heap := func() uint64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	var perSession float64
	for b.Loop() {
		before := heap()
		open := make([]*sessionRuntime, sessions)
		for i := range open {
			open[i], err = factory.open(b.Context(), loop.Turn{
				SessionID: acp.SessionId(uuid.NewV7().String()), Cwd: home, Model: model,
				Update: func(context.Context, acp.SessionUpdate) error { return nil },
			})
			require.NoError(b, err)
		}
		perSession = float64(heap()-before) / sessions
		for _, r := range open {
			require.NoError(b, r.Close(context.Background()))
		}
		runtime.KeepAlive(open)
	}
	b.ReportMetric(perSession, "heap-B/session")
}
