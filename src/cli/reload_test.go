package cli_test

import (
	"context"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/go-acp/acptest"
	"github.com/gfx-labs/towa/src/cli"
)

// A change to a config file reaches a session that is already open: its
// client is told the models as they now are, and a session on a model the
// file no longer names moves to the first one that it does.
func TestConfigFileChangeReachesAnOpenSession(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "towa"), 0o755))
	path := filepath.Join(dir, "towa", "config.yaml")
	write := func(names ...string) {
		var listed []string
		for _, name := range names {
			listed = append(listed, "{id: "+name+", context_window: 100000, max_output_tokens: 1000}")
		}
		next := filepath.Join(dir, "towa", "next.yaml")
		require.NoError(t, os.WriteFile(next, []byte(`
models: ["gateway:main/`+strings.Join(names, `", "gateway:main/`)+`"]
providers:
  - id: gateway
    kind: chat
    base_url: http://127.0.0.1:1/v1
    models: [`+strings.Join(listed, ", ")+`]
accounts:
  - {id: main, provider: gateway, kind: none}
`), 0o600))
		require.NoError(t, os.Rename(next, path))
	}
	write("first", "second")

	w, stdin, stdout := acptest.New(t)
	done := make(chan error, 1)
	go func() {
		done <- cli.Run(context.Background(), []string{"towa"}, cli.Host{
			Stdin: stdin, Stdout: stdout, Stderr: io.Discard,
			Environ: []string{"XDG_CONFIG_HOME=" + dir, "XDG_STATE_HOME=" + filepath.Join(dir, "state"), "HOME=" + dir},
		})
	}()
	w.Send(
		`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":1}}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"`+dir+`","mcpServers":[]}}`,
	)
	got, _ := w.Await(`0`, `1`)
	require.Nil(t, got[`1`].Error)
	var created acp.NewSessionResponse
	require.NoError(t, json.Unmarshal(got[`1`].Result, &created))
	id := string(created.SessionId)
	w.Send(`{"jsonrpc":"2.0","id":2,"method":"session/set_config_option","params":{"sessionId":"` + id + `","configId":"model","value":"gateway:main/second"}}`)
	got, _ = w.Await(`2`)
	require.Nil(t, got[`2`].Error)

	write("first", "third")
	var options []acp.SessionConfigOption
	for options == nil {
		message := w.Recv()
		var notice acp.SessionNotification
		if message.Method != "session/update" || json.Unmarshal(message.Params, &notice) != nil || notice.Update.ConfigOptionUpdate == nil {
			continue
		}
		require.Equal(t, id, string(notice.SessionId))
		options = notice.Update.ConfigOptionUpdate.ConfigOptions
	}
	current, choices := modelOf(t, options)
	require.Equal(t, acp.SessionConfigValueId("gateway:main/first"), current)
	require.Equal(t, []acp.SessionConfigValueId{"gateway:main/first", "gateway:main/third"}, choices)

	w.Send(`{"jsonrpc":"2.0","id":3,"method":"session/set_config_option","params":{"sessionId":"` + id + `","configId":"model","value":"gateway:main/third"}}`)
	got, _ = w.Await(`3`)
	require.Nil(t, got[`3`].Error)

	w.Close()
	require.NoError(t, <-done)
}

func modelOf(t *testing.T, options []acp.SessionConfigOption) (acp.SessionConfigValueId, []acp.SessionConfigValueId) {
	t.Helper()
	for _, o := range options {
		if o.Select != nil && o.Select.Id == "model" {
			var values []acp.SessionConfigValueId
			for _, c := range *o.Select.Options.Ungrouped {
				values = append(values, c.Value)
			}
			return o.Select.CurrentValue, values
		}
	}
	require.Fail(t, "no model option")
	return "", nil
}
