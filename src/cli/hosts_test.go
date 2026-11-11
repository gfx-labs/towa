package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"

	"github.com/gfx-labs/acpharness/src/auth"
)

// The hosts command from the argument list to its two streams: the list on
// standard output alone, and what it does not cover on standard error.
func TestHostsPrintsTheListAndWhatItDoesNotCover(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "extra.toml")
	require.NoError(t, os.WriteFile(file, []byte(`
[[providers]]
id = "openrouter"
kind = "chat"
base_url = "https://openrouter.ai/api/v1"

[[accounts]]
id = "work"
provider = "claude"
kind = "oauth"

[[mcp.servers]]
name = "docs"
transport = "sse"
url = "http://mcp.internal:9000/sse"
`), 0o600))
	flow := auth.ClaudeFlow()
	flow.AuthorizeURL, flow.TokenURL = "https://login.example/authorize", "https://login.example/token"

	var stdout, stderr bytes.Buffer
	h := Host{
		Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr,
		Environ: []string{"OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp.example:4318"},
	}
	app := &cli.Command{
		Name:     "acpharness",
		Flags:    []cli.Flag{&cli.StringSliceFlag{Name: "config"}, &cli.StringSliceFlag{Name: "set"}},
		Commands: []*cli.Command{hostsCommandWith(h, map[string]auth.Flow{"claude": flow})},
		Writer:   io.Discard, ErrWriter: io.Discard,
	}
	require.NoError(t, app.Run(t.Context(), []string{"acpharness", "--config", file, "--set", "telemetry.otel=true", "hosts"}))

	// The authorize and token endpoints share a host, which is one line.
	require.Equal(t, strings.Join([]string{
		"login.example:443\taccounts[0].provider",
		"mcp.internal:9000\tmcp.servers[0].url",
		"openrouter.ai:443\tproviders[0].base_url",
		"otlp.example:4318\ttelemetry.otel:OTEL_EXPORTER_OTLP_ENDPOINT",
		"",
	}, "\n"), stdout.String())
	for line := range strings.Lines(stderr.String()) {
		require.True(t, strings.HasPrefix(line, "hosts: not covered: "), line)
	}
	require.Contains(t, stderr.String(), `built-in provider "claude"`)
	require.Contains(t, stderr.String(), "embeddings")
}
