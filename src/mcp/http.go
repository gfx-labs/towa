package mcp

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync/atomic"
)

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
			t.unauthorized.Store(true)
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	if t.tokens == nil {
		t.unauthorized.Store(true)
		return resp, nil
	}
	// The server refused a token that had not expired by the clock. It did
	// not act on the request, so the request is sent once more with a
	// refreshed token, when its body can be read again.
	t.tokens.reject(token)
	retry := req.Clone(req.Context())
	if req.Body != nil && req.Body != http.NoBody {
		if req.GetBody == nil {
			t.unauthorized.Store(true)
			return resp, nil
		}
		body, err := req.GetBody()
		if err != nil {
			t.unauthorized.Store(true)
			return resp, nil
		}
		retry.Body = body
	}
	fresh, err := t.tokens.token(req.Context())
	if err != nil {
		if retry.Body != nil {
			_ = retry.Body.Close()
		}
		t.unauthorized.Store(true)
		t.logger.Warn("mcp: no token after the server refused one", "server", t.tokens.server.Name, "error", err)
		return resp, nil
	}
	_ = resp.Body.Close()
	retry.Header.Set("Authorization", "Bearer "+fresh)
	resp, err = t.base.RoundTrip(retry)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		t.tokens.reject(fresh)
		t.unauthorized.Store(true)
	}
	return resp, err
}

// loginRequired turns err into a *LoginRequiredError when the server
// answered 401 since the last time this was asked.
func (t *serverTransport) loginRequired(server string, err error) error {
	if t == nil || err == nil || !t.unauthorized.Swap(false) {
		return err
	}
	return &LoginRequiredError{Server: server, Err: err}
}
