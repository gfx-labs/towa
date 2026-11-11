package hosts_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/auth"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/hosts"
)

// load resolves a TOML file as the program would.
func load(t *testing.T, toml string) config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(toml), 0o600))
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
[telemetry]
otel = true

[[providers]]
id = "openrouter"
kind = "chat"
base_url = "https://openrouter.ai/api/v1"

[[providers]]
id = "local"
kind = "responses"
base_url = "http://[::1]:8080/v1"

[[accounts]]
id = "work"
provider = "claude"
kind = "oauth"

[[accounts]]
id = "held"
provider = "codex"
kind = "proxied_oauth"

[[accounts]]
id = "key"
provider = "openrouter"
kind = "api_key"
api_key_env = "OPENROUTER_API_KEY"

[[mcp.servers]]
name = "docs"
transport = "streamable_http"
url = "https://mcp.example.com/mcp"

[[mcp.servers]]
name = "files"
transport = "stdio"
command = "mcp-files"
`)
	// The flows are the real ones, pointed at hosts this test names, so
	// that the test does not repeat the URLs src/auth owns.
	claude, codex := auth.ClaudeFlow(), auth.CodexFlow()
	claude.AuthorizeURL, claude.TokenURL = "https://login.example/authorize", "https://token.example:8443/token"
	codex.AuthorizeURL, codex.TokenURL = "https://codex-login.example/authorize", "https://codex-login.example/token"

	report, err := hosts.List(cfg, hosts.Inputs{
		Flows: map[string]auth.Flow{"claude": claude, "codex": codex},
		OTLP:  hosts.OTLP{Endpoint: "http://collector:4318", MetricsEndpoint: "https://metrics.example/v1/metrics"},
	})
	require.NoError(t, err)

	require.Equal(t, map[string][]string{
		"providers[0].base_url": {"openrouter.ai:443"},
		"providers[1].base_url": {"[::1]:8080"},
		// The oauth account's flow. The proxied account's flow is not
		// listed: its proxy refreshes the token.
		"accounts[0].provider": {"login.example:443", "token.example:8443"},
		"mcp.servers[0].url":   {"mcp.example.com:443"},
		// Traces and logs fall back to the general variable.
		"telemetry.otel:" + hosts.EnvOTLPEndpoint:        {"collector:4318"},
		"telemetry.otel:" + hosts.EnvOTLPMetricsEndpoint: {"metrics.example:443"},
	}, addresses(report))
	require.IsIncreasingf(t, hostLines(report), "hosts are sorted")

	// Both accounts of a built-in provider contact a host that is not
	// listed, the proxied one included, and the stdio server is named.
	require.Contains(t, report.NotCovered, `accounts[0].provider: the base URL of the built-in provider "claude" is not listed yet`)
	require.Contains(t, report.NotCovered, `accounts[1].provider: the base URL of the built-in provider "codex" is not listed yet`)
	require.Contains(t, report.NotCovered, `mcp.servers[1]: "files" is a stdio server, and what the command "mcp-files" contacts cannot be known`)
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

	off, err := hosts.List(load(t, ""), hosts.Inputs{OTLP: otlp})
	require.NoError(t, err)
	require.Empty(t, off.Hosts)

	// On without a variable: the exporter's default is not ours to state,
	// so each signal is reported as not covered.
	unnamed, err := hosts.List(load(t, "[telemetry]\notel = true\n"), hosts.Inputs{})
	require.NoError(t, err)
	require.Empty(t, unnamed.Hosts)
	require.Greater(t, len(unnamed.NotCovered), len(off.NotCovered))

	// A value the exporter could not use is an error, not a missing line.
	_, err = hosts.List(load(t, "[telemetry]\notel = true\n"), hosts.Inputs{OTLP: hosts.OTLP{Endpoint: "collector"}})
	require.Error(t, err)
}
