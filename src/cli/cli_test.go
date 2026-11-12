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
