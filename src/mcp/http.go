package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sync/atomic"
)

type httpStateKey struct{}

// httpState separates concurrent calls' auth failures. Connection state is
// only a fallback for calls the SDK rejects before making an HTTP request.
type httpState struct {
	requested    atomic.Bool
	unauthorized atomic.Bool
}

func withHTTPState(ctx context.Context) (context.Context, *httpState) {
	state := &httpState{}
	return context.WithValue(ctx, httpStateKey{}, state), state
}

func requestState(req *http.Request) *httpState {
	state, _ := req.Context().Value(httpStateKey{}).(*httpState)
	return state
}

func (t *serverTransport) markUnauthorized(req *http.Request) {
	t.unauthorized.Store(true)
	if state := requestState(req); state != nil {
		state.unauthorized.Store(true)
	}
}

// notSentError is a request that failed before it got a connection, so the
// server cannot have acted on it.
type notSentError struct{ err error }

func (e *notSentError) Error() string { return e.err.Error() }
func (e *notSentError) Unwrap() error { return e.err }

// send marks failures before a connection was obtained. After that even a
// partial write may have reached the server. The SDK otherwise reports
// every failed request alike, whether the server saw it or not.
func (t *serverTransport) send(req *http.Request) (*http.Response, error) {
	if state := requestState(req); state != nil {
		state.requested.Store(true)
	}
	var connected atomic.Bool
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected.Store(true) }}
	resp, err := t.base.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	if err != nil && !connected.Load() {
		return nil, &notSentError{err: err}
	}
	if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		t.unauthorized.Store(false)
	}
	return resp, err
}

// httpClientFor returns the client one remote server is reached through:
// the manager's client, with the server's headers and its OAuth token added
// to requests that go to the server.
func (m *Manager) httpClientFor(s Server) (*http.Client, *serverTransport, error) {
	endpoint, err := url.Parse(s.URL)
	if err != nil {
		return nil, nil, fmt.Errorf("mcp: server %q: %w", s.Name, err)
	}
	tokens, err := m.tokenSourceFor(s)
	if err != nil {
		return nil, nil, err
	}
	transport := &serverTransport{endpoint: endpoint, headers: s.Headers, tokens: tokens, logger: m.logger}
	client, err := wrapClient(m.client, func(base http.RoundTripper) http.RoundTripper {
		transport.base = base
		return transport
	})
	if err != nil {
		return nil, nil, fmt.Errorf("mcp: server %q: %w", s.Name, err)
	}
	return client, transport, nil
}

// errDefaultTransport is a client whose transport is unset, which net/http
// reads as its default transport. A client from src/httpclient always has
// one.
var errDefaultTransport = errors.New("the HTTP client has no transport, so it is not a client from src/httpclient")

// wrapClient returns a copy of client whose requests pass through what wrap
// returns. The copy keeps the connection pool, proxy and trust settings of
// the client from src/httpclient, because only the round trip is wrapped.
//
// This is the one place in the package that touches a client's Transport
// field. The lint rule's own comment allows it: take a client from
// src/httpclient and assign its Transport to wrap it. Nothing is built here.
func wrapClient(client *http.Client, wrap func(http.RoundTripper) http.RoundTripper) (*http.Client, error) {
	if client == nil {
		return nil, errDefaultTransport
	}
	wrapped := *client
	if wrapped.Transport == nil {
		return nil, errDefaultTransport
	}
	wrapped.Transport = wrap(wrapped.Transport)
	return &wrapped, nil
}

// serverTransport adds a server's headers and token to the requests sent to
// that server. A request to another host, such as a redirect, gets neither.
type serverTransport struct {
	base     http.RoundTripper
	endpoint *url.URL
	headers  map[string]string
	tokens   *tokenSource
	logger   *slog.Logger
	// unauthorized is set when the server answers 401, or when there is no
	// valid token to send. The SDK's error does not carry the status.
	unauthorized atomic.Bool
}

func (t *serverTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != t.endpoint.Scheme || req.URL.Host != t.endpoint.Host {
		return t.base.RoundTrip(req)
	}
	// A RoundTripper must not change the request it is given.
	req = req.Clone(req.Context())
	for name, value := range t.headers {
		req.Header.Set(name, value)
	}
	var token string
	if t.tokens != nil {
		var err error
		token, err = t.tokens.token(req.Context())
		if err != nil {
			t.markUnauthorized(req)
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := t.send(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	if t.tokens == nil {
		t.markUnauthorized(req)
		return resp, nil
	}
	// The server refused a token that had not expired by the clock. It did
	// not act on the request, so the request is sent once more with a
	// refreshed token, when its body can be read again.
	t.tokens.reject(token)
	retry := req.Clone(req.Context())
	if req.Body != nil && req.Body != http.NoBody {
		if req.GetBody == nil {
			t.markUnauthorized(req)
			return resp, nil
		}
		body, err := req.GetBody()
		if err != nil {
			t.markUnauthorized(req)
			return resp, nil
		}
		retry.Body = body
	}
	fresh, err := t.tokens.token(req.Context())
	if err != nil {
		if retry.Body != nil {
			_ = retry.Body.Close()
		}
		t.markUnauthorized(req)
		t.logger.Warn("mcp: no token after the server refused one", "server", t.tokens.server.Name, "error", err)
		return resp, nil
	}
	_ = resp.Body.Close()
	retry.Header.Set("Authorization", "Bearer "+fresh)
	resp, err = t.send(retry)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		t.tokens.reject(fresh)
		t.markUnauthorized(req)
	}
	return resp, err
}

// loginRequired preserves an auth failure until a request succeeds. An SSE
// transport may close after a 401, so later calls get a connection-closed
// error without making another request that could set the flag again.
func (t *serverTransport) loginRequired(server string, state *httpState, err error) error {
	if t == nil || err == nil {
		return err
	}
	unauthorized := t.unauthorized.Load()
	if state != nil && (state.requested.Load() || state.unauthorized.Load()) {
		unauthorized = state.unauthorized.Load()
	}
	if !unauthorized {
		return err
	}
	return &LoginRequiredError{Server: server, Err: err}
}
