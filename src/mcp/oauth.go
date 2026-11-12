package mcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
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
// valid OAuth login, and that only a person can fix it.
type LoginRequiredError struct {
	Server string
	// Err is what was seen: a 401, no stored login, a refused refresh.
	Err error
}

func (e *LoginRequiredError) Error() string {
	return fmt.Sprintf("mcp: server %q needs an OAuth login: %v: log in with \"acph mcp login %s\"", e.Server, e.Err, e.Server)
}

func (e *LoginRequiredError) Unwrap() error { return e.Err }

// deadRefreshCodes are the token endpoint's codes (RFC 6749 section 5.2)
// that mean the stored refresh token will not work again: the grant is
// gone, or the client it belongs to is. Any other failure, such as a
// network error or a 5xx, may pass and is tried again.
var deadRefreshCodes = []string{"invalid_grant", "invalid_client", "unauthorized_client"}

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
	// A refresh is tried only once the access token has stopped working,
	// so a login whose refresh was refused has no token worth sending.
	if t.AccessToken == "" || t.AccessToken == ts.rejected || t.RefreshRejectedAtUnixMs != 0 {
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

// refreshRefused is the error for a login whose refresh token is recorded
// as dead.
func (ts *tokenSource) refreshRefused(t auth.MCPTokens, cause error) error {
	err := fmt.Errorf("the token endpoint refused the stored refresh token (%s) at %s",
		t.RefreshRejection, time.UnixMilli(t.RefreshRejectedAtUnixMs).Format(time.RFC3339))
	if cause != nil {
		err = fmt.Errorf("%w: %w", err, cause)
	}
	return &LoginRequiredError{Server: ts.server.Name, Err: err}
}

// token returns an access token that is not expired and was not refused,
// refreshing under the store's lock when the current one is neither.
func (ts *tokenSource) token(ctx context.Context) (string, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.usable(ts.current) {
		return ts.current.AccessToken, nil
	}
	if ts.current.RefreshRejectedAtUnixMs != 0 {
		// Known dead. Neither the lock nor the endpoint is needed to say so.
		return "", ts.refreshRefused(ts.current, nil)
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
	// Another process may have refreshed, or found the refresh token dead,
	// while this one waited.
	stored, err := ts.store.LoadMCP(ts.server.Name)
	switch {
	case errors.Is(err, auth.ErrNotLoggedIn):
		return "", &LoginRequiredError{Server: ts.server.Name, Err: err}
	case err != nil:
		return "", fmt.Errorf("mcp: server %q: stored login: %w", ts.server.Name, err)
	case stored.ServerURL != ts.server.URL:
		return "", &LoginRequiredError{Server: ts.server.Name, Err: fmt.Errorf("the stored login is for %s", stored.ServerURL)}
	case stored.RefreshRejectedAtUnixMs != 0:
		ts.current = stored
		return "", ts.refreshRefused(stored, nil)
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
		if !errors.As(err, &refused) || !slices.Contains(deadRefreshCodes, refused.ErrorCode) {
			return "", fmt.Errorf("mcp: server %q: refresh token: %w", ts.server.Name, err)
		}
		// Recorded, so that neither this process nor another sends the
		// dead token again. A new login writes the file without the record.
		stored.RefreshRejectedAtUnixMs, stored.RefreshRejection = time.Now().UnixMilli(), refused.ErrorCode
		if saveErr := ts.store.SaveMCP(ts.server.Name, stored); saveErr != nil {
			return "", fmt.Errorf("mcp: server %q: record the refused refresh token: %w", ts.server.Name, errors.Join(err, saveErr))
		}
		ts.current = stored
		return "", ts.refreshRefused(stored, err)
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
	// Manual prints the authorization page and reads the address the
	// browser ends on from In. It is the login for a host without a
	// browser, and it starts no callback server.
	Manual bool
	// OpenURL opens the authorization page. A manual login does not use it.
	OpenURL func(ctx context.Context, url string) error
	// In is where a manual login reads the pasted address, and Out where
	// the user is told what to do.
	In  io.Reader
	Out io.Writer
	// ClientName and ClientVersion name the harness to the server, and
	// ClientName names it in a dynamic client registration.
	ClientName    string
	ClientVersion string
}

// Login runs the OAuth authorization code flow for one remote server and
// stores the tokens. It asks the server which authorization server it
// trusts, registers a client there unless the server has one configured,
// and takes the redirect on a loopback address or, for a manual login, as
// a pasted address.
func Login(ctx context.Context, opts LoginOptions) error {
	s := opts.Server
	switch {
	case s.Transport != config.MCPTransportStreamableHTTP && s.Transport != config.MCPTransportSSE:
		return fmt.Errorf("mcp: server %q is not remote, so it has no login", s.Name)
	case opts.HTTPClient == nil:
		return errors.New("mcp: LoginOptions.HTTPClient is required")
	case opts.Tokens == nil || opts.Out == nil:
		return errors.New("mcp: LoginOptions.Tokens and Out are required")
	case opts.Manual && opts.In == nil:
		return errors.New("mcp: a manual login needs LoginOptions.In")
	case !opts.Manual && opts.OpenURL == nil:
		return errors.New("mcp: LoginOptions.OpenURL is required unless the login is manual")
	}
	m := &Manager{client: opts.HTTPClient, logger: slog.New(slog.DiscardHandler)}
	client, _, err := m.httpClientFor(s)
	if err != nil {
		return err
	}

	redirect, fetch, stop, err := redirectReceiver(ctx, opts)
	if err != nil {
		return err
	}
	defer stop()

	var saved *auth.MCPTokens
	handlerConfig := &sdkauth.AuthorizationCodeHandlerConfig{
		RedirectURL:              redirect,
		RequestRefreshToken:      true,
		Client:                   opts.HTTPClient,
		AuthorizationCodeFetcher: fetch,
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
	// Under the lock, so that a refresh of the old login running in
	// another process does not write over the new one.
	unlock, err := opts.Tokens.LockMCP(ctx, s.Name)
	if err != nil {
		return fmt.Errorf("mcp: server %q: lock stored login: %w", s.Name, err)
	}
	if err := opts.Tokens.SaveMCP(s.Name, *saved); err != nil {
		return errors.Join(fmt.Errorf("mcp: server %q: save login: %w", s.Name, err), unlock())
	}
	return unlock()
}

// redirectReceiver returns the redirect URL to give the authorization
// server and the function that waits for the user to come back from it.
func redirectReceiver(ctx context.Context, opts LoginOptions) (redirect string, fetch sdkauth.AuthorizationCodeFetcher, stop func(), err error) {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", opts.Config.CallbackAddr)
	if err != nil {
		return "", nil, nil, fmt.Errorf("mcp: listen for the OAuth redirect on mcp.oauth.callback_addr: %w (a manual login with a fixed port needs no listener)", err)
	}
	redirect = "http://" + listener.Addr().String() + opts.Config.CallbackPath
	if opts.Manual {
		// The listener only chose the port. Nothing answers there, so the
		// browser shows an address it cannot open, and the user pastes it.
		if err := listener.Close(); err != nil {
			return "", nil, nil, err
		}
		return redirect, func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
			return pastedRedirect(ctx, opts, args.URL, redirect)
		}, func() {}, nil
	}

	type callback struct {
		result *sdkauth.AuthorizationResult
		err    error
	}
	callbacks := make(chan callback, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(opts.Config.CallbackPath, func(w http.ResponseWriter, r *http.Request) {
		result, err := authorizationResult(r.URL.Query())
		message := "acpharness has the authorization code. You can close this page."
		if err != nil {
			message = "The login was not completed. You can close this page."
		}
		select {
		case callbacks <- callback{result, err}:
		default:
			// A second redirect. The first one decides.
		}
		_, _ = fmt.Fprintln(w, message)
	})
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	fetch = func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
		if _, err := fmt.Fprintf(opts.Out, "Opening this page in a browser:\n\n%s\n\n", args.URL); err != nil {
			return nil, err
		}
		if err := opts.OpenURL(ctx, args.URL); err != nil {
			return nil, fmt.Errorf("no browser could be opened: %w (a manual login needs none)", err)
		}
		timeout := time.NewTimer(time.Duration(opts.Config.LoginTimeoutSeconds) * time.Second)
		defer timeout.Stop()
		select {
		case got := <-callbacks:
			return got.result, got.err
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		case <-timeout.C:
			return nil, fmt.Errorf("the browser did not come back within mcp.oauth.login_timeout_seconds (%d) (a manual login waits for a pasted address)", opts.Config.LoginTimeoutSeconds)
		}
	}
	return redirect, fetch, func() { _ = server.Close() }, nil
}

// pastedRedirect prints the page to open, on any device, and reads the
// address the browser was sent to.
func pastedRedirect(ctx context.Context, opts LoginOptions, page, redirect string) (*sdkauth.AuthorizationResult, error) {
	if _, err := fmt.Fprintf(opts.Out,
		"Open this page in a browser on any device:\n\n%s\n\nApprove the login. The browser then fails to open an address that starts with %s\nPaste that whole address:\n",
		page, redirect); err != nil {
		return nil, err
	}
	// The line is read in a goroutine so that a cancelled context ends the
	// login. The goroutine ends with the process or the next line.
	type line struct {
		text string
		err  error
	}
	read := make(chan line, 1)
	go func() {
		text, err := bufio.NewReader(opts.In).ReadString('\n')
		if errors.Is(err, io.EOF) && text != "" {
			err = nil
		}
		read <- line{text, err}
	}()
	var pasted line
	select {
	case pasted = <-read:
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
	if pasted.err != nil {
		return nil, fmt.Errorf("reading the pasted address: %w", pasted.err)
	}
	text := strings.TrimSpace(pasted.text)
	query := text
	if u, err := url.Parse(text); err == nil && u.Scheme != "" {
		query = u.RawQuery
	}
	values, err := url.ParseQuery(strings.TrimPrefix(query, "?"))
	if err != nil {
		return nil, fmt.Errorf("the pasted address has a query that cannot be read: %w", err)
	}
	return authorizationResult(values)
}

// authorizationResult reads the redirect's query. The SDK checks the state
// and the issuer against what it sent.
func authorizationResult(q url.Values) (*sdkauth.AuthorizationResult, error) {
	switch {
	case q.Get("error") != "":
		return nil, fmt.Errorf("the authorization server refused the login: %s %s", q.Get("error"), q.Get("error_description"))
	case q.Get("code") == "":
		return nil, errors.New("the redirect address carries no authorization code")
	case q.Get("state") == "":
		return nil, errors.New("the redirect address carries a code and no state: paste the whole address")
	}
	return &sdkauth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}, nil
}
