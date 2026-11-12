// Package anthropic is the client for the Anthropic Messages API. One
// client serves API keys and Claude subscription OAuth tokens. The
// credential's scheme picks the request shape.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"iter"
	"mime"
	"net/http"
	"slices"
	"strings"
	"uuid"

	"github.com/gfx-labs/sse"
	"github.com/valyala/bytebufferpool"

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/transcript"
)

// Values of the wire protocol.
const (
	// DefaultBaseURL is where requests go when Config.BaseURL is empty.
	DefaultBaseURL = "https://api.anthropic.com"
	// APIVersion is the anthropic-version header.
	APIVersion = "2023-06-01"
	// Provider is the Item.Provider value of items from this client when
	// Config.Provider is empty.
	Provider = "anthropic"
	// DefaultErrorBodyBytes is how much of an error response is read when
	// Config.ErrorBodyBytes is zero. An error body is a JSON object of a few
	// hundred bytes. A gateway's HTML error page is the large case.
	DefaultErrorBodyBytes = 1 << 20

	// CacheTTL5m and CacheTTL1h are the lifetimes a cache breakpoint can ask
	// for.
	CacheTTL5m = "5m"
	CacheTTL1h = "1h"
)

// The Claude subscription request shape, from jcode 037163c7b
// (crates/jcode-provider-anthropic, jcode-provider-core/src/anthropic.rs)
// and its probe of 2026-11-05. Without the billing block Opus and Sonnet
// answer an OAuth request with HTTP 429.
const (
	// BillingEntrypoint is the cc_entrypoint value of the billing block.
	// jcode's branches differ: "jcode" on 037163c7b, which is used here,
	// and "sdk-cli" on origin/master 8e11eb23e, which is what the probe
	// ran with. Neither was sent from towa to a live account yet.
	BillingEntrypoint = "jcode"
	// billingVersion and billingHash are the cc_version and cch values
	// jcode sends.
	billingVersion = "2.1.280"
	billingHash    = "33f85"
	// billingBlock is sent as the first system block, as text. Anthropic
	// removes it before the model sees it.
	billingBlock = "x-anthropic-billing-header: cc_version=" + billingVersion +
		"; cc_entrypoint=" + BillingEntrypoint + "; cch=" + billingHash + ";"
	// oauthBetas is jcode's ANTHROPIC_OAUTH_BETA_HEADERS. Only
	// oauth-2025-04-20 is known to be required.
	oauthBetas = "oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27," +
		"prompt-caching-scope-2026-01-05,advisor-tool-2026-03-01,advanced-tool-use-2025-11-20,effort-2025-11-24"

	interleavedThinkingBeta = "interleaved-thinking-2025-05-14"
	thinkingBindingBeta     = "thinking-binding-controls-2026-08-01"
)

// ThinkingMode is how a model is asked to think.
type ThinkingMode string

// Thinking modes.
const (
	// ThinkingOff sends no thinking parameter.
	ThinkingOff ThinkingMode = ""
	// ThinkingAdaptive lets the model decide how much to think. The effort
	// of the request steers it.
	ThinkingAdaptive ThinkingMode = "adaptive"
	// ThinkingManual gives the model a token budget, for models without
	// adaptive thinking.
	ThinkingManual ThinkingMode = "manual"
)

// Thinking is the thinking parameter for one model. Which mode a model takes
// is model metadata, which this package does not hold.
type Thinking struct {
	Mode ThinkingMode
	// BudgetTokens is the budget of ThinkingManual. It has to be below the
	// request's MaxOutputTokens.
	BudgetTokens int64
	// Display is the display value of ThinkingAdaptive, such as
	// "summarized". It is not sent when empty.
	Display string
	// DropMismatchedBlocks lets the API discard signed thinking that no
	// longer matches the prompt before it, in place of rejecting the
	// request. Models whose thinking cannot be turned off need it after a
	// compaction. It applies to ThinkingAdaptive.
	DropMismatchedBlocks bool
}

// Config configures a Client.
type Config struct {
	// HTTPClient sends every request. It comes from src/httpclient.
	HTTPClient *http.Client
	// Version is the towa version. The user agent is
	// towa/<Version>.
	Version string
	// BaseURL is the root of the API, without /v1/messages. Empty means
	// DefaultBaseURL.
	BaseURL string
	// Provider is the Item.Provider value of items from this client. Empty
	// means Provider. Another endpoint that speaks the Messages API needs
	// its own value, so that thinking signatures are not sent across.
	Provider string
	// Thinking returns the thinking parameter for a model. Nil means no
	// request asks for thinking.
	Thinking func(model string) Thinking
	// Betas are added to the anthropic-beta header of every request, for
	// the betas that tools passed as Tool.Native need.
	Betas []string
	// CacheTTL is the lifetime of the cache breakpoints: CacheTTL5m or
	// CacheTTL1h. Empty sends none, which the API reads as five minutes.
	CacheTTL string
	// ErrorBodyBytes is how much of an error response is read. Zero means
	// DefaultErrorBodyBytes. A longer body is reported as such in the
	// error's message.
	ErrorBodyBytes int64
}

// Client sends requests to the Messages API. It is safe for concurrent use.
type Client struct {
	cfg       Config
	userAgent string
}

var _ provider.Provider = (*Client)(nil)

// ContextName returns the provenance used by this Messages endpoint.
func (c *Client) ContextName(cred auth.Credential) (string, error) {
	switch cred.Scheme {
	case auth.SchemeAPIKey, auth.SchemeOAuth:
		return c.cfg.Provider, nil
	default:
		return "", invalidRequest(fmt.Sprintf("credential scheme %q is not %q or %q", cred.Scheme, auth.SchemeOAuth, auth.SchemeAPIKey))
	}
}

// New returns a client. It fails when the configuration cannot work.
func New(cfg Config) (*Client, error) {
	if cfg.HTTPClient == nil {
		return nil, errors.New("anthropic: Config.HTTPClient is nil")
	}
	if cfg.Version == "" {
		return nil, errors.New("anthropic: Config.Version is empty")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Provider == "" {
		cfg.Provider = Provider
	}
	switch cfg.CacheTTL {
	case "", CacheTTL5m, CacheTTL1h:
	default:
		return nil, fmt.Errorf("anthropic: Config.CacheTTL %q is not %q or %q", cfg.CacheTTL, CacheTTL5m, CacheTTL1h)
	}
	if cfg.ErrorBodyBytes == 0 {
		cfg.ErrorBodyBytes = DefaultErrorBodyBytes
	}
	if cfg.ErrorBodyBytes < 0 {
		return nil, fmt.Errorf("anthropic: Config.ErrorBodyBytes is %d", cfg.ErrorBodyBytes)
	}
	return &Client{cfg: cfg, userAgent: "towa/" + cfg.Version}, nil
}

// Stream sends one request and yields what the model produces.
func (c *Client) Stream(ctx context.Context, req provider.Request) iter.Seq2[provider.Event, error] {
	return func(yield func(provider.Event, error) bool) {
		if perr := c.stream(ctx, req, yield); perr != nil {
			yield(provider.Event{}, perr)
		}
	}
}

// stream returns the error to yield. It returns nil once the stream has
// stopped or the consumer has broken out of the range.
func (c *Client) stream(ctx context.Context, req provider.Request, yield func(provider.Event, error) bool) *provider.Error {
	if req.Credential.Account == "" {
		return invalidRequest("the request credential names no account")
	}
	body, thinking, perr := c.buildRequest(req)
	if perr != nil {
		return perr
	}
	header, perr := c.header(req.Credential, thinking)
	if perr != nil {
		return perr
	}
	// The transport may read the body until the response is closed, so the
	// buffer goes back only after that.
	buf := bytebufferpool.Get()
	defer bytebufferpool.Put(buf)
	if err := json.MarshalWrite(buf, body); err != nil {
		return invalidRequest("encode request: " + err.Error())
	}
	// NewRequest sets the length and GetBody for a bytes.Reader, which a
	// redirect or a retried connection needs.
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(req.Credential), bytes.NewReader(buf.B))
	if err != nil {
		return invalidRequest("build request: " + err.Error())
	}
	httpReq.Header = header

	resp, err := c.cfg.HTTPClient.Do(httpReq)
	if err != nil {
		return transportError(ctx, "send request", err)
	}
	defer resp.Body.Close() //nolint:errcheck // nothing to do about a close error on a body being abandoned
	if resp.StatusCode != http.StatusOK {
		return c.statusError(ctx, resp)
	}
	// A gateway can answer 200 with a page of its own.
	if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType != "text/event-stream" {
		return &provider.Error{
			Class: provider.Fail, Reason: provider.ReasonNetwork, Status: resp.StatusCode,
			Message: fmt.Sprintf("the response is %q, not an event stream", resp.Header.Get("Content-Type")),
		}
	}

	state := newStreamState(c.cfg.Provider, transcript.Origin{Model: req.Model, Account: req.Credential.Account})
	for ev, err := range sse.Events(resp.Body) {
		if err != nil {
			return transportError(ctx, "read stream", err)
		}
		done, perr := state.handle(ev.Data, yield)
		if perr != nil {
			return perr
		}
		if done {
			return nil
		}
	}
	if err := ctx.Err(); err != nil {
		return transportError(ctx, "read stream", err)
	}
	return &provider.Error{
		Class: provider.Retry, Reason: provider.ReasonNetwork,
		Message: "the stream ended before the message did", Err: io.ErrUnexpectedEOF,
	}
}

// url is the endpoint for a credential. jcode sends OAuth requests with
// beta=true, and its probe kept it.
func (c *Client) url(cred auth.Credential) string {
	if cred.Scheme == auth.SchemeOAuth {
		return c.cfg.BaseURL + "/v1/messages?beta=true"
	}
	return c.cfg.BaseURL + "/v1/messages"
}

// header builds the request headers. It never reads Credential.Proxied: a
// proxied account gets the request a held token gets.
func (c *Client) header(cred auth.Credential, thinking Thinking) (http.Header, *provider.Error) {
	if cred.Token == "" {
		return nil, &provider.Error{Class: provider.Fallback, Reason: provider.ReasonAuth, Message: "the credential has no token"}
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Anthropic-Version", APIVersion)
	h.Set("User-Agent", c.userAgent)

	var betas []string
	switch cred.Scheme {
	case auth.SchemeOAuth:
		h.Set("Authorization", "Bearer "+cred.Token)
		// jcode sends this accept value and a request id on the OAuth path.
		h.Set("Accept", "application/json")
		h.Set("X-Client-Request-Id", uuid.NewV4().String())
		betas = strings.Split(oauthBetas, ",")
	case auth.SchemeAPIKey:
		h.Set("X-Api-Key", cred.Token)
		h.Set("Accept", "text/event-stream")
	default:
		return nil, invalidRequest(fmt.Sprintf("credential scheme %q is not %q or %q", cred.Scheme, auth.SchemeOAuth, auth.SchemeAPIKey))
	}
	add := func(beta string) {
		if !slices.Contains(betas, beta) {
			betas = append(betas, beta)
		}
	}
	if thinking.Mode != ThinkingOff {
		add(interleavedThinkingBeta)
	}
	if thinking.Mode == ThinkingAdaptive && thinking.DropMismatchedBlocks {
		add(thinkingBindingBeta)
	}
	for _, beta := range c.cfg.Betas {
		add(beta)
	}
	if len(betas) > 0 {
		h.Set("Anthropic-Beta", strings.Join(betas, ","))
	}
	return h, nil
}

// statusError reads the body of a response that is not 200 and classifies
// it.
func (c *Client) statusError(ctx context.Context, resp *http.Response) *provider.Error {
	limit := c.cfg.ErrorBodyBytes
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		perr := transportError(ctx, "read error response", err)
		perr.Status = resp.StatusCode
		return perr
	}
	over := int64(len(data)) > limit
	if over {
		data = data[:limit]
	}
	perr := classify(resp.StatusCode, resp.Header, parseErrorBody(data))
	if over {
		perr.Message += fmt.Sprintf(" (the error response is longer than %d bytes and was not read to its end)", limit)
	}
	return perr
}
