package hosts_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/hosts"
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

// The list itself, from a configuration file to the printed lines, is tested
// through the command in src/cli/hosts_test.go.

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
