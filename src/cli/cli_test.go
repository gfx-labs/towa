package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/acp/acptest"
	"github.com/gfx-labs/acpharness/src/cli"
)

// The whole program: flags and a config file reach the logger, and a client
// on standard input and output gets through a prompt turn.
func TestServeAnswersAClientOnStdio(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "acpharness"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "acpharness", "config.toml"),
		[]byte("[telemetry.logs.stderr]\nformat = \"json\"\nlevel = \"error\"\n"), 0o600))

	w, stdin, stdout := acptest.New(t)
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- cli.Run(context.Background(), []string{"acpharness", "--log-level", "info"}, cli.Host{
			Stdin:   stdin,
			Stdout:  stdout,
			Stderr:  &stderr,
			Environ: []string{"XDG_CONFIG_HOME=" + dir},
		})
	}()

	w.Send(
		`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":1}}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/work","mcpServers":[]}}`,
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
	require.JSONEq(t, `{"stopReason":"end_turn"}`, string(got[`2`].Result))
	require.Len(t, others, 1)

	// The client closing standard input ends the program without an error.
	w.Close()
	require.NoError(t, <-done)
	// The file chose JSON and the flag overrode the file's level.
	require.Contains(t, stderr.String(), `"msg":"serving ACP on stdio"`)
}
