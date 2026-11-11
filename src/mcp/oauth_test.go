package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/auth"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/httpclient"
)

// protectedServer is an MCP server behind the SDK's bearer token check,
// with an authorization server beside it. The MCP side and the token check
// are the SDK's. The authorization server is written here, because the
// SDK's own is in an internal package: it does metadata, dynamic
// registration, the code grant with PKCE, and refresh tokens that work
// once.
type protectedServer struct {
	URL string // the MCP endpoint

	mu sync.Mutex
	// challenges maps a code to the PKCE challenge it was issued for.
	challenges map[string]string
	clients    map[string]string // client id to redirect URI
	access     map[string]bool
	refresh    map[string]bool
	// tokenRequests counts requests to the token endpoint by grant type.
	tokenRequests map[string]int
}

func startProtectedServer(t *testing.T, transport string) *protectedServer {
	t.Helper()
	p := &protectedServer{
		challenges: map[string]string{}, clients: map[string]string{},
		access: map[string]bool{}, refresh: map[string]bool{}, tokenRequests: map[string]int{},
	}
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})
	issuer := ts.URL
	p.URL = ts.URL + "/mcp"

	server := newTestServer()
	getServer := func(*http.Request) *sdk.Server { return server }
	var handler http.Handler = sdk.NewStreamableHTTPHandler(getServer, nil)
	if transport == config.MCPTransportSSE {
		handler = sdk.NewSSEHandler(getServer, nil)
	}
	verify := func(_ context.Context, token string, _ *http.Request) (*sdkauth.TokenInfo, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if !p.access[token] {
			return nil, sdkauth.ErrInvalidToken
		}
		return &sdkauth.TokenInfo{Expiration: time.Now().Add(time.Hour)}, nil
	}
	mux.Handle("/mcp", sdkauth.RequireBearerToken(verify, &sdkauth.RequireBearerTokenOptions{
		ResourceMetadataURL: ts.URL + "/.well-known/oauth-protected-resource",
	})(handler))
	mux.Handle("/.well-known/oauth-protected-resource", sdkauth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource: p.URL, AuthorizationServers: []string{issuer},
	}))

	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.MarshalWrite(w, v)
	}
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer":                                issuer,
			"authorization_endpoint":                issuer + "/authorize",
			"token_endpoint":                        issuer + "/token",
			"registration_endpoint":                 issuer + "/register",
			"response_types_supported":              []string{"code"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"none"},
		})
	})
	mux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		var in oauthex.ClientRegistrationMetadata
		if err := json.UnmarshalRead(r.Body, &in); err != nil || len(in.RedirectURIs) != 1 {
			http.Error(w, "bad registration", http.StatusBadRequest)
			return
		}
		id := rand.Text()
		p.mu.Lock()
		p.clients[id] = in.RedirectURIs[0]
		p.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{
			"client_id": id, "redirect_uris": in.RedirectURIs, "token_endpoint_auth_method": "none",
		})
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		p.mu.Lock()
		redirect, known := p.clients[q.Get("client_id")]
		p.mu.Unlock()
		if !known || redirect != q.Get("redirect_uri") || q.Get("code_challenge_method") != "S256" ||
			q.Get("code_challenge") == "" || q.Get("resource") != p.URL {
			http.Error(w, "bad authorization request", http.StatusBadRequest)
			return
		}
		code := rand.Text()
		p.mu.Lock()
		p.challenges[code] = q.Get("code_challenge")
		p.mu.Unlock()
		http.Redirect(w, r, redirect+"?"+url.Values{"code": {code}, "state": {q.Get("state")}}.Encode(), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		grant := r.PostForm.Get("grant_type")
		p.mu.Lock()
		defer p.mu.Unlock()
		p.tokenRequests[grant]++
		ok := false
		switch grant {
		case "authorization_code":
			code := r.PostForm.Get("code")
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			ok = p.challenges[code] != "" && p.challenges[code] == base64.RawURLEncoding.EncodeToString(sum[:])
			delete(p.challenges, code)
		case "refresh_token":
			token := r.PostForm.Get("refresh_token")
			ok = p.refresh[token]
			delete(p.refresh, token)
		}
		if _, known := p.clients[r.PostForm.Get("client_id")]; !ok || !known {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		access, refresh := rand.Text(), rand.Text()
		p.access[access], p.refresh[refresh] = true, true
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": access, "refresh_token": refresh, "token_type": "Bearer", "expires_in": 3600,
		})
	})
	return p
}

// revokeAccess makes the server refuse every access token issued so far.
func (p *protectedServer) revokeAccess() {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.access)
}

func (p *protectedServer) requests(grant string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tokenRequests[grant]
}

func TestOAuthLoginAndRefresh(t *testing.T) {
	for _, transport := range []string{config.MCPTransportStreamableHTTP, config.MCPTransportSSE} {
		t.Run(transport, func(t *testing.T) {
			ctx := t.Context()
			protected := startProtectedServer(t, transport)
			server := Server{Name: "team/tracker", Transport: transport, URL: protected.URL}
			store := auth.NewStore(t.TempDir())
			client := httpclient.New()
			cfg := defaultConfig(t)
			newManager := func() *Manager {
				return newTestManager(t, Options{Config: cfg, Servers: []Server{server}, HTTPClient: client, Tokens: store})
			}

			// Without a login the server's refusal is reported as such.
			_, err := newManager().ListTools(ctx, server.Name)
			var login *LoginRequiredError
			require.ErrorAs(t, err, &login)

			// The browser is a client that follows the authorization
			// server's redirect to the loopback callback.
			require.NoError(t, Login(ctx, LoginOptions{
				Server: server, Config: cfg.OAuth, HTTPClient: client, Tokens: store,
				ClientName: "acpharness-test", ClientVersion: "0",
				OpenURL: func(ctx context.Context, address string) error {
					req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
					if err != nil {
						return err
					}
					go func() {
						if resp, err := client.Do(req); err == nil {
							_ = resp.Body.Close()
						}
					}()
					return nil
				},
			}))
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
			require.Zero(t, protected.requests("refresh_token"))

			// The server stops accepting the access token. The next call
			// refreshes once, stores the new tokens and goes through.
			protected.revokeAccess()
			res, err := m.CallTool(ctx, server.Name, "echo", []byte(`{"text":"after refresh"}`))
			require.NoError(t, err)
			require.Equal(t, "after refresh", textOf(t, res))
			require.Equal(t, 1, protected.requests("refresh_token"))
			second, err := store.LoadMCP(server.Name)
			require.NoError(t, err)
			require.NotEqual(t, first.AccessToken, second.AccessToken)
			require.NotEqual(t, first.RefreshToken, second.RefreshToken)
			require.Equal(t, first.ClientID, second.ClientID)

			// A stored login is not sent to another URL under the same name.
			moved := server
			moved.URL = startProtectedServer(t, transport).URL
			_, err = newTestManager(t, Options{Config: cfg, Servers: []Server{moved}, HTTPClient: client, Tokens: store}).
				ListTools(ctx, server.Name)
			require.ErrorAs(t, err, &login)

			// When the refresh token is refused too, a person has to log in.
			protected.revokeAccess()
			protected.mu.Lock()
			clear(protected.refresh)
			protected.mu.Unlock()
			_, err = m.CallTool(ctx, server.Name, "echo", []byte(`{"text":"x"}`))
			require.ErrorAs(t, err, &login)
		})
	}
}
