// Package openai is the client for the OpenAI Responses API. One client
// serves two endpoints, chosen by the credential's scheme: an API key goes
// to api.openai.com or any Responses-compatible base URL, and a ChatGPT
// OAuth token goes to the Codex backend at chatgpt.com.
package openai

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"sync"

	"github.com/gfx-labs/sse"
	"github.com/valyala/bytebufferpool"

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/provider"
)

// Endpoints and the values the Codex backend expects.
const (
	// DefaultBaseURL is where an API key request goes when Config.BaseURL is
	// empty.
	DefaultBaseURL = "https://api.openai.com/v1"
	// DefaultCodexBaseURL is where an OAuth request goes when
	// Config.CodexBaseURL is empty.
	DefaultCodexBaseURL = "https://chatgpt.com/backend-api/codex"
	// Originator is the originator header of an OAuth request. jcode's probe
	// of 2026-11-05 found the backend accepts any value.
	Originator = "towa"
	// DefaultErrorBodyBytes is how much of an error response is read when
	// Config.ErrorBodyBytes is zero. An error body is a JSON object of a few
	// hundred bytes. A gateway's HTML error page is the large case.
	DefaultErrorBodyBytes = 1 << 20

	// ProviderAPI and ProviderCodex are the Item.Provider values of items
	// from the two endpoints. They differ because encrypted reasoning from
	// one is not known to be readable by the other.
	ProviderAPI   = "openai"
	ProviderCodex = "openai-codex"
)

// Transport is how a request reaches the endpoint.
type Transport string

// Transports of a request.
const (
	// TransportSSE posts the request and reads server-sent events.
	TransportSSE Transport = "sse"
	// TransportWebSocket sends the request on a WebSocket that is kept for
	// the next request with the same cache key.
	TransportWebSocket Transport = "websocket"
)

// Config configures a Client.
type Config struct {
	// HTTPClient sends every request, including the WebSocket handshake. It
	// comes from src/support/httpclient.
	HTTPClient *http.Client
	// BaseURL is the root of the API key endpoint, without /responses.
	// Empty means DefaultBaseURL.
	BaseURL string
	// CodexBaseURL is the root of the OAuth endpoint. Empty means
	// DefaultCodexBaseURL.
	CodexBaseURL string
	// Provider is the Item.Provider value of items from the API key
	// endpoint. Empty means ProviderAPI. A second Responses-compatible
	// endpoint needs its own value, so that its items are not sent to
	// OpenAI and OpenAI's are not sent to it.
	Provider string
	// UserAgent is sent when it is not empty.
	UserAgent string
	// Transport is TransportSSE when empty.
	Transport Transport
	// MaxIdleSockets is how many WebSocket connections are kept open
	// between requests, one per conversation. It must be set when Transport
	// is TransportWebSocket.
	MaxIdleSockets int
	// ParallelToolCalls lets the model ask for several tools in one
	// response.
	ParallelToolCalls bool
	// CompactThreshold asks the server to compact the conversation itself
	// once it holds this many tokens. The result arrives as a Native item
	// of kind "compaction". Zero leaves server-side compaction off.
	CompactThreshold int64
	// ErrorBodyBytes is how much of an error response is read. Zero means
	// DefaultErrorBodyBytes. A longer body is reported as such in the
	// error's message.
	ErrorBodyBytes int64
}

// Client sends requests to the Responses API. It is safe for concurrent
// use.
type Client struct {
	cfg Config

	mu   sync.Mutex
	idle map[socketKey]*socket
	// idleOrder lists the keys of idle from the longest idle to the newest.
	idleOrder []socketKey
	closed    bool
}

var _ provider.Provider = (*Client)(nil)

// New returns a client. It fails when the configuration cannot work.
func New(cfg Config) (*Client, error) {
	if cfg.HTTPClient == nil {
		return nil, errors.New("openai: Config.HTTPClient is nil")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.CodexBaseURL == "" {
		cfg.CodexBaseURL = DefaultCodexBaseURL
	}
	if cfg.Provider == "" {
		cfg.Provider = ProviderAPI
	}
	if cfg.Provider == ProviderCodex {
		return nil, fmt.Errorf("openai: Config.Provider %q is the name of the OAuth endpoint", ProviderCodex)
	}
	if cfg.Transport == "" {
		cfg.Transport = TransportSSE
	}
	if cfg.Transport != TransportSSE && cfg.Transport != TransportWebSocket {
		return nil, fmt.Errorf("openai: Config.Transport %q is not %q or %q", cfg.Transport, TransportSSE, TransportWebSocket)
	}
	if cfg.Transport == TransportWebSocket && cfg.MaxIdleSockets <= 0 {
		return nil, fmt.Errorf("openai: Config.MaxIdleSockets %d is not positive, and the websocket transport needs it", cfg.MaxIdleSockets)
	}
	if cfg.CompactThreshold < 0 {
		return nil, fmt.Errorf("openai: Config.CompactThreshold %d is negative", cfg.CompactThreshold)
	}
	if cfg.ErrorBodyBytes < 0 {
		return nil, fmt.Errorf("openai: Config.ErrorBodyBytes %d is negative", cfg.ErrorBodyBytes)
	}
	if cfg.ErrorBodyBytes == 0 {
		cfg.ErrorBodyBytes = DefaultErrorBodyBytes
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	cfg.CodexBaseURL = strings.TrimRight(cfg.CodexBaseURL, "/")
	return &Client{cfg: cfg, idle: map[socketKey]*socket{}}, nil
}

// endpoint is what a credential's scheme selects.
type endpoint struct {
	base string
	// provider tags the items the endpoint produces.
	provider string
	codex    bool
}

// ContextName returns the provenance of this credential's Responses items.
func (c *Client) ContextName(cred auth.Credential) (string, error) {
	name, perr := c.contextName(cred)
	if perr != nil {
		return "", perr
	}
	return name, nil
}

func (c *Client) contextName(cred auth.Credential) (string, *provider.Error) {
	switch cred.Scheme {
	case auth.SchemeAPIKey, auth.SchemeNone:
		return c.cfg.Provider, nil
	case auth.SchemeOAuth:
		return ProviderCodex, nil
	default:
		return "", invalidRequest(fmt.Sprintf("account %q has credential scheme %q, which is not %q, %q or %q",
			cred.Account, cred.Scheme, auth.SchemeAPIKey, auth.SchemeOAuth, auth.SchemeNone))
	}
}

func (c *Client) endpoint(cred auth.Credential) (endpoint, *provider.Error) {
	name, perr := c.contextName(cred)
	if perr != nil {
		return endpoint{}, perr
	}
	if cred.Token == "" && cred.Scheme != auth.SchemeNone {
		return endpoint{}, &provider.Error{
			Class: provider.Fallback, Reason: provider.ReasonAuth,
			Message: fmt.Sprintf("account %q has no token", cred.Account),
		}
	}
	if cred.Scheme == auth.SchemeOAuth {
		return endpoint{base: c.cfg.CodexBaseURL, provider: name, codex: true}, nil
	}
	return endpoint{base: c.cfg.BaseURL, provider: name}, nil
}

// header returns the headers of a request. A proxied credential gets the
// same headers: its token is a placeholder the proxy replaces.
func (c *Client) header(ep endpoint, req provider.Request) http.Header {
	h := http.Header{}
	// An endpoint that takes no credential gets no header, not an empty one.
	if req.Credential.Scheme != auth.SchemeNone {
		h.Set("Authorization", "Bearer "+req.Credential.Token)
	}
	h.Set("Content-Type", "application/json")
	if c.cfg.UserAgent != "" {
		h.Set("User-Agent", c.cfg.UserAgent)
	}
	if !ep.codex {
		return h
	}
	h.Set("originator", Originator)
	// openai/codex ce0323f2 build_routing_hint_header: the backend routes
	// on the model before it has read the body.
	h.Set("x-codex-routing-hint", "model="+req.Model)
	if req.CacheKey != "" {
		// The backend takes cache affinity from this header as well as from
		// prompt_cache_key in the body.
		h.Set("session-id", req.CacheKey)
	}
	if req.Credential.ChatGPTAccountID != "" {
		h.Set("chatgpt-account-id", req.Credential.ChatGPTAccountID)
	}
	return h
}

// Stream sends one request and yields the response's events.
func (c *Client) Stream(ctx context.Context, req provider.Request) iter.Seq2[provider.Event, error] {
	return func(yield func(provider.Event, error) bool) {
		if req.Credential.Account == "" {
			yield(provider.Event{}, invalidRequest("the request credential names no account"))
			return
		}
		ep, perr := c.endpoint(req.Credential)
		if perr != nil {
			yield(provider.Event{}, perr)
			return
		}
		p := &parser{provider: ep.provider, model: req.Model, account: req.Credential.Account}
		// One stream event can produce several provider events, and the
		// range must not be called again once it has returned false.
		emit := func(ev provider.Event) bool {
			if p.stopped {
				return false
			}
			if !yield(ev, nil) {
				p.stopped = true
			}
			return !p.stopped
		}
		if c.cfg.Transport == TransportWebSocket {
			perr = c.streamWebSocket(ctx, ep, req, p, emit)
		} else {
			perr = c.streamSSE(ctx, ep, req, p, emit)
		}
		if perr != nil && !p.stopped {
			yield(provider.Event{}, perr)
		}
	}
}

func (c *Client) streamSSE(ctx context.Context, ep endpoint, req provider.Request, p *parser, emit func(provider.Event) bool) *provider.Error {
	body, perr := c.buildRequest(ep, req)
	if perr != nil {
		return perr
	}
	buf := newPooledBody()
	defer buf.release()
	if err := json.MarshalWrite(buf.buf, body); err != nil {
		return invalidRequest("encode request: " + err.Error())
	}
	resp, perr := c.post(ctx, ep.base+"/responses", c.header(ep, req), buf, "text/event-stream")
	if perr != nil {
		return perr
	}
	defer resp.Body.Close() //nolint:errcheck // nothing to do about a close error on a body being abandoned

	for ev, err := range sse.Events(resp.Body) {
		if err != nil {
			return provider.TransportError(ctx, "read stream", err)
		}
		if ev.Data == nil || string(ev.Data) == "[DONE]" {
			continue
		}
		done, perr := p.handle(ev.Data, emit)
		if perr != nil {
			return perr
		}
		// The terminal event ends the response even when the connection
		// stays open.
		if done || p.stopped {
			return nil
		}
	}
	return &provider.Error{
		Class: provider.Retry, Reason: provider.ReasonNetwork,
		Message: "the stream ended before the response was completed",
	}
}

// pooledBody is a request body in a pooled buffer. net/http may still read
// a request body after the response has arrived, and says to wait for the
// body's Close before reusing it, so release waits for every reader handed
// out.
type pooledBody struct {
	buf     *bytebufferpool.ByteBuffer
	readers sync.WaitGroup
}

func newPooledBody() *pooledBody { return &pooledBody{buf: bytebufferpool.Get()} }

func (b *pooledBody) reader() io.ReadCloser {
	b.readers.Add(1)
	return &pooledReader{Reader: bytes.NewReader(b.buf.B), body: b}
}

// release returns the buffer once the transport has closed every reader.
// It is called after the response body is closed.
func (b *pooledBody) release() {
	b.readers.Wait()
	bytebufferpool.Put(b.buf)
}

type pooledReader struct {
	*bytes.Reader
	body   *pooledBody
	closed sync.Once
}

func (r *pooledReader) Close() error {
	r.closed.Do(r.body.readers.Done)
	return nil
}

// post sends a JSON body and returns the response when its status is 200.
// Any other status is returned as a classified error.
func (c *Client) post(ctx context.Context, url string, header http.Header, body *pooledBody, accept string) (*http.Response, *provider.Error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return nil, invalidRequest("build request: " + err.Error())
	}
	httpReq.Header = header
	httpReq.Header.Set("Accept", accept)
	// GetBody is what a redirect or a retried connection reads the body
	// from again.
	httpReq.Body = body.reader()
	httpReq.ContentLength = int64(body.buf.Len())
	httpReq.GetBody = func() (io.ReadCloser, error) { return body.reader(), nil }

	resp, err := c.cfg.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, provider.TransportError(ctx, "send request", err)
	}
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	defer resp.Body.Close() //nolint:errcheck // the status already decides the error
	return nil, c.statusError(ctx, resp)
}

// statusError reads the body of a response that is not 200 and classifies
// it.
func (c *Client) statusError(ctx context.Context, resp *http.Response) *provider.Error {
	return provider.StatusError(ctx, resp, c.cfg.ErrorBodyBytes, func(body []byte) *provider.Error {
		return classify(resp.StatusCode, resp.Header, parseErrorBody(body))
	})
}
