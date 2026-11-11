package mcp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/auth"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/httpclient"
	"github.com/gfx-labs/acpharness/src/mcp/mcptest"
)

// The server side of these tests is the SDK's MCP handler behind the SDK's
// bearer token check. The authorization server beside it is hand-written
// (see mcptest), so these tests show our client's side of the exchange and
// say nothing about a real authorization server's metadata, registration
// rules or error bodies.

// browser follows the authorization server's redirect to the loopback
// callback, as a browser does once the user has approved.
func browser(client *http.Client) func(context.Context, string) error {
	return func(ctx context.Context, page string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, page, nil)
		if err != nil {
			return err
		}
		go func() {
			if resp, err := client.Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}
}

func TestOAuthLoginAndRefresh(t *testing.T) {
	for _, transport := range []string{config.MCPTransportStreamableHTTP, config.MCPTransportSSE} {
		t.Run(transport, func(t *testing.T) {
			ctx := t.Context()
			sse := transport == config.MCPTransportSSE
			protected := mcptest.StartProtected(t, newTestServer(), sse)
			server := Server{Name: "team/tracker", Transport: transport, URL: protected.URL}
			store := auth.NewStore(t.TempDir())
			client := httpclient.New()
			cfg := defaultConfig(t)
			newManager := func() *Manager {
				return newTestManager(t, Options{Config: cfg, Servers: []Server{server}, HTTPClient: client, Tokens: store})
			}
			login := func() error {
				return Login(ctx, LoginOptions{
					Server: server, Config: cfg.OAuth, HTTPClient: client, Tokens: store, Out: io.Discard,
					ClientName: "acpharness-test", ClientVersion: "0", OpenURL: browser(client),
				})
			}

			// Without a login the server's refusal is reported as such.
			_, err := newManager().ListTools(ctx, server.Name)
			var required *LoginRequiredError
			require.ErrorAs(t, err, &required)

			require.NoError(t, login())
			first, err := store.LoadMCP(server.Name)
			require.NoError(t, err)
			require.Equal(t, protected.URL, first.ServerURL)
			require.NotEmpty(t, first.RefreshToken)
			require.NotEmpty(t, first.ClientID)

			// A new manager, as a new process would have, uses the stored
			// login.
			m := newManager()
			list, err := m.ListTools(ctx, server.Name)
			require.NoError(t, err)
			require.NotEmpty(t, list)
			require.Zero(t, protected.TokenRequests("refresh_token"))

			// The server stops accepting the access token. The next call
			// refreshes once, stores the new tokens and goes through.
			protected.RevokeAccess()
			res, err := m.CallTool(ctx, server.Name, "echo", []byte(`{"text":"after refresh"}`))
			require.NoError(t, err)
			require.Equal(t, "after refresh", textOf(t, res))
			require.Equal(t, 1, protected.TokenRequests("refresh_token"))
			second, err := store.LoadMCP(server.Name)
			require.NoError(t, err)
			require.NotEqual(t, first.AccessToken, second.AccessToken)
			require.NotEqual(t, first.RefreshToken, second.RefreshToken)
			require.Equal(t, first.ClientID, second.ClientID)

			// A stored login is not sent to another URL under the same name.
			moved := server
			moved.URL = mcptest.StartProtected(t, newTestServer(), sse).URL
			_, err = newTestManager(t, Options{Config: cfg, Servers: []Server{moved}, HTTPClient: client, Tokens: store}).
				ListTools(ctx, server.Name)
			require.ErrorAs(t, err, &required)

			// The refresh token is refused too. That is recorded, and the
			// error names the command that fixes it.
			protected.RevokeAccess()
			protected.RevokeRefresh()
			_, err = m.CallTool(ctx, server.Name, "echo", []byte(`{"text":"x"}`))
			require.ErrorAs(t, err, &required)
			require.Contains(t, required.Error(), "mcp login "+server.Name)
			require.Equal(t, 2, protected.TokenRequests("refresh_token"))
			dead, err := store.LoadMCP(server.Name)
			require.NoError(t, err)
			require.NotZero(t, dead.RefreshRejectedAtUnixMs)
			require.Equal(t, "invalid_grant", dead.RefreshRejection)

			// Neither this manager nor a new one sends the dead token again.
			_, err = m.CallTool(ctx, server.Name, "echo", []byte(`{"text":"x"}`))
			require.ErrorAs(t, err, &required)
			_, err = newManager().ListTools(ctx, server.Name)
			require.ErrorAs(t, err, &required)
			require.Equal(t, 2, protected.TokenRequests("refresh_token"))

			// A new login clears the record.
			require.NoError(t, login())
			again, err := store.LoadMCP(server.Name)
			require.NoError(t, err)
			require.Zero(t, again.RefreshRejectedAtUnixMs)
			_, err = newManager().ListTools(ctx, server.Name)
			require.NoError(t, err)
		})
	}
}

// A manual login: the page is printed, the user opens it elsewhere, and
// pastes the address the browser could not open.
func TestOAuthManualLogin(t *testing.T) {
	ctx := t.Context()
	protected := mcptest.StartProtected(t, newTestServer(), false)
	server := Server{Name: "tracker", Transport: config.MCPTransportStreamableHTTP, URL: protected.URL}
	store := auth.NewStore(t.TempDir())
	client := httpclient.New()
	cfg := defaultConfig(t)

	// The user: reads the page from what Login prints, opens it without
	// following the redirect, and types the address it was sent to.
	printed, out := io.Pipe()
	pasted, in := io.Pipe()
	noFollow := *client
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	user := make(chan error, 1)
	go func() {
		user <- func() error {
			lines := bufio.NewScanner(printed)
			for lines.Scan() {
				if !strings.Contains(lines.Text(), "/authorize?") {
					continue
				}
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSpace(lines.Text()), nil)
				if err != nil {
					return err
				}
				resp, err := noFollow.Do(req)
				if err != nil {
					return err
				}
				_ = resp.Body.Close()
				go func() { _, _ = io.Copy(io.Discard, printed) }()
				_, err = io.WriteString(in, resp.Header.Get("Location")+"\n")
				return err
			}
			return errors.New("no authorization page was printed")
		}()
	}()

	err := Login(ctx, LoginOptions{
		Server: server, Config: cfg.OAuth, HTTPClient: client, Tokens: store, Manual: true, In: pasted, Out: out,
		ClientName: "acpharness-test", ClientVersion: "0",
	})
	require.NoError(t, out.Close())
	require.NoError(t, err)
	require.NoError(t, <-user)

	m := newTestManager(t, Options{Config: cfg, Servers: []Server{server}, HTTPClient: client, Tokens: store})
	_, err = m.ListTools(ctx, server.Name)
	require.NoError(t, err)

	// An address from some other login is refused, and the stored login
	// stays as it was.
	before, err := store.LoadMCP(server.Name)
	require.NoError(t, err)
	err = Login(ctx, LoginOptions{
		Server: server, Config: cfg.OAuth, HTTPClient: client, Tokens: store, Manual: true,
		In: strings.NewReader("http://127.0.0.1:1/callback?code=abc&state=not-this-login\n"), Out: io.Discard,
		ClientName: "acpharness-test", ClientVersion: "0",
	})
	require.Error(t, err)
	after, err := store.LoadMCP(server.Name)
	require.NoError(t, err)
	require.Equal(t, before, after)
}
