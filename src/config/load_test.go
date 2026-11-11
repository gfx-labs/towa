package config_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/config"
)

func write(t *testing.T, path, body string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// Each layer sets the stderr level, and each later layer is added in turn:
// the newest layer must win, and the keys it leaves alone must keep the
// value of the layer below.
func TestLaterLayersOverrideEarlierOnes(t *testing.T) {
	dir := t.TempDir()
	system, user := filepath.Join(dir, "etc"), filepath.Join(dir, "user")
	write(t, filepath.Join(system, "config.toml"), "[telemetry]\notel = true\n[telemetry.logs.stderr]\nlevel = \"warn\"\nformat = \"json\"\n")
	write(t, filepath.Join(user, "config.yaml"), "telemetry:\n  logs:\n    stderr:\n      level: error\n")
	extra := write(t, filepath.Join(dir, "extra.json"), `{"telemetry":{"logs":{"stderr":{"level":"debug"}}}}`)

	steps := []struct {
		name       string
		apply      func(*config.Sources)
		wantLevel  slog.Level
		wantOrigin string
	}{
		{"defaults", func(*config.Sources) {}, slog.LevelInfo, "default"},
		{"system file", func(s *config.Sources) { s.SystemDir = system }, slog.LevelWarn, filepath.Join(system, "config.toml")},
		{"user file", func(s *config.Sources) { s.UserDir = user }, slog.LevelError, filepath.Join(user, "config.yaml")},
		{"listed file", func(s *config.Sources) { s.Files = []string{extra} }, slog.LevelDebug, extra},
		{"environment", func(s *config.Sources) {
			s.Environ = []string{"ACPHARNESS_TELEMETRY__LOGS__STDERR__LEVEL=warn", "ACPHARNESS_CONFIG=" + extra, "HOME=/nowhere"}
		}, slog.LevelWarn, "environment"},
		{"flag", func(s *config.Sources) {
			s.Flags = map[string]any{config.FlagKeys["log-level"]: "error"}
		}, slog.LevelError, "flag"},
		{"set", func(s *config.Sources) { s.Set = []string{"telemetry.logs.stderr.level=debug"} }, slog.LevelDebug, "--set"},
	}
	var src config.Sources
	for _, step := range steps {
		step.apply(&src)
		got, err := config.Load(src)
		require.NoError(t, err, step.name)
		require.Equal(t, step.wantLevel, got.Config.Telemetry.Logs.Stderr.Level, step.name)
		require.Equal(t, step.wantOrigin, got.Origin["telemetry.logs.stderr.level"], step.name)
	}

	// The system file's other keys survived every layer above it.
	got, err := config.Load(src)
	require.NoError(t, err)
	require.True(t, got.Config.Telemetry.OTel)
	require.Equal(t, config.FormatJSON, got.Config.Telemetry.Logs.Stderr.Format)
	require.Equal(t, filepath.Join(system, "config.toml"), got.Origin["telemetry.logs.stderr.format"])
}

// Text from the environment and --set is converted to the key's type.
func TestTextValuesBecomeTheKeysType(t *testing.T) {
	got, err := config.Load(config.Sources{
		Environ: []string{"ACPHARNESS_TELEMETRY__OTEL=true"},
		Set:     []string{"telemetry.logs.stderr.enabled=false"},
	})
	require.NoError(t, err)
	require.True(t, got.Config.Telemetry.OTel)
	require.False(t, got.Config.Telemetry.Logs.Stderr.Enabled)
}

func TestInvalidInputIsAnError(t *testing.T) {
	dir := t.TempDir()
	two := filepath.Join(dir, "two")
	write(t, filepath.Join(two, "config.toml"), "")
	write(t, filepath.Join(two, "config.json"), "{}")

	tests := []struct {
		name string
		src  config.Sources
		want string
	}{
		{"unknown key in a file", config.Sources{Files: []string{write(t, filepath.Join(dir, "typo.toml"), "[telemetry]\notle = true\n")}}, "otle"},
		{"unknown key in the environment", config.Sources{Environ: []string{"ACPHARNESS_TELEMETRY__OTLE=1"}}, "otle"},
		{"unknown key in --set", config.Sources{Set: []string{"telemetry.otle=1"}}, "otle"},
		{"value of the wrong type", config.Sources{Set: []string{"telemetry.otel=sometimes"}}, "telemetry.otel"},
		{"unknown log level", config.Sources{Set: []string{"telemetry.logs.stderr.level=loud"}}, "loud"},
		{"unknown log format", config.Sources{Set: []string{"telemetry.logs.stderr.format=xml"}}, "telemetry.logs.stderr.format"},
		{"shell timeout over maximum", config.Sources{Set: []string{"tools.shell.timeout=11m"}}, "tools.shell.timeout"},
		{"disabled gate still requires budget", config.Sources{Set: []string{"tools.gate.enabled=false", "tools.gate.budget_tokens=0"}}, "tools.gate.budget_tokens"},
		{"retry nan", config.Sources{Set: []string{"limits.retry.multiplier=NaN"}}, "limits.retry.multiplier"},
		{"retry infinity", config.Sources{Set: []string{"limits.retry.multiplier=+Inf"}}, "limits.retry.multiplier"},
		{"compaction nan", config.Sources{Set: []string{"compaction.reactive_threshold=NaN"}}, "compaction."},
		{"compaction duration overflow", config.Sources{Set: []string{"compaction.pending_wait_ms=9223372036855"}}, "compaction.pending_wait_ms"},
		{"history shorter than samples", config.Sources{Set: []string{"compaction.history_window=2"}}, "compaction.history_window"},
		{"nested section requires one source", config.Sources{Files: []string{write(t, filepath.Join(dir, "section.toml"), "[[context.sections]]\nname='policy'\ntext='rule'\nfile='rule.md'\n")}}, "context.sections[0].text"},
		{"nested hook tool name", config.Sources{Files: []string{write(t, filepath.Join(dir, "hook.toml"), "[[hooks]]\nevent='pre_tool'\ncommand=['true']\ntimeout='1s'\ntools=['']\n")}}, "hooks[0].tools[0]"},
		{"--set without a value", config.Sources{Set: []string{"telemetry.otel"}}, "key=value"},
		{"listed file is missing", config.Sources{Files: []string{filepath.Join(dir, "absent.toml")}}, "absent.toml"},
		{"listed file of an unknown format", config.Sources{Files: []string{write(t, filepath.Join(dir, "c.ini"), "")}}, ".ini"},
		{"two config files in one directory", config.Sources{UserDir: two}, "more than one"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(tt.src)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestOptionalSectionsPreserveLoaderPolicy(t *testing.T) {
	path := write(t, filepath.Join(t.TempDir(), "config.toml"), `
[subagents]
max_depth = 0
max_concurrent = 0
[compaction]
mode = "off"
openai_native = "off"
openai_native_threshold_tokens = -1
[embeddings]
base_url = "http://127.0.0.1:11434/v1"
model = "embedding-model"
[jev]
route = "custom"
base_url = "http://127.0.0.1:8080/v1"
model = "decision-model"
[[providers]]
id = "gateway"
kind = "chat"
base_url = "https://example.com/v1?accepted=query#fragment"
[[hooks]]
event = "prompt"
command = ["printf", ""]
timeout = "1s"
[[context.sections]]
name = "policy"
file = "policy.md"
`)
	_, err := config.Load(config.Sources{Files: []string{path}})
	require.NoError(t, err)
}
