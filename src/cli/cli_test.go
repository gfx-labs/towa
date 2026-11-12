package cli_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/pkg/acp/acptest"
	"github.com/gfx-labs/towa/pkg/acp/harness"
	"github.com/gfx-labs/towa/src/cli"
	"github.com/gfx-labs/towa/src/config"
)

// The whole program: flags and a config file reach the logger, and a client
// on standard input and output can create a session without a model, then
// receives a typed failure instead of an echoed prompt.
func TestServeAnswersAClientOnStdio(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "towa"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "towa", "config.yaml"),
		[]byte("telemetry:\n  logs:\n    stderr:\n      format: json\n      level: error\n"), 0o600))

	w, stdin, stdout := acptest.New(t)
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- cli.Run(context.Background(), []string{"towa", "--log-level", "info"}, cli.Host{
			Stdin:   stdin,
			Stdout:  stdout,
			Stderr:  &stderr,
			Environ: []string{"XDG_CONFIG_HOME=" + dir, "XDG_STATE_HOME=" + filepath.Join(dir, "state"), "HOME=" + dir},
		})
	}()

	w.Send(
		`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":1}}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"`+dir+`","mcpServers":[]}}`,
	)
	got, _ := w.Await(`0`, `1`)
	require.Nil(t, got[`0`].Error)
	require.Nil(t, got[`1`].Error)
	var created struct {
		SessionID string `json:"sessionId"`
	}
	require.NoError(t, json.Unmarshal(got[`1`].Result, &created))

	w.Send(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"` + created.SessionID + `","prompt":[{"type":"text","text":"hello"}]}}`)
	got, others := w.Await(`2`)
	require.Empty(t, got[`2`].Result)
	require.NotNil(t, got[`2`].Error)
	require.Equal(t, -32603, got[`2`].Error.Code)
	data, ok := got[`2`].Error.Data.(map[string]any)
	require.True(t, ok, "error data: %#v", got[`2`].Error.Data)
	addition, present, err := harness.FromMeta(data)
	require.NoError(t, err)
	require.True(t, present)
	require.NotNil(t, addition.Stop)
	require.Equal(t, harness.StopRequest, addition.Stop.Kind)
	// A failed turn must not emit a successful-looking text response.
	replies := 0
	for _, m := range others {
		if strings.Contains(string(m.Params), `"sessionUpdate":"agent_message_chunk"`) {
			replies++
		}
	}
	require.Zero(t, replies, "notifications: %v", others)

	// The client closing standard input ends the program without an error.
	w.Close()
	require.NoError(t, <-done)
	// The file chose JSON and the flag overrode the file's level.
	require.Contains(t, stderr.String(), `"msg":"serving ACP on stdio"`)
}

func TestConfigRedactsCredentials(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(file, []byte(`
providers:
  - id: gateway
    kind: chat
    base_url: https://provider-user:provider-password@example.com/v1?key=provider-query
mcp:
  servers:
    - name: remote
      transport: streamable_http
      url: https://mcp-user:mcp-password@example.com/mcp?token=mcp-query
      headers:
        Authorization: Bearer header-secret
      headers_env:
        X-Key: MCP_KEY
    - name: local
      transport: stdio
      command: /bin/true
      args: [argument-secret]
      env:
        API_TOKEN: env-secret
hooks:
  - event: stop
    command: [/bin/true, hook-secret]
    timeout: 1s
websearch:
  endpoint: https://example.com/search?key=search-secret
`), 0o600))
	var stdout, stderr bytes.Buffer
	require.NoError(t, cli.Run(t.Context(), []string{"towa", "--config", file, "config"}, cli.Host{
		Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr,
		Environ: []string{"HOME=" + dir, "XDG_CONFIG_HOME=" + dir, "XDG_STATE_HOME=" + dir},
	}))
	for _, secret := range []string{
		"provider-user", "provider-password", "provider-query", "mcp-user", "mcp-password", "mcp-query",
		"header-secret", "env-secret", "argument-secret", "hook-secret", "search-secret",
	} {
		require.NotContains(t, stdout.String(), secret)
	}
	require.Contains(t, stdout.String(), "example.com/mcp")
	require.Contains(t, stdout.String(), "MCP_KEY")
	require.Contains(t, stdout.String(), "API_TOKEN")
	loaded, err := config.Load(config.Sources{Files: []string{file}})
	require.NoError(t, err)
	require.Equal(t, "https://provider-user:provider-password@example.com/v1?key=provider-query", loaded.Config.Providers[0].BaseURL)
	require.Equal(t, "Bearer header-secret", loaded.Config.MCP.Servers[0].Headers["Authorization"])
	require.Equal(t, "env-secret", loaded.Config.MCP.Servers[1].Env["API_TOKEN"])
	require.Equal(t, []string{"argument-secret"}, loaded.Config.MCP.Servers[1].Args)
	require.Equal(t, []string{"/bin/true", "hook-secret"}, loaded.Config.Hooks[0].Command)
	require.Equal(t, "https://example.com/search?key=search-secret", loaded.Config.WebSearch.Endpoint)
}
