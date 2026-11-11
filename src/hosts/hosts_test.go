package hosts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/auth"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/hosts"
)

// load resolves a YAML file as the program would.
func load(t *testing.T, yaml string) config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	loaded, err := config.Load(config.Sources{Files: []string{path}})
	require.NoError(t, err)
	return loaded.Config
}

func addresses(r hosts.Report) map[string][]string {
	byKey := map[string][]string{}
	for _, h := range r.Hosts {
		byKey[h.Key] = append(byKey[h.Key], h.Address)
	}
	return byKey
}

func TestListFromAConfigFile(t *testing.T) {
	cfg := load(t, `
telemetry:
  otel: true
providers:
  - id: openrouter
    kind: chat
    base_url: https://openrouter.ai/api/v1
  - id: local
    kind: responses
    base_url: "http://[::1]:8080/v1"
accounts:
  - id: work
    provider: claude
    kind: oauth
  - id: held
    provider: codex
    kind: proxied_oauth
  - id: key
    provider: openrouter
    kind: api_key
    api_key_env: OPENROUTER_API_KEY
  - id: codex-key
    provider: codex
    kind: api_key
    api_key_env: OPENAI_API_KEY
embeddings:
  base_url: http://127.0.0.1:8080/v1
  model: bge
websearch:
  endpoint: https://search.example/html/
jev:
  route: custom
  model: jev-local
  base_url: https://jev.internal.example
mcp:
  servers:
    - name: docs
      transport: streamable_http
      url: https://mcp.example.com/mcp
    - name: tickets
      transport: sse
      url: https://tickets.example.com/sse
    - name: files
      transport: stdio
      command: mcp-files
`)
	// The flows are the real ones, pointed at hosts this test names, so
	// that the test does not repeat the URLs src/auth owns.
	claude, codex := auth.ClaudeFlow(), auth.CodexFlow()
	claude.AuthorizeURL, claude.TokenURL = "https://login.example/authorize", "https://token.example:8443/token"
	codex.AuthorizeURL, codex.TokenURL = "https://codex-login.example/authorize", "https://codex-login.example/token"

	report, err := hosts.List(cfg, hosts.Inputs{
		Flows: map[string]auth.Flow{"claude": claude, "codex": codex},
		OTLP:  hosts.OTLP{Endpoint: "http://collector:4318", MetricsEndpoint: "https://metrics.example/v1/metrics"},
		BuiltIn: hosts.BuiltIn{
			Claude: "https://claude-api.example", CodexAPIKey: "https://codex-api.example/v1", CodexOAuth: "https://codex-login-api.example/backend",
		},
		JevEndpoint:       "https://jev.internal.example/v1/systemone",
		MCPTokenEndpoints: map[string]string{"docs": "https://auth.example.com/token"},
	})
	require.NoError(t, err)

	require.Equal(t, map[string][]string{
		"providers[0].base_url": {"openrouter.ai:443"},
		"providers[1].base_url": {"[::1]:8080"},
		// The built-in provider's endpoint, then the oauth account's flow.
		"accounts[0].provider": {"claude-api.example:443", "login.example:443", "token.example:8443"},
		// The proxied account contacts its provider's login endpoint. Its
		// flow is not listed: its proxy refreshes the token.
		"accounts[1].provider": {"codex-login-api.example:443"},
		// An API key for the same provider goes to another endpoint.
		"accounts[3].provider": {"codex-api.example:443"},
		"mcp.servers[0].url":   {"mcp.example.com:443"},
		"mcp.servers[0].oauth": {"auth.example.com:443"},
		"mcp.servers[1].url":   {"tickets.example.com:443"},
		"embeddings.base_url":  {"127.0.0.1:8080"},
		"websearch.endpoint":   {"search.example:443"},
		"jev.base_url":         {"jev.internal.example:443"},
		// Traces and logs fall back to the general variable.
		"telemetry.otel:" + hosts.EnvOTLPEndpoint:        {"collector:4318"},
		"telemetry.otel:" + hosts.EnvOTLPMetricsEndpoint: {"metrics.example:443"},
	}, addresses(report))
	require.IsIncreasingf(t, hostLines(report), "hosts are sorted")

	// The stdio server is said to be left out. The CLI test owns the rest of
	// this wording.
	require.Contains(t, strings.Join(report.NotCovered, "\n"), `mcp.servers[2]: "files" is a stdio server`)
}

// What the configuration turns on and List was given no URL for is an
// error, not a missing line.
func TestAnEndpointThatWasNotGivenIsAnError(t *testing.T) {
	claude := load(t, "accounts:\n  - id: a\n    provider: claude\n    kind: proxied_api_key\n")
	_, err := hosts.List(claude, hosts.Inputs{})
	require.Error(t, err)

	jev := load(t, "accounts:\n  - id: k\n    provider: or\n    kind: api_key\n    api_key_env: K\n"+
		"providers:\n  - id: or\n    kind: chat\n    base_url: https://or.example/v1\n"+
		"jev:\n  route: openrouter\n  account: k\n")
	_, err = hosts.List(jev, hosts.Inputs{})
	require.Error(t, err)
	report, err := hosts.List(jev, hosts.Inputs{JevEndpoint: "https://decisions.example/x"})
	require.NoError(t, err)
	require.Equal(t, []string{"decisions.example:443"}, addresses(report)["jev.route"])
}

func hostLines(r hosts.Report) []string {
	lines := make([]string, len(r.Hosts))
	for i, h := range r.Hosts {
		lines[i] = h.Address + "\t" + h.Key
	}
	return lines
}

func TestOTLPIsListedOnlyWhenTurnedOnAndNamed(t *testing.T) {
	otlp := hosts.OTLP{Endpoint: "http://collector:4318"}

	const noSearch = "websearch:\n  enabled: false\n"
	off, err := hosts.List(load(t, noSearch), hosts.Inputs{OTLP: otlp})
	require.NoError(t, err)
	require.Empty(t, off.Hosts)

	// On without a variable: the exporter's default is not ours to state,
	// so each signal is reported as not covered.
	unnamed, err := hosts.List(load(t, noSearch+"telemetry:\n  otel: true\n"), hosts.Inputs{})
	require.NoError(t, err)
	require.Empty(t, unnamed.Hosts)
	require.Greater(t, len(unnamed.NotCovered), len(off.NotCovered))

	// A value the exporter could not use is an error, not a missing line.
	_, err = hosts.List(load(t, "telemetry:\n  otel: true\n"), hosts.Inputs{OTLP: hosts.OTLP{Endpoint: "collector"}})
	require.Error(t, err)
}
