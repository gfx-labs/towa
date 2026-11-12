package config_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/config"
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
	write(t, filepath.Join(system, "config.yaml"), "telemetry:\n  otel: true\n  logs:\n    stderr:\n      level: warn\n      format: json\n")
	write(t, filepath.Join(user, "config.yml"), "telemetry:\n  logs:\n    stderr:\n      level: error\n")
	extra := write(t, filepath.Join(dir, "extra.json"), `{"telemetry":{"logs":{"stderr":{"level":"debug"}}}}`)
	flagged := write(t, filepath.Join(dir, "flagged.yaml"), "telemetry:\n  logs:\n    stderr:\n      level: info\n")

	var src config.Sources
	var args []string
	steps := []struct {
		name       string
		apply      func()
		wantLevel  slog.Level
		wantOrigin string
	}{
		{"defaults", func() {}, slog.LevelInfo, "default"},
		{"system file", func() { src.SystemDir = system }, slog.LevelWarn, filepath.Join(system, "config.yaml")},
		{"user file", func() { src.UserDir = user }, slog.LevelError, filepath.Join(user, "config.yml")},
		{"listed file", func() { src.Files = []string{extra} }, slog.LevelDebug, extra},
		{"file of --config", func() { args = []string{"--config", flagged} }, slog.LevelInfo, flagged},
		{"environment", func() {
			src.Environ = []string{"TOWA_TELEMETRY__LOGS__STDERR__LEVEL=warn", "TOWA_CONFIG=" + extra, "HOME=/nowhere"}
		}, slog.LevelWarn, "environment"},
		{"flag", func() { args = append(args, "--telemetry.logs.stderr.level=error") }, slog.LevelError, "flag"},
		{"flag by its short name", func() { args = append(args, "--log-level", "debug") }, slog.LevelDebug, "flag"},
	}
	for _, step := range steps {
		step.apply()
		got, err := config.Load(src, args...)
		require.NoError(t, err, step.name)
		require.Equal(t, step.wantLevel, got.Config.Telemetry.Logs.Stderr.Level, step.name)
		require.Equal(t, step.wantOrigin, got.Origin["telemetry.logs.stderr.level"], step.name)
	}

	// The system file's other keys survived every layer above it.
	got, err := config.Load(src, args...)
	require.NoError(t, err)
	require.True(t, got.Config.Telemetry.OTel)
	require.Equal(t, config.FormatJSON, got.Config.Telemetry.Logs.Stderr.Format)
	require.Equal(t, filepath.Join(system, "config.yaml"), got.Origin["telemetry.logs.stderr.format"])
	require.Equal(t, `"DEBUG"`, got.Values["telemetry.logs.stderr.level"])
}

func TestInvalidInputIsAnError(t *testing.T) {
	dir := t.TempDir()
	two, old := filepath.Join(dir, "two"), filepath.Join(dir, "old")
	write(t, filepath.Join(two, "config.yaml"), "")
	write(t, filepath.Join(two, "config.json"), "{}")
	write(t, filepath.Join(old, "config.toml"), "")
	file := func(name, body string) config.Sources {
		return config.Sources{Files: []string{write(t, filepath.Join(dir, name), body)}}
	}

	tests := []struct {
		name string
		src  config.Sources
		args []string
		want string
	}{
		{"unknown key in a file", file("typo.yaml", "telemetry:\n  otle: true\n"), nil, "typo.yaml: telemetry.otle"},
		{"unknown section in a file", file("section.yaml", "telemetery:\n  otel: true\n"), nil, "section.yaml: telemetery"},
		// The flag's spelling of a key is not a file's.
		{"dotted key in a file", file("dotted.yaml", "limits.max_wall: 30m\n"), nil, "dotted.yaml: limits.max_wall"},
		{"unknown key in a table", file("table.yaml", "hooks:\n  - event: stop\n    command: ['true']\n    timeout: 1s\n    timout: 2s\n"), nil, "hooks[0].timout"},
		{"unknown key in the environment", config.Sources{Environ: []string{"TOWA_TELEMETRY__OTLE=1"}}, nil, "TOWA_TELEMETRY__OTLE"},
		{"unknown flag", config.Sources{}, []string{"--telemetry.otle"}, "otle"},
		{"file that does not parse", file("broken.yaml", "telemetry:\n  otel: [true\n"), nil, "broken.yaml"},
		{"file with two documents", file("two.yaml", "telemetry:\n  otel: true\n---\ntelemetry:\n  otel: false\n"), nil, "two.yaml"},
		{"section given one value", file("flat.yaml", "telemetry: true\n"), nil, "flat.yaml: telemetry"},
		// YAML reads 0600 as the number 384.
		{"number where a file needs text", file("mode.yaml", "listen:\n  unix:\n    mode: 0600\n"), nil, "mode.yaml"},
		{"number where a file needs a duration", file("wall.yaml", "limits:\n  max_wall: 30\n"), nil, "wall.yaml"},
		{"number where a table needs a duration", file("hook-wait.yaml", "hooks:\n  - event: stop\n    command: ['true']\n    timeout: 1\n"), nil, "hooks[0].timeout"},
		{"value of the wrong type in a file", file("type.yaml", "limits:\n  max_tool_calls: many\n"), nil, "type.yaml"},
		{"text where a file needs a list", file("list.yaml", "context:\n  agents: a.md,b.md\n"), nil, "list.yaml"},
		{"value of the wrong type in the environment", config.Sources{Environ: []string{"TOWA_LIMITS__MAX_WALL=soon"}}, nil, "TOWA_LIMITS__MAX_WALL"},
		{"value of the wrong type", config.Sources{}, []string{"--telemetry.otel=sometimes"}, "telemetry.otel"},
		{"unknown log level", config.Sources{}, []string{"--telemetry.logs.stderr.level=loud"}, "loud"},
		{"unknown log format", config.Sources{}, []string{"--telemetry.logs.stderr.format=xml"}, "telemetry.logs.stderr.format"},
		{"shell timeout over maximum", config.Sources{}, []string{"--tools.shell.timeout=11m"}, "tools.shell.timeout"},
		{"disabled gate still requires budget", config.Sources{}, []string{"--tools.gate.enabled=false", "--tools.gate.budget_tokens=0"}, "tools.gate.budget_tokens"},
		{"retry nan", config.Sources{}, []string{"--limits.retry.multiplier=NaN"}, "limits.retry.multiplier"},
		{"retry infinity", config.Sources{}, []string{"--limits.retry.multiplier=+Inf"}, "limits.retry.multiplier"},
		{"compaction nan", config.Sources{}, []string{"--compaction.reactive_threshold=NaN"}, "compaction."},
		{"compaction duration overflow", config.Sources{}, []string{"--compaction.pending_wait_ms=9223372036855"}, "compaction.pending_wait_ms"},
		{"history shorter than samples", config.Sources{}, []string{"--compaction.history_window=2"}, "compaction.history_window"},
		{"nested section requires one source", file("section-two.yaml", "context:\n  sections:\n    - name: policy\n      text: rule\n      file: rule.md\n"), nil, "context.sections[0].text"},
		{"a section's name is its tag", file("section-name.yaml", "context:\n  sections:\n    - name: House Rules\n      text: rule\n"), nil, "context.sections[0].name"},
		{"nested hook tool name", file("hook.yaml", "hooks:\n  - event: pre_tool\n    command: ['true']\n    timeout: 1s\n    tools: ['']\n"), nil, "hooks[0].tools[0]"},
		{"hook tools on an event without a tool", file("hook-stop.yaml", "hooks:\n  - event: stop\n    command: ['true']\n    timeout: 1s\n    tools: [Bash]\n"), nil, "hooks[0].tools"},
		{"hook with an empty program", file("hook-empty.yaml", "hooks:\n  - event: stop\n    command: ['']\n    timeout: 1s\n"), nil, "hooks[0].command"},
		{"provider invalid scheme with credentials", file("provider-url.yaml", "providers:\n  - id: gateway\n    kind: chat\n    base_url: ftp://url-user:url-password@example.com?token=url-query-secret\n"), nil, "providers[0].base_url"},
		{"provider URL parse failure", file("provider-parse.yaml", "providers:\n  - id: gateway\n    kind: chat\n    base_url: https://url-user:url-password@example.com/%zz?token=url-query-secret\n"), nil, "providers[0].base_url"},
		{"provider URL of the wrong type", file("provider-type.yaml", "providers:\n  - id: gateway\n    kind: chat\n    base_url: [\"https://url-user:url-password@example.com?token=url-query-secret\"]\n"), nil, "providers[0].base_url"},
		{"embeddings URL query", config.Sources{}, []string{"--embeddings.model=embed", "--embeddings.base_url=https://url-user:url-password@example.com?token=url-query-secret"}, "embeddings.base_url"},
		{"embeddings URL parse failure", config.Sources{}, []string{"--embeddings.model=embed", "--embeddings.base_url=https://url-user:url-password@example.com/%zz?token=url-query-secret"}, "embeddings.base_url"},
		{"jev URL without route", config.Sources{}, []string{"--jev.base_url=https://url-user:url-password@example.com?token=url-query-secret"}, "jev.base_url"},
		{"jev URL credentials", config.Sources{}, []string{"--jev.route=custom", "--jev.model=decision", "--jev.base_url=https://url-user:url-password@example.com?token=url-query-secret"}, "jev.base_url"},
		{"jev URL parse failure", config.Sources{}, []string{"--jev.route=custom", "--jev.model=decision", "--jev.base_url=https://url-user:url-password@example.com/%zz?token=url-query-secret"}, "jev.base_url"},
		{"mcp invalid scheme with credentials", file("mcp-url.yaml", "mcp:\n  servers:\n    - name: remote\n      transport: sse\n      url: ftp://url-user:url-password@example.com?token=url-query-secret\n"), nil, "mcp.servers[0].url"},
		{"mcp URL parse failure", file("mcp-parse.yaml", "mcp:\n  servers:\n    - name: remote\n      transport: sse\n      url: https://url-user:url-password@example.com/%zz?token=url-query-secret\n"), nil, "mcp.servers[0].url"},
		{"listed file is missing", config.Sources{Files: []string{filepath.Join(dir, "absent.yaml")}}, nil, "absent.yaml"},
		{"file of --config is missing", config.Sources{}, []string{"--config", filepath.Join(dir, "gone.yaml")}, "gone.yaml"},
		{"listed file of an unknown format", file("c.ini", ""), nil, ".ini"},
		{"listed file of the format that is no longer read", file("c.toml", ""), nil, ".toml"},
		{"two config files in one directory", config.Sources{UserDir: two}, nil, "more than one"},
		{"config.toml left in a directory", config.Sources{UserDir: old}, nil, "config.toml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(tt.src, tt.args...)
			require.ErrorContains(t, err, tt.want)
			for _, secret := range []string{"url-user", "url-password", "url-query-secret"} {
				require.NotContains(t, err.Error(), secret)
			}
		})
	}
}

func TestOptionalSectionsPreserveLoaderPolicy(t *testing.T) {
	path := write(t, filepath.Join(t.TempDir(), "config.yaml"), `
subagents:
  max_depth: 0
  max_concurrent: 0
compaction:
  mode: "off"
  openai_native: "off"
  openai_native_threshold_tokens: -1
embeddings:
  base_url: http://127.0.0.1:11434/v1
  model: embedding-model
jev:
  route: custom
  base_url: http://127.0.0.1:8080/v1
  model: decision-model
providers:
  - id: gateway
    kind: chat
    base_url: https://example.com/v1?accepted=query#fragment
hooks:
  - event: prompt
    command: [printf, ""]
    timeout: 1s
  - event: pre_tool
    tools: [Bash]
    command: ["true"]
    timeout: 10s
    max_output_bytes: 65536
context:
  default_agents: false
  agents: ["notes, with a comma.md", docs]
  skills_dirs: ["~/a"]
  max_file_bytes: 1000
  max_total_bytes: 5000
  sections:
    - name: policy
      file: policy.md
`)
	got, err := config.Load(config.Sources{Files: []string{path}})
	require.NoError(t, err)
	// A list item keeps its comma, and an empty value in a file replaces a
	// default that is not empty.
	require.Equal(t, []string{"notes, with a comma.md", "docs"}, got.Config.Context.Agents)
	require.False(t, got.Config.Context.DefaultAgents)
	require.Equal(t, path, got.Origin["context.default_agents"])
}

// A table is one key: a flag or an environment variable gives it as one
// line of YAML, and it replaces the table of a file whole.
func TestATableFromTheEnvironmentReplacesTheFilesTable(t *testing.T) {
	path := write(t, filepath.Join(t.TempDir(), "config.yaml"),
		"hooks:\n  - event: prompt\n    command: [one]\n    timeout: 1s\n  - event: stop\n    command: [two]\n    timeout: 1s\n")
	got, err := config.Load(config.Sources{
		Files:   []string{path},
		Environ: []string{`TOWA_HOOKS=[{event: stop, command: [three], timeout: 2s}]`},
	})
	require.NoError(t, err)
	require.Len(t, got.Config.Hooks, 1)
	require.Equal(t, []string{"three"}, got.Config.Hooks[0].Command)
	require.Equal(t, "environment", got.Origin["hooks"])
}

// The models are one list in order, so a later file's list replaces an
// earlier file's, as any other key does, and a flag replaces both.
func TestTheLastModelsListWins(t *testing.T) {
	dir := t.TempDir()
	system := write(t, filepath.Join(dir, "system.yaml"),
		"models: ['claude:work/claude-opus-4-7#high', 'claude:work/claude-sonnet-4-6']\nmodel_context_window: 1000\n")
	user := write(t, filepath.Join(dir, "user.yaml"), "models:\n  - codex:chatgpt/gpt-6.1-sol\n")
	got, err := config.Load(config.Sources{Files: []string{system, user}})
	require.NoError(t, err)
	require.Equal(t, []string{"codex:chatgpt/gpt-6.1-sol"}, got.Config.Models)
	require.EqualValues(t, 1000, got.Config.ModelContextWindow)
	require.Equal(t, user, got.Origin["models"])

	got, err = config.Load(config.Sources{Files: []string{system, user}}, "--models", "claude:work/claude-opus-4-7#low,codex:chatgpt/gpt-6.1-sol")
	require.NoError(t, err)
	require.Equal(t, []string{"claude:work/claude-opus-4-7#low", "codex:chatgpt/gpt-6.1-sol"}, got.Config.Models)
}

// An account comes from a config layer, from the auth store, or from a
// model that names it on a built-in provider ahead of its login. A declared
// account is kept as declared when the store has the same id.
func TestAccountsComeFromConfigStoreAndChains(t *testing.T) {
	dir := t.TempDir()
	path := write(t, filepath.Join(dir, "config.yaml"), `
models:
  - claude:work/claude-opus-4-7
  - claude:later/claude-opus-4-7
accounts:
  - id: work
    provider: claude
    kind: proxied_oauth
`)
	got, err := config.Load(config.Sources{
		Files: []string{path},
		StoredAccounts: func() (map[string]string, error) {
			return map[string]string{"work": "claude", "gpt": "codex"}, nil
		},
	})
	require.NoError(t, err)
	require.Equal(t, []config.Account{
		{ID: "work", Provider: "claude", Kind: config.AccountProxiedOAuth},
		{ID: "gpt", Provider: "codex", Kind: config.AccountOAuth, Stored: true},
		{ID: "later", Provider: "claude", Kind: config.AccountOAuth, Stored: true},
	}, got.Config.Accounts)

	// A login cannot make an account of a configured provider, so a model
	// that names one must find it declared.
	path = write(t, filepath.Join(dir, "gateway.yaml"), `
models:
  - gateway:nobody/some-model
model_context_window: 1000
model_max_output_tokens: 100
providers:
  - id: gateway
    kind: chat
    base_url: https://example.com/v1
`)
	_, err = config.Load(config.Sources{Files: []string{path}})
	require.ErrorContains(t, err, "nobody")
}

// Text from a command substitution is one entry per flag, commas and
// newlines included.
func TestEachAgentsTextFlagIsOneEntry(t *testing.T) {
	got, err := config.Load(config.Sources{}, "--context.agents_text=a, b\nc", "--context.agents_text=d")
	require.NoError(t, err)
	require.Equal(t, []string{"a, b\nc", "d"}, got.Config.Context.AgentsText)
}
