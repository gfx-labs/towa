package cli

import (
	"bytes"
	"context"
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
	env := builtInEnv()
	env.login = loginEnv{
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
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config", "acpharness", "config.yaml"), []byte(`
# The model names an account that no login has made yet.
models: ["claude:fresh/claude-opus-4-7"]
accounts:
  - id: work
    provider: claude
    kind: oauth
  - id: held
    provider: claude
    kind: proxied_oauth
  - id: key
    provider: openrouter
    kind: api_key
    api_key_env: OPENROUTER_API_KEY
`), 0o600))

	var printed bytes.Buffer
	run := func(args ...string) (string, error) {
		var stdout bytes.Buffer
		printed.Reset()
		h := Host{
			Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &printed,
			Environ: []string{
				"XDG_CONFIG_HOME=" + filepath.Join(dir, "config"),
				"XDG_STATE_HOME=" + filepath.Join(dir, "state"),
			},
		}
		err := run(t.Context(), append([]string{"acpharness"}, args...), h, env)
		return stdout.String(), err
	}
	file := filepath.Join(dir, "state", "acpharness", "auth", "auth.json")
	store := auth.NewStore(filepath.Dir(file))

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
	require.Len(t, lines, 5)
	require.Regexp(t, `^work\s+claude\s+oauth\s+logged in`, lines[1])
	require.Regexp(t, `^held\s+claude\s+proxied_oauth\s+held by a proxy`, lines[2])
	require.Regexp(t, `^key\s+openrouter\s+api_key\s+OPENROUTER_API_KEY is not set`, lines[3])
	require.Regexp(t, `^fresh\s+claude\s+oauth\s+not logged in`, lines[4])
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
	_, err = run("logout", "--account", "nobody")
	require.ErrorIs(t, err, auth.ErrUnknownAccount)
	_, err = run("login", "codex", "--account", "work")
	require.Error(t, err)
	_, err = run("login", "claude")
	require.Error(t, err)
	// The three ways to log in exclude each other, and nothing is sent.
	_, err = run("login", "claude", "--account", "work", "--manual", "--device")
	require.ErrorContains(t, err, "--manual and --device")
	_, err = run("login", "claude", "--code", "the-code#state")
	require.ErrorIs(t, err, auth.ErrNoPendingLogin)
	require.Equal(t, int32(1), tokenRequests.Load())

	// An account that no config file declares: the login creates it. Nobody
	// is at this terminal, so the page is printed and the code comes in a
	// later run, which needs the provider alone to find the account. A
	// second new account is not held up by the first one's missing login.
	out, err = run("login", "claude", "--account", "second", "--manual")
	require.NoError(t, err)
	require.Contains(t, out, "account second: the claude login is waiting")
	out, err = run("login", "claude", "--account", "fresh", "--manual")
	require.NoError(t, err)
	require.Contains(t, out, "waiting for its code")
	var page *url.URL
	for line := range strings.SplitSeq(printed.String(), "\n") {
		if strings.HasPrefix(line, provider.URL) {
			page, err = url.Parse(line)
			require.NoError(t, err)
		}
	}
	require.NotNil(t, page)
	_, err = store.Load("fresh")
	require.ErrorIs(t, err, auth.ErrNotLoggedIn)
	code := "the-code#" + page.Query().Get("state")
	_, err = run("login", "claude", "--account", "work", "--code", code)
	require.Error(t, err)
	require.Equal(t, int32(1), tokenRequests.Load())
	out, err = run("login", "claude", "--code", code)
	require.NoError(t, err)
	require.Contains(t, out, "fresh is created and logged in")
	_, err = run("login", "claude", "--code", code)
	require.ErrorIs(t, err, auth.ErrNoPendingLogin)
	require.Equal(t, int32(2), tokenRequests.Load())
	out, err = run("accounts")
	require.NoError(t, err)
	require.Regexp(t, `(?m)^fresh\s+claude\s+oauth\s+logged in`, out)

	_, err = run("logout", "--account", "work")
	require.NoError(t, err)
	_, err = store.Load("work")
	require.ErrorIs(t, err, auth.ErrNotLoggedIn)
	_, err = run("logout", "--account", "work")
	require.ErrorIs(t, err, auth.ErrNotLoggedIn)
	// The created account is in auth.json alone, and goes with its login.
	// The chain still names it, so it is an account without a login.
	_, err = run("logout", "--account", "fresh")
	require.NoError(t, err)
	_, err = run("logout", "--account", "fresh")
	require.ErrorIs(t, err, auth.ErrNotLoggedIn)
}
