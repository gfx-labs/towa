// Package mcptest runs an MCP server that asks for an OAuth login, for the
// tests of the MCP client and of the mcp commands.
//
// The MCP side and the bearer token check are the MCP Go SDK's. The
// authorization server is written here by hand, because the SDK keeps its
// own in an internal package and no real one can be reached from a test.
// It follows the RFCs as this file reads them: metadata (RFC 8414), dynamic
// registration (RFC 7591), the code grant with PKCE (RFC 7636) and refresh
// tokens that work once. A test against it shows that our client completes
// that exchange and stores what it is given. It does not show how a real
// authorization server words its metadata, what it requires at
// registration, or what its error bodies look like.
package mcptest

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
)

// Protected is an MCP server behind a bearer token check, with its
// authorization server on the same host.
type Protected struct {
	// URL is the MCP endpoint.
	URL string

	mu sync.Mutex
	// challenges maps a code to the PKCE challenge it was issued for.
	challenges map[string]string
	// clients maps a client id to its redirect URI.
	clients       map[string]string
	access        map[string]bool
	refresh       map[string]bool
	tokenRequests map[string]int
	requests      int
}

// StartProtected serves server over the SDK's streamable HTTP handler, or
// its SSE handler when sse is set, until the test ends.
func StartProtected(t testing.TB, server *sdk.Server, sse bool) *Protected {
	t.Helper()
	p := &Protected{
		challenges: map[string]string{}, clients: map[string]string{},
		access: map[string]bool{}, refresh: map[string]bool{}, tokenRequests: map[string]int{},
	}
	mux := http.NewServeMux()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.requests++
		p.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})
	issuer := ts.URL
	p.URL = ts.URL + "/mcp"

	getServer := func(*http.Request) *sdk.Server { return server }
	var handler http.Handler = sdk.NewStreamableHTTPHandler(getServer, nil)
	if sse {
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
	// The user approves at once and the browser is sent back.
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

// RevokeAccess makes the server refuse every access token issued so far.
func (p *Protected) RevokeAccess() {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.access)
}

// RevokeRefresh makes the token endpoint refuse every refresh token issued
// so far.
func (p *Protected) RevokeRefresh() {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.refresh)
}

// TokenRequests is how many requests with the given grant type the token
// endpoint has had.
func (p *Protected) TokenRequests(grant string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tokenRequests[grant]
}

// Requests is how many requests the host has had, on any path.
func (p *Protected) Requests() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests
}
