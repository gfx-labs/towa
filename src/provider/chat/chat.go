// Package chat is the Chat Completions streaming client. It serves
// OpenRouter, z.ai, DeepSeek, OpenCode Go and any other endpoint that speaks
// the protocol, and adds the OpenRouter extensions when the provider's
// configuration turns them on.
package chat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"mime"
	"net/http"
	"strings"

	"github.com/gfx-labs/sse"
	"github.com/valyala/bytebufferpool"

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
)

// Config is what a Client is built from.
type Config struct {
	// HTTPClient sends the requests. It comes from src/httpclient.
	HTTPClient *http.Client
	// Provider is the endpoint's configuration. Its ID tags the items the
	// client produces, so that only this provider reads their Opaque.
	Provider config.Provider
	// UserAgent is sent with every request, as "towa/<version>".
	UserAgent string
}

// Client sends Chat Completions requests to one endpoint.
type Client struct {
	http      *http.Client
	cfg       config.Provider
	userAgent string
	url       string
}

var _ provider.Provider = (*Client)(nil)

// ContextName returns the provenance used by this Chat endpoint.
func (c *Client) ContextName(cred auth.Credential) (string, error) {
	name, perr := c.contextName(cred)
	if perr != nil {
		return "", perr
	}
	return name, nil
}

func (c *Client) contextName(cred auth.Credential) (string, *provider.Error) {
	switch cred.Scheme {
	case auth.SchemeAPIKey, auth.SchemeOAuth, auth.SchemeNone:
		return c.cfg.ID, nil
	}
	return "", invalid("credential scheme %q is not %q, %q or %q", cred.Scheme, auth.SchemeAPIKey, auth.SchemeOAuth, auth.SchemeNone)
}

// New returns a client for the endpoint in cfg.
func New(cfg Config) (*Client, error) {
	switch {
	case cfg.HTTPClient == nil:
		return nil, errors.New("chat: no HTTP client")
	case cfg.UserAgent == "":
		return nil, errors.New("chat: no user agent")
	case cfg.Provider.Kind != config.ProviderChat:
		return nil, fmt.Errorf("chat: provider %q has kind %q, not %q", cfg.Provider.ID, cfg.Provider.Kind, config.ProviderChat)
	}
	if err := cfg.Provider.Validate("provider"); err != nil {
		return nil, fmt.Errorf("chat: %w", err)
	}
	return &Client{
		http:      cfg.HTTPClient,
		cfg:       cfg.Provider,
		userAgent: cfg.UserAgent,
		url:       strings.TrimRight(cfg.Provider.BaseURL, "/") + "/chat/completions",
	}, nil
}

// Stream sends one request and yields the response as events.
func (c *Client) Stream(ctx context.Context, req provider.Request) iter.Seq2[provider.Event, error] {
	return func(yield func(provider.Event, error) bool) {
		if err := c.stream(ctx, req, yield); err != nil {
			yield(provider.Event{}, err)
		}
	}
}

func (c *Client) stream(ctx context.Context, req provider.Request, yield func(provider.Event, error) bool) *provider.Error {
	if req.Credential.Account == "" {
		return invalid("the request credential names no account")
	}
	if _, perr := c.contextName(req.Credential); perr != nil {
		return perr
	}
	// The transport may read the body until the response is closed, so the
	// buffer goes back only after that.
	body := bytebufferpool.Get()
	defer bytebufferpool.Put(body)
	if perr := c.body(body, req); perr != nil {
		return perr
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body.B))
	if err != nil {
		return &provider.Error{Class: provider.Fail, Reason: provider.ReasonRequest, Err: err}
	}
	h := httpReq.Header
	// An endpoint that takes no credential gets no header, not an empty one.
	if req.Credential.Scheme != auth.SchemeNone {
		h.Set("Authorization", "Bearer "+req.Credential.Token)
	}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	h.Set("User-Agent", c.userAgent)
	if c.cfg.Referer != "" {
		h.Set("HTTP-Referer", c.cfg.Referer)
	}
	if c.cfg.Title != "" {
		// https://openrouter.ai/docs/app-attribution names this header and
		// keeps X-Title only for older clients (read 2026-11-11).
		h.Set("X-OpenRouter-Title", c.cfg.Title)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return transportError(ctx, err)
	}
	// The body is closed on every path, which ends the request when the
	// caller stops early. A close error after that has nothing to report to.
	defer func() { _ = resp.Body.Close() }()

	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusOK || mediaType != "text/event-stream" {
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return transportError(ctx, err)
		}
		if resp.StatusCode != http.StatusOK {
			return c.classifyResponse(resp.StatusCode, resp.Header, raw)
		}
		// A provider can answer 200 with a JSON body that holds only an
		// error, when it fails after it has sent the status.
		if e, ok := parseErrorBody(raw); ok {
			return c.classifyError(resp.StatusCode, resp.Header, e)
		}
		return &provider.Error{
			Class: provider.Fail, Reason: provider.ReasonRequest, Status: resp.StatusCode,
			Message: fmt.Sprintf("response has content type %q, not text/event-stream", resp.Header.Get("Content-Type")),
		}
	}

	s := &streamState{client: c, req: req, status: resp.StatusCode, header: resp.Header}
	dec := sse.NewDecoder(resp.Body)
	var ev sse.Event
	done := false
	for !done {
		err := dec.Decode(&ev)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return cutOff(ctx, err)
		}
		if ev.Data == nil {
			continue
		}
		if string(bytes.TrimSpace(ev.Data)) == "[DONE]" {
			done = true
			break
		}
		s.out = s.out[:0]
		if perr := s.chunk(ev.Data); perr != nil {
			return perr
		}
		for _, e := range s.out {
			if !yield(e, nil) {
				return nil
			}
		}
	}
	if !done && s.finish == "" {
		// The body ended without a finish reason and without [DONE]: the
		// connection closed before the model was done.
		return cutOff(ctx, io.ErrUnexpectedEOF)
	}
	s.out = s.out[:0]
	if perr := s.end(); perr != nil {
		return perr
	}
	for _, e := range s.out {
		if !yield(e, nil) {
			return nil
		}
	}
	return nil
}

// transportError is a request that got no response.
func transportError(ctx context.Context, err error) *provider.Error {
	if ctx.Err() != nil {
		// The caller cancelled or its deadline passed. Sending again is its
		// decision, not a retry.
		return &provider.Error{Class: provider.Fail, Reason: provider.ReasonNetwork, Err: ctx.Err()}
	}
	return &provider.Error{Class: provider.Retry, Reason: provider.ReasonNetwork, Err: err}
}

// cutOff is a response that stopped mid-stream. Nothing of it was yielded as
// an item, so sending the request again loses nothing.
func cutOff(ctx context.Context, err error) *provider.Error {
	e := transportError(ctx, err)
	e.Status = http.StatusOK
	e.Message = "the stream ended before the response was complete"
	return e
}
