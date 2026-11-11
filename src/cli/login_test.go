package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"

	"github.com/gfx-labs/acpharness/src/auth"
	"github.com/gfx-labs/acpharness/src/httpclient"
)

// The account commands from the command line to the files: a login through
// the browser callback against a provider run by the test, what `accounts`
// then says, a refused login for a proxy-held account, and a logout.
func TestLoginAccountsLogout(t *testing.T) {
	var tokenRequests atomic.Int32
	mux := http.NewServeMux()
	// The provider's user approves at once and the browser is sent back.
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		back := q.Get("redirect_uri") + "?" + url.Values{"code": {"the-code"}, "state": {q.Get("state")}}.Encode()
		http.Redirect(w, r, back, http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, _ *http.Request) {
		tokenRequests.Add(1)
		_, _ = io.WriteString(w, `{"access_token":"access-1","refresh_token":"refresh-1","expires_in":3600,"scope":"user:inference"}`)
	})
	provider := httptest.NewServer(mux)
	t.Cleanup(provider.Close)

	flow := auth.ClaudeFlow()
	flow.AuthorizeURL, flow.TokenURL = provider.URL+"/authorize", provider.URL+"/token"
	env := loginEnv{
		flows: map[string]auth.Flow{"claude": flow},
		openBrowser: func(ctx context.Context, page string) error {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, page, nil)
			if err != nil {
				return err
			}
			resp, err := httpclient.New().Do(req)
			if err != nil {
				return err
			}
			return resp.Body.Close()
		},
	}

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "config", "acpharness"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config", "acpharness", "config.toml"), []byte(`
[[accounts]]
id = "work"
provider = "claude"
kind = "oauth"

[[accounts]]
id = "held"
provider = "claude"
kind = "proxied_oauth"

[[accounts]]
id = "key"
provider = "openrouter"
kind = "api_key"
api_key_env = "OPENROUTER_API_KEY"
`), 0o600))

	run := func(args ...string) (string, error) {
		var stdout bytes.Buffer
		h := Host{
			Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: io.Discard,
			Environ: []string{
				"XDG_CONFIG_HOME=" + filepath.Join(dir, "config"),
				"XDG_STATE_HOME=" + filepath.Join(dir, "state"),
			},
		}
		// The flags loadConfig reads from the root command.
		app := &cli.Command{
			Name:     "acpharness",
			Flags:    []cli.Flag{&cli.StringSliceFlag{Name: "config"}, &cli.StringSliceFlag{Name: "set"}},
			Commands: accountCommandsWith(h, env),
			Writer:   io.Discard, ErrWriter: io.Discard,
		}
		err := app.Run(t.Context(), append([]string{"acpharness"}, args...))
		return stdout.String(), err
	}
	file := filepath.Join(dir, "state", "acpharness", "auth", "work.json")

	// The form the design gives: the provider, then the flag.
	out, err := run("login", "claude", "--account", "work")
	require.NoError(t, err)
	require.Contains(t, out, "work is logged in")
	info, err := os.Stat(file)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	out, err = run("accounts")
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 4)
	require.Regexp(t, `^work\s+claude\s+oauth\s+logged in`, lines[1])
	require.Regexp(t, `^held\s+claude\s+proxied_oauth\s+held by a proxy`, lines[2])
	require.Regexp(t, `^key\s+openrouter\s+api_key\s+OPENROUTER_API_KEY is not set`, lines[3])
	require.NotContains(t, out, "access-1")

	// A proxy-held account: refused before anything is sent, and no file.
	for _, command := range []string{"login", "logout"} {
		args := []string{command, "--account", "held"}
		if command == "login" {
			args = []string{command, "claude", "--account", "held"}
		}
		_, err = run(args...)
		require.ErrorIs(t, err, auth.ErrProxied, command)
	}
	_, err = run("login", "claude", "--account", "nobody")
	require.ErrorIs(t, err, auth.ErrUnknownAccount)
	_, err = run("login", "codex", "--account", "work")
	require.Error(t, err)
	require.Equal(t, int32(1), tokenRequests.Load())
	entries, err := os.ReadDir(filepath.Dir(file))
	require.NoError(t, err)
	require.Len(t, entries, 2, fmt.Sprint(entries)) // work.json and work.lock

	_, err = run("logout", "--account", "work")
	require.NoError(t, err)
	require.NoFileExists(t, file)
	_, err = run("logout", "--account", "work")
	require.ErrorIs(t, err, auth.ErrNotLoggedIn)
}
