package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/support/httpclient"
	"github.com/gfx-labs/towa/src/tools/mcp/mcptest"
)

// The mcp commands from the command line to the token file, against an MCP
// server served by the SDK behind its bearer token check. The authorization
// server beside it is the hand-written one in mcptest, so this shows the
// commands and our side of the exchange, not a real authorization server.
func TestMCPLoginLogout(t *testing.T) {
	protected := mcptest.StartProtected(t, sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, nil), false)
	client := httpclient.New()
	env := builtInEnv()
	env.mcp = mcpEnv{openBrowser: func(ctx context.Context, page string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, page, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}}

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "config", "towa"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config", "towa", "config.yaml"), []byte(`
mcp:
  servers:
    - name: tracker
      transport: streamable_http
      url: "`+protected.URL+`"
    - name: files
      transport: stdio
      command: /bin/false
`), 0o600))

	run := func(stdin io.Reader, stderr io.Writer, args ...string) (string, error) {
		var stdout bytes.Buffer
		h := Host{
			Stdin: stdin, Stdout: &stdout, Stderr: stderr,
			Environ: []string{
				"XDG_CONFIG_HOME=" + filepath.Join(dir, "config"),
				"XDG_STATE_HOME=" + filepath.Join(dir, "state"),
			},
		}
		err := run(t.Context(), append([]string{"towa"}, args...), h, env)
		return stdout.String(), err
	}
	store := auth.NewStore(filepath.Join(dir, "state", "towa", "auth"))
	sum := sha256.Sum256([]byte("tracker"))
	file := filepath.Join(dir, "state", "towa", "auth", "mcp", hex.EncodeToString(sum[:])+".json")

	// A server that is not configured, or is not remote, is refused before
	// anything is sent.
	for _, args := range [][]string{
		{"mcp", "login", "nobody"}, {"mcp", "login", "files"}, {"mcp", "logout", "nobody"}, {"mcp", "logout", "files"},
	} {
		_, err := run(strings.NewReader(""), io.Discard, args...)
		require.ErrorIs(t, err, errMCPServer, args)
	}
	_, err := run(strings.NewReader(""), io.Discard, "mcp", "login")
	require.Error(t, err)
	require.Zero(t, protected.Requests())
	require.NoFileExists(t, file)

	out, err := run(strings.NewReader(""), io.Discard, "mcp", "login", "tracker")
	require.NoError(t, err)
	require.Contains(t, out, "tracker: logged in")
	info, err := os.Stat(file)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	first, err := store.LoadMCP("tracker")
	require.NoError(t, err)
	require.Equal(t, protected.URL, first.ServerURL)
	require.NotEmpty(t, first.AccessToken)
	require.NotContains(t, out, first.AccessToken)

	_, err = run(strings.NewReader(""), io.Discard, "mcp", "logout", "tracker")
	require.NoError(t, err)
	require.NoFileExists(t, file)
	_, err = run(strings.NewReader(""), io.Discard, "mcp", "logout", "tracker")
	require.ErrorIs(t, err, auth.ErrNotLoggedIn)

	// A manual login: the user reads the page from stderr, opens it on
	// another device, and pastes the address the browser was sent to.
	printed, stderr := io.Pipe()
	pasted, stdin := io.Pipe()
	noFollow := *client
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	user := make(chan error, 1)
	go func() {
		user <- func() error {
			var seen bytes.Buffer
			buf := make([]byte, 4096)
			page := regexp.MustCompile(`\S+/authorize\?\S+`)
			for page.Find(seen.Bytes()) == nil || !bytes.Contains(seen.Bytes(), []byte("Paste")) {
				n, err := printed.Read(buf)
				if err != nil {
					return err
				}
				seen.Write(buf[:n])
			}
			go func() { _, _ = io.Copy(io.Discard, printed) }()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, string(page.Find(seen.Bytes())), nil)
			if err != nil {
				return err
			}
			resp, err := noFollow.Do(req)
			if err != nil {
				return err
			}
			_ = resp.Body.Close()
			_, err = io.WriteString(stdin, resp.Header.Get("Location")+"\n")
			return err
		}()
	}()
	_, err = run(pasted, stderr, "mcp", "login", "--manual", "tracker")
	require.NoError(t, stderr.Close())
	require.NoError(t, err)
	require.NoError(t, <-user)
	second, err := store.LoadMCP("tracker")
	require.NoError(t, err)
	require.NotEqual(t, first.AccessToken, second.AccessToken)
}
