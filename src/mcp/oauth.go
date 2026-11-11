package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/gfx-labs/acpharness/src/auth"
	"github.com/gfx-labs/acpharness/src/config"
)

// TokenStore is what this package needs from the auth store: the OAuth
// login of each remote server, kept by server name, and a lock that other
// processes honour so that a refresh token is used once. *auth.Store
// implements it.
type TokenStore interface {
	// LoadMCP fails with auth.ErrNotLoggedIn when the server has no login.
	LoadMCP(server string) (auth.MCPTokens, error)
	SaveMCP(server string, t auth.MCPTokens) error
	DeleteMCP(server string) error
	LockMCP(ctx context.Context, server string) (unlock func() error, err error)
}

// LoginRequiredError says a remote server refused a request for want of a
// valid OAuth login, and that only a person can fix it, with Login.
type LoginRequiredError struct {
	Server string
	// Err is what was seen: a 401, no stored login, a refused refresh.
	Err error
}

func (e *LoginRequiredError) Error() string {
	return fmt.Sprintf("mcp: server %q needs an OAuth login: %v", e.Server, e.Err)
}

func (e *LoginRequiredError) Unwrap() error { return e.Err }

// tokenSourceFor returns the source of the server's access token, or nil
// when there is no token to send: no store, or no login for this server at
// this URL.
func (m *Manager) tokenSourceFor(s Server) (*tokenSource, error) {
	if m.tokens == nil {
		return nil, nil
	}
	stored, err := m.tokens.LoadMCP(s.Name)
	switch {
	case errors.Is(err, auth.ErrNotLoggedIn):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("mcp: server %q: stored login: %w", s.Name, err)
	}
	if stored.ServerURL != s.URL {
		// The name now points somewhere else. The token is not sent there.
		m.logger.Warn("mcp: stored login is for another URL and is not used",
			"server", s.Name, "url", s.URL, "login_url", stored.ServerURL)
		return nil, nil
	}
	return &tokenSource{server: s, store: m.tokens, client: m.client, margin: m.refreshMargin, logger: m.logger, current: stored}, nil
}

// tokenSource hands out one server's access token and refreshes it.
type tokenSource struct {
	server Server
	store  TokenStore
	client *http.Client
	margin time.Duration
	logger *slog.Logger

	mu      sync.Mutex
	current auth.MCPTokens
	// rejected is an access token the server answered with 401. It is not
	// sent again.
	rejected string
}

func (ts *tokenSource) usable(t auth.MCPTokens) bool {
	if t.AccessToken == "" || t.AccessToken == ts.rejected {
		return false
	}
	// Zero means the authorization server gave no expiry.
	return t.ExpiresAtUnixMs == 0 || time.Now().Add(ts.margin).Before(time.UnixMilli(t.ExpiresAtUnixMs))
}

// reject records that the server refused the token.
func (ts *tokenSource) reject(token string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.rejected = token
}

// token returns an access token that is not expired and was not refused,
// refreshing under the store's lock when the current one is neither.
func (ts *tokenSource) token(ctx context.Context) (string, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.usable(ts.current) {
		return ts.current.AccessToken, nil
	}
	unlock, err := ts.store.LockMCP(ctx, ts.server.Name)
	if err != nil {
		return "", fmt.Errorf("mcp: server %q: lock stored login: %w", ts.server.Name, err)
	}
	defer func() {
		if err := unlock(); err != nil {
			ts.logger.Warn("mcp: unlock stored login", "server", ts.server.Name, "error", err)
		}
	}()
	// Another process may have refreshed while this one waited.
	stored, err := ts.store.LoadMCP(ts.server.Name)
	switch {
	case errors.Is(err, auth.ErrNotLoggedIn):
		return "", &LoginRequiredError{Server: ts.server.Name, Err: err}
	case err != nil:
		return "", fmt.Errorf("mcp: server %q: stored login: %w", ts.server.Name, err)
	case stored.ServerURL != ts.server.URL:
		return "", &LoginRequiredError{Server: ts.server.Name, Err: fmt.Errorf("the stored login is for %s", stored.ServerURL)}
	}
	if ts.usable(stored) {
		ts.current = stored
		return stored.AccessToken, nil
	}
	if stored.RefreshToken == "" || stored.TokenEndpoint == "" {
		return "", &LoginRequiredError{Server: ts.server.Name, Err: errors.New("the access token is no longer valid and there is no refresh token")}
	}
	cfg := oauth2.Config{
		ClientID:     stored.ClientID,
		ClientSecret: stored.ClientSecret,
		Endpoint:     oauth2.Endpoint{TokenURL: stored.TokenEndpoint},
	}
	if stored.ClientSecret == "" {
		// A public client names itself in the form. Left to detect the
		// style, oauth2 first sends the id as basic auth, and a server
		// that refuses that try may still count the refresh token as used.
		cfg.Endpoint.AuthStyle = oauth2.AuthStyleInParams
	}
	fresh, err := cfg.TokenSource(context.WithValue(ctx, oauth2.HTTPClient, ts.client), &oauth2.Token{RefreshToken: stored.RefreshToken}).Token()
	if err != nil {
		var refused *oauth2.RetrieveError
		if errors.As(err, &refused) && refused.ErrorCode == "invalid_grant" {
			return "", &LoginRequiredError{Server: ts.server.Name, Err: err}
		}
		return "", fmt.Errorf("mcp: server %q: refresh token: %w", ts.server.Name, err)
	}
	next := tokensFrom(fresh, stored)
	// The authorization server may have replaced the refresh token already,
	// so the save does not depend on the caller's context.
	if err := ts.store.SaveMCP(ts.server.Name, next); err != nil {
		return "", fmt.Errorf("mcp: server %q: save refreshed login: %w", ts.server.Name, err)
	}
	ts.current = next
	return next.AccessToken, nil
}

// tokensFrom is prev with the token an authorization server just issued.
// A response without a refresh token keeps the one there was.
func tokensFrom(t *oauth2.Token, prev auth.MCPTokens) auth.MCPTokens {
	next := prev
	next.AccessToken = t.AccessToken
	next.TokenType = t.TokenType
	if t.RefreshToken != "" {
		next.RefreshToken = t.RefreshToken
	}
	next.ExpiresAtUnixMs = 0
	if !t.Expiry.IsZero() {
		next.ExpiresAtUnixMs = t.Expiry.UnixMilli()
	}
	if scope, ok := t.Extra("scope").(string); ok && scope != "" {
		next.Scopes = strings.Fields(scope)
	}
	return next
}

// LoginOptions configures Login.
type LoginOptions struct {
	// Server is the remote server to log in to.
	Server Server
	Config config.MCPOAuth
	// HTTPClient is a client from src/httpclient.
	HTTPClient *http.Client
	Tokens     TokenStore
	// OpenURL shows the authorization page to the user, by opening a
	// browser or by printing the address.
	OpenURL func(ctx context.Context, url string) error
	// ClientName and ClientVersion name the harness to the server, and
	// ClientName names it in a dynamic client registration.
	ClientName    string
	ClientVersion string
}

// Login runs the OAuth authorization code flow for one remote server and
// stores the tokens. It asks the server which authorization server it
// trusts, registers a client there unless the server has one configured,
// and waits on a loopback address for the redirect.
func Login(ctx context.Context, opts LoginOptions) error {
	s := opts.Server
	switch {
	case s.Transport != config.MCPTransportStreamableHTTP && s.Transport != config.MCPTransportSSE:
		return fmt.Errorf("mcp: server %q is not remote, so it has no login", s.Name)
	case opts.HTTPClient == nil:
		return errors.New("mcp: LoginOptions.HTTPClient is required")
	case opts.Tokens == nil || opts.OpenURL == nil:
		return errors.New("mcp: LoginOptions.Tokens and OpenURL are required")
	}
	m := &Manager{client: opts.HTTPClient, logger: slog.New(slog.DiscardHandler)}
	client, _, err := m.httpClientFor(s)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", opts.Config.CallbackAddr)
	if err != nil {
		return fmt.Errorf("mcp: listen for the OAuth redirect on mcp.oauth.callback_addr: %w", err)
	}
	redirect := "http://" + listener.Addr().String() + opts.Config.CallbackPath
	type callback struct {
		result *sdkauth.AuthorizationResult
		err    error
	}
	callbacks := make(chan callback, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(opts.Config.CallbackPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		got := callback{result: &sdkauth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}}
		message := "Login received. You can close this page."
		switch {
		case q.Get("error") != "":
			got = callback{err: fmt.Errorf("the authorization server answered %q: %s", q.Get("error"), q.Get("error_description"))}
			message = "The login was refused."
		case got.result.Code == "":
			got = callback{err: errors.New("the redirect carried no code")}
			message = "The login was not completed."
		}
		select {
		case callbacks <- got:
		default:
			// A second redirect. The first one decides.
		}
		_, _ = fmt.Fprintln(w, message)
	})
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	defer func() { _ = server.Close() }()

	var saved *auth.MCPTokens
	handlerConfig := &sdkauth.AuthorizationCodeHandlerConfig{
		RedirectURL:         redirect,
		RequestRefreshToken: true,
		Client:              opts.HTTPClient,
		AuthorizationCodeFetcher: func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
			if err := opts.OpenURL(ctx, args.URL); err != nil {
				return nil, fmt.Errorf("open the authorization page: %w", err)
			}
			timeout := time.NewTimer(time.Duration(opts.Config.LoginTimeoutSeconds) * time.Second)
			defer timeout.Stop()
			select {
			case got := <-callbacks:
				return got.result, got.err
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-timeout.C:
				return nil, fmt.Errorf("no redirect arrived within mcp.oauth.login_timeout_seconds (%d)", opts.Config.LoginTimeoutSeconds)
			}
		},
		// Called once the code has been exchanged, with the client and the
		// token endpoint that were used. That is what a later refresh needs.
		NewTokenSource: func(_ context.Context, cfg *oauth2.Config, token *oauth2.Token) (oauth2.TokenSource, error) {
			t := tokensFrom(token, auth.MCPTokens{
				ServerURL:     s.URL,
				Scopes:        cfg.Scopes,
				TokenEndpoint: cfg.Endpoint.TokenURL,
				ClientID:      cfg.ClientID,
				ClientSecret:  cfg.ClientSecret,
			})
			saved = &t
			return oauth2.StaticTokenSource(token), nil
		},
	}
	if s.OAuthClientID != "" {
		handlerConfig.PreregisteredClient = &oauthex.ClientCredentials{ClientID: s.OAuthClientID}
		if s.OAuthClientSecret != "" {
			handlerConfig.PreregisteredClient.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: s.OAuthClientSecret}
		}
	} else {
		handlerConfig.DynamicClientRegistrationConfig = &sdkauth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				RedirectURIs:  []string{redirect},
				ClientName:    opts.ClientName,
				GrantTypes:    []string{"authorization_code", "refresh_token"},
				ResponseTypes: []string{"code"},
				// A command-line program cannot keep a client secret.
				TokenEndpointAuthMethod: "none",
			},
		}
	}
	handler, err := sdkauth.NewAuthorizationCodeHandler(handlerConfig)
	if err != nil {
		return fmt.Errorf("mcp: server %q: %w", s.Name, err)
	}

	if s.Transport == config.MCPTransportStreamableHTTP {
		// The SDK's transport runs the flow itself when the server answers
		// 401, then sends the request again with the token.
		session, err := sdk.NewClient(&sdk.Implementation{Name: opts.ClientName, Version: opts.ClientVersion}, nil).
			Connect(ctx, &sdk.StreamableClientTransport{Endpoint: s.URL, HTTPClient: client, OAuthHandler: handler}, nil)
		if err != nil {
			return fmt.Errorf("mcp: server %q: login: %w", s.Name, err)
		}
		_ = session.Close()
	} else {
		// The SSE transport has no such hook, so the 401 is fetched here.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
		if err != nil {
			return fmt.Errorf("mcp: server %q: %w", s.Name, err)
		}
		req.Header.Set("Accept", "text/event-stream")
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("mcp: server %q: login: %w", s.Name, err)
		}
		if resp.StatusCode != http.StatusUnauthorized {
			_ = resp.Body.Close()
			return fmt.Errorf("mcp: server %q answered %s without a token, so it asks for no login", s.Name, resp.Status)
		}
		// Authorize closes the body.
		if err := handler.Authorize(ctx, req, resp); err != nil {
			return fmt.Errorf("mcp: server %q: login: %w", s.Name, err)
		}
	}
	if saved == nil {
		return fmt.Errorf("mcp: server %q accepted a request without a token, so it asks for no login", s.Name)
	}
	unlock, err := opts.Tokens.LockMCP(ctx, s.Name)
	if err != nil {
		return fmt.Errorf("mcp: server %q: lock stored login: %w", s.Name, err)
	}
	if err := opts.Tokens.SaveMCP(s.Name, *saved); err != nil {
		return errors.Join(fmt.Errorf("mcp: server %q: save login: %w", s.Name, err), unlock())
	}
	return unlock()
}
