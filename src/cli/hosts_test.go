package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/auth"
	"github.com/gfx-labs/acpharness/src/hosts"
)

// The hosts command from the argument list to its two streams: the list on
// standard output alone, and what it does not cover on standard error.
func TestHostsPrintsTheListAndWhatItDoesNotCover(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "extra.yaml")
	require.NoError(t, os.WriteFile(file, []byte(`
providers:
  - id: openrouter
    kind: chat
    base_url: https://openrouter.ai/api/v1
accounts:
  - id: work
    provider: claude
    kind: oauth
jev:
  route: custom
  model: jev-local
  base_url: https://jev.internal.example/v1
embeddings:
  base_url: http://embed.internal:8080/v1
  model: bge
mcp:
  servers:
    - name: docs
      transport: sse
      url: http://mcp.internal:9000/sse
    - name: moved
      transport: streamable_http
      url: https://moved.example/mcp
`), 0o600))
	flow := auth.ClaudeFlow()
	flow.AuthorizeURL, flow.TokenURL = "https://login.example/authorize", "https://login.example/token"

	// The stored logins are written by the store the command reads. The
	// second is for an address the server no longer has, so the MCP
	// client would not use it and its endpoint is not listed.
	state := filepath.Join(dir, "state")
	stateDir, err := auth.StateDir(state, "")
	require.NoError(t, err)
	store := auth.NewStore(stateDir)
	require.NoError(t, store.SaveMCP("docs", auth.MCPTokens{
		ServerURL: "http://mcp.internal:9000/sse", AccessToken: "a", RefreshToken: "r", TokenEndpoint: "https://sso.example/oauth/token",
	}))
	require.NoError(t, store.SaveMCP("moved", auth.MCPTokens{
		ServerURL: "https://old.example/mcp", AccessToken: "a", RefreshToken: "r", TokenEndpoint: "https://old-sso.example/token",
	}))

	var stdout, stderr bytes.Buffer
	h := Host{
		Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr,
		Environ: []string{"OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp.example:4318", "XDG_STATE_HOME=" + state},
	}
	env := builtInEnv()
	env.hosts = hostsEnv{
		flows:   map[string]auth.Flow{"claude": flow},
		builtIn: hosts.BuiltIn{Claude: "https://claude-api.example"},
	}
	// A config key is a flag, before or after the command.
	require.NoError(t, run(t.Context(), []string{"acpharness", "--config", file, "--telemetry.otel", "hosts", "--websearch.enabled=false"}, h, env))

	// The authorize and token endpoints share a host, which is one line.
	require.Equal(t, strings.Join([]string{
		"claude-api.example:443\taccounts[0].provider",
		"embed.internal:8080\tembeddings.base_url",
		"jev.internal.example:443\tjev.base_url",
		"login.example:443\taccounts[0].provider",
		"mcp.internal:9000\tmcp.servers[0].url",
		"moved.example:443\tmcp.servers[1].url",
		"openrouter.ai:443\tproviders[0].base_url",
		"otlp.example:4318\ttelemetry.otel:OTEL_EXPORTER_OTLP_ENDPOINT",
		"sso.example:443\tmcp.servers[0].oauth",
		"",
	}, "\n"), stdout.String())
	for line := range strings.Lines(stderr.String()) {
		require.True(t, strings.HasPrefix(line, "hosts: not covered: "), line)
	}
	require.Contains(t, stderr.String(), `"moved" has no stored login`)
	require.NotContains(t, stderr.String(), `"docs" has no stored login`)
	// Webfetch is said to reach any host, and is not in the list.
	require.Contains(t, stderr.String(), "webfetch: the tool fetches any URL the model names, so it reaches any host")
	require.NotContains(t, stdout.String(), "webfetch")
}

// The command's own inputs are the built-in URLs of the packages that own
// them, each of which List can read as a host.
func TestHostsWithTheBuiltInEndpoints(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "c.yaml")
	require.NoError(t, os.WriteFile(file, []byte(`
accounts:
  - id: a
    provider: claude
    kind: proxied_oauth
  - id: b
    provider: codex
    kind: proxied_oauth
  - id: c
    provider: codex
    kind: proxied_api_key
jev:
  route: typesafe
  account: c
`), 0o600))
	var stdout, stderr bytes.Buffer
	h := Host{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr, Environ: []string{"XDG_STATE_HOME=" + dir}}
	require.NoError(t, Run(t.Context(), []string{"acpharness", "--config", file, "hosts"}, h))
	keys := map[string]int{}
	for line := range strings.Lines(stdout.String()) {
		_, key, ok := strings.Cut(strings.TrimSuffix(line, "\n"), "\t")
		require.True(t, ok, line)
		keys[key]++
	}
	// The search endpoint is the configured default, with no key of the user's.
	require.Equal(t, map[string]int{
		"accounts[0].provider": 1, "accounts[1].provider": 1, "accounts[2].provider": 1, "jev.route": 1, "websearch.endpoint": 1,
	}, keys)
}
