package chat

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/httpclient"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/transcript"
)

func newClient(t testing.TB, p config.Provider) *Client {
	t.Helper()
	if p.ID == "" {
		p.ID = "test"
	}
	if p.BaseURL == "" {
		p.BaseURL = "https://example.invalid/v1"
	}
	p.Kind = config.ProviderChat
	c, err := New(Config{HTTPClient: httpclient.New(), Provider: p, UserAgent: "towa/test"})
	require.NoError(t, err)
	return c
}

func openRouter(routing config.OpenRouterRouting) config.Provider {
	return config.Provider{
		ID: "openrouter", CacheKeyField: config.CacheKeySessionID,
		OpenRouter: config.OpenRouter{Enabled: true, Routing: routing},
	}
}

// conversation is one turn with reasoning, text, two tool calls and their
// results, as the loop stores it.
func conversation(providerID string) []transcript.Item {
	return []transcript.Item{
		{User: &transcript.User{Parts: []transcript.Part{{Text: "list the files"}}}},
		{Thinking: &transcript.Thinking{Text: "I should look."}, Provider: providerID, Opaque: jsontext.Value(`{"field":"reasoning_content"}`), Origin: &transcript.Origin{Model: "deepseek-chat", Account: "main"}},
		{Text: &transcript.Text{Text: "Looking."}, Provider: providerID},
		{ToolCall: &transcript.ToolCall{ID: "call_1", Name: "shell", Input: jsontext.Value(`{"command":"ls"}`)}, Provider: providerID},
		// Arguments that were not valid JSON are stored in a JSON string.
		{ToolCall: &transcript.ToolCall{ID: "call_2", Name: "shell", Input: jsontext.Value(`"{\"command\":"`)}, Provider: providerID},
		{ToolResult: &transcript.ToolResult{CallID: "call_1", Name: "shell", Outcome: transcript.OutcomeCompleted, Parts: []transcript.Part{
			{Text: "a.go\n"},
			{Stored: &transcript.Stored{Hash: "sha256:00", Size: 9000}},
		}}},
		{ToolResult: &transcript.ToolResult{CallID: "call_2", Name: "shell", Outcome: transcript.OutcomeFailed, Parts: []transcript.Part{{Text: "invalid arguments"}}}},
		{User: &transcript.User{Parts: []transcript.Part{
			{Text: "and this picture"},
			{Image: &transcript.Blob{MimeType: "image/png", Data: []byte{1, 2, 3}}},
		}}},
	}
}

func TestRequestBodyForAPlainEndpoint(t *testing.T) {
	c := newClient(t, config.Provider{ID: "deepseek", CacheKeyField: config.CacheKeyPromptCacheKey})
	body, perr := c.body(provider.Request{
		Model:      "deepseek-chat",
		Credential: auth.Credential{Account: "main"},
		System:     []provider.SystemBlock{{Name: "base", Text: "You are an agent."}, {Name: "empty"}, {Name: "rules", Text: "Be brief."}},
		Items:      conversation("deepseek"),
		Tools: []provider.Tool{
			{Name: "shell", Description: "Run a command.", InputSchema: jsontext.Value(`{"type":"object","properties":{"command":{"type":"string"}}}`)},
			{NativeAPI: provider.NativeAPIChatCompletions, Native: jsontext.Value(`{"type":"web_search"}`)},
		},
		MaxOutputTokens: 4096,
		Effort:          "high",
		CacheKey:        "session-1",
	})
	require.Nil(t, perr)
	require.JSONEq(t, `{
		"model": "deepseek-chat",
		"stream": true,
		"stream_options": {"include_usage": true},
		"max_tokens": 4096,
		"reasoning_effort": "high",
		"prompt_cache_key": "session-1",
		"messages": [
			{"role": "system", "content": "You are an agent.\n\nBe brief."},
			{"role": "user", "content": "list the files"},
			{"role": "assistant", "content": "Looking.", "reasoning_content": "I should look.", "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "shell", "arguments": "{\"command\":\"ls\"}"}},
				{"id": "call_2", "type": "function", "function": {"name": "shell", "arguments": "{\"command\":"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "a.go\n"},
			{"role": "tool", "tool_call_id": "call_2", "content": "invalid arguments"},
			{"role": "user", "content": [
				{"type": "text", "text": "and this picture"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,AQID"}}
			]}
		],
		"tools": [
			{"type": "function", "function": {"name": "shell", "description": "Run a command.", "parameters": {"type":"object","properties":{"command":{"type":"string"}}}}},
			{"type": "web_search"}
		]
	}`, string(body))
}

func TestRequestBodyForOpenRouter(t *testing.T) {
	no := false
	p := openRouter(config.OpenRouterRouting{
		Order: []string{"anthropic", "google-vertex"}, AllowFallbacks: &no, DataCollection: "deny", Sort: "throughput",
		MaxPrice: map[string]float64{"prompt": 3, "completion": 15}, PreferredMaxLatencySeconds: 2.5,
	})
	p.OpenRouter.Reasoning.Exclude = true
	c := newClient(t, p)
	// Another provider wrote the reasoning, so it cannot go back.
	body, perr := c.body(provider.Request{
		Model: "anthropic/claude-sonnet-4.5", Items: conversation("deepseek")[:3], Effort: "low", CacheKey: "session-1",
	})
	require.Nil(t, perr)
	require.JSONEq(t, `{
		"model": "anthropic/claude-sonnet-4.5",
		"stream": true,
		"reasoning": {"effort": "low", "exclude": true},
		"session_id": "session-1",
		"provider": {
			"order": ["anthropic", "google-vertex"], "allow_fallbacks": false, "data_collection": "deny",
			"sort": "throughput", "max_price": {"prompt": 3, "completion": 15}, "preferred_max_latency": 2.5
		},
		"messages": [
			{"role": "user", "content": "list the files"},
			{"role": "assistant", "content": "Looking."}
		]
	}`, string(body))
}

func TestRequestRejectsWhatItCannotSend(t *testing.T) {
	c := newClient(t, config.Provider{})
	user := transcript.Item{User: &transcript.User{Parts: []transcript.Part{{Text: "hi"}}}}
	cases := map[string]provider.Request{
		"no model":        {Items: []transcript.Item{user}},
		"nothing to send": {Model: "m"},
		"usage item":      {Model: "m", Items: []transcript.Item{user, {Usage: &transcript.Usage{Model: "m"}}}},
		"empty item":      {Model: "m", Items: []transcript.Item{{}}},
		"empty part":      {Model: "m", Items: []transcript.Item{{User: &transcript.User{Parts: []transcript.Part{{}}}}}},
		"own native item": {Model: "m", Items: []transcript.Item{user, {Native: &transcript.Native{Kind: "x"}, Provider: "test"}}},
		"image in a result": {Model: "m", Items: []transcript.Item{user, {ToolResult: &transcript.ToolResult{CallID: "c", Parts: []transcript.Part{
			{Image: &transcript.Blob{MimeType: "image/png", Data: []byte{1}}},
		}}}}},
		"tool without a name": {Model: "m", Items: []transcript.Item{user}, Tools: []provider.Tool{{Description: "d"}}},
	}
	for name, req := range cases {
		_, perr := c.body(req)
		require.NotNil(t, perr, name)
		require.Equal(t, provider.Fail, perr.Class, name)
		require.Equal(t, provider.ReasonRequest, perr.Reason, name)
	}
}

// The delta is the streaming example in OpenRouter's reasoning tokens guide
// (docs/guides/best-practices/reasoning-tokens, "Streaming Response"), sent
// here in two pieces. The merged detail must go back in the next request.
func TestReasoningDetailsGoBackAsTheyCame(t *testing.T) {
	c := newClient(t, openRouter(config.OpenRouterRouting{}))
	s := &streamState{client: c, req: provider.Request{Model: "m", Credential: auth.Credential{Account: "main"}}, status: http.StatusOK}
	for _, data := range []string{
		`{"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.text","text":"Let me think about this ","signature":null,"id":"reasoning-text-1","format":"anthropic-claude-v1","index":0}]}}]}`,
		`{"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.text","text":"step by step...","signature":"sig","index":0}]}}]}`,
		`{"choices":[{"delta":{"content":"Done."},"finish_reason":"stop"}]}`,
	} {
		require.Nil(t, s.chunk([]byte(data)))
	}
	s.out = nil
	require.Nil(t, s.end())
	require.Len(t, s.out, 3)
	thinking := s.out[0].Item
	require.Equal(t, "Let me think about this step by step...", thinking.Thinking.Text)
	require.Equal(t, provider.StopEndTurn, s.out[2].Stop)

	user := transcript.Item{User: &transcript.User{Parts: []transcript.Part{{Text: "hi"}}}}
	body, perr := c.body(provider.Request{Model: "m", Credential: auth.Credential{Account: "main"}, Items: []transcript.Item{user, *thinking, *s.out[1].Item}})
	require.Nil(t, perr)
	require.JSONEq(t, `{"model":"m","stream":true,"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"Done.","reasoning_details":[
			{"type":"reasoning.text","text":"Let me think about this step by step...","signature":"sig","id":"reasoning-text-1","format":"anthropic-claude-v1","index":0}
		]}
	]}`, string(body))
}

// Each body is quoted from the provider's documentation, named in source.
func TestClassifiesDocumentedErrors(t *testing.T) {
	resetHeader := http.Header{"X-Ratelimit-Reset": {"1791763200000"}}
	cases := []struct {
		name, source string
		status       int
		header       http.Header
		body         string
		class        provider.Class
		reason       provider.Reason
		retryAfter   time.Duration
		resetAt      time.Time
	}{
		{
			name: "rate limit from an upstream provider", source: "openrouter.ai/docs/api/reference/errors-and-debugging, Provider errors and Retry-After header",
			status: 429, header: http.Header{"Retry-After": {"60"}},
			body:  `{"error":{"code":429,"message":"Rate limit exceeded","metadata":{"error_type":"rate_limit_exceeded","provider_code":"rate_limited"}}}`,
			class: provider.Retry, reason: provider.ReasonRateLimit, retryAfter: time.Minute,
		},
		{
			name: "free model limit with a reset header", source: "openrouter.ai/docs/api/reference/limits, Rate limits: X-RateLimit-Reset is milliseconds since the epoch",
			status: 429, header: resetHeader,
			body:  `{"error":{"code":429,"message":"Rate limit exceeded","metadata":{"error_type":"rate_limit_exceeded"}}}`,
			class: provider.Retry, reason: provider.ReasonRateLimit, resetAt: time.UnixMilli(1791763200000),
		},
		{
			name: "in-flight budget", source: "openrouter.ai/docs/api/reference/limits, In-flight spending budget",
			status: 402, header: http.Header{"Retry-After": {"5"}},
			body:  `{"error":{"code":402,"message":"This request would exceed your available credits given your current in-flight requests. Retry after in-flight requests settle, or add credits.","metadata":{"reason":"in_flight_budget_exhausted","limit_source":"openrouter_in_flight_budget","remedy_hint":"Retry after your in-flight requests settle (see the Retry-After header). Adding credits at https://openrouter.ai/settings/credits raises your in-flight budget, up to a capped ceiling."}}}`,
			class: provider.Retry, reason: provider.ReasonRateLimit, retryAfter: 5 * time.Second,
		},
		{
			name: "spent budget", source: "openrouter.ai/docs/api/reference/errors-and-debugging, Budget errors",
			status: 403,
			body:   `{"error":{"code":403,"message":"Budget limit exceeded (weekly limit). Contact your org admin. $52.00 of $50.00 used; resets 2026-10-12T00:00:00.000Z.","metadata":{"limit_source":"openrouter_guardrail_budget","remedy_hint":"Wait for the budget window to reset (see metadata.resets_at), or ask your org admin to raise the guardrail budget.","scope":"workspace_default_member","limit_usd":50,"spend_usd":52,"resets_at":"2026-10-12T00:00:00.000Z"}}}`,
			class:  provider.Fallback, reason: provider.ReasonRateLimit, resetAt: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "guardrail block", source: "openrouter.ai/docs/api/reference/errors-and-debugging, Guardrail errors",
			status: 403,
			body:   `{"error":{"code":403,"message":"Request blocked: prompt injection patterns detected","metadata":{"patterns":["ignore all previous instructions"]}}}`,
			class:  provider.Fallback, reason: provider.ReasonAuth,
		},
		{
			name: "context length", source: "OpenAI API error for a prompt over the context window, as quoted in jcode crates/jcode-tui/src/tui/app/tests/state_model_poke_01/part_01.rs",
			status: 400,
			body:   `{"error":{"message":"This model's maximum context length is 200000 tokens","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`,
			class:  provider.Fail, reason: provider.ReasonContextFull,
		},
		{
			name: "gateway page", source: "a 502 whose body is not JSON",
			status: 502, body: "<html><body>Bad Gateway</body></html>",
			class: provider.Retry, reason: provider.ReasonOverload,
		},
	}
	c := newClient(t, openRouter(config.OpenRouterRouting{}))
	for _, tc := range cases {
		got := c.classifyResponse(tc.status, tc.header, []byte(tc.body))
		require.Equal(t, tc.class, got.Class, tc.name)
		require.Equal(t, tc.reason, got.Reason, tc.name)
		require.Equal(t, tc.status, got.Status, tc.name)
		require.Equal(t, tc.retryAfter, got.RetryAfter, tc.name)
		require.True(t, tc.resetAt.Equal(got.ResetAt), "%s: reset at %v", tc.name, got.ResetAt)
		require.NotEmpty(t, got.Message, tc.name)
	}

	// Another provider's X-RateLimit-Reset has another unit and is not read.
	plain := newClient(t, config.Provider{})
	require.True(t, plain.classifyResponse(429, resetHeader, []byte(cases[1].body)).ResetAt.IsZero())
}

// The event is the example SSE data in OpenRouter's errors guide
// (docs/api/reference/errors-and-debugging, Mid-stream errors). The HTTP
// status is 200, so the class comes from the code in the event.
func TestClassifiesADocumentedMidStreamError(t *testing.T) {
	s := &streamState{client: newClient(t, openRouter(config.OpenRouterRouting{})), status: http.StatusOK}
	perr := s.chunk([]byte(`{"id":"gen-abc123","object":"chat.completion.chunk","created":1234567890,"model":"openai/gpt-4o","provider":"OpenAI","error":{"code":429,"message":"Rate limit exceeded","metadata":{"error_type":"rate_limit_exceeded"}},"choices":[{"index":0,"delta":{"content":""},"finish_reason":"error"}]}`))
	require.NotNil(t, perr)
	require.Equal(t, provider.Retry, perr.Class)
	require.Equal(t, provider.ReasonRateLimit, perr.Reason)
	require.Equal(t, http.StatusOK, perr.Status)
	require.Equal(t, "Rate limit exceeded", perr.Message)
}

func streamError(ctx context.Context, t *testing.T, c *Client) *provider.Error {
	t.Helper()
	req := provider.Request{
		Model: "m", Credential: auth.Credential{Account: "main", Scheme: auth.SchemeAPIKey, Token: "sk-test"},
		Items: []transcript.Item{{User: &transcript.User{Parts: []transcript.Part{{Text: "hi"}}}}},
	}
	for ev, err := range c.Stream(ctx, req) {
		if err != nil {
			var perr *provider.Error
			require.True(t, errors.As(err, &perr), "every error is a *provider.Error")
			return perr
		}
		require.Fail(t, "unexpected event", "%+v", ev)
	}
	require.Fail(t, "the stream ended without an error")
	return nil
}

// The endpoint answers a bare 503. Only the request the client sent is
// checked.
func TestSendsItsHeaders(t *testing.T) {
	var got http.Header
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, path = r.Header.Clone(), r.URL.Path
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	p := openRouter(config.OpenRouterRouting{})
	p.BaseURL, p.Referer, p.Title = server.URL+"/api/v1/", "https://github.com/gfx-labs/towa", "towa"
	perr := streamError(t.Context(), t, newClient(t, p))
	require.Equal(t, http.StatusServiceUnavailable, perr.Status)

	require.Equal(t, "/api/v1/chat/completions", path)
	require.Equal(t, "Bearer sk-test", got.Get("Authorization"))
	require.Equal(t, "https://github.com/gfx-labs/towa", got.Get("HTTP-Referer"))
	require.Equal(t, "towa", got.Get("X-OpenRouter-Title"))
	require.Equal(t, "towa/test", got.Get("User-Agent"))
}

// hangingServer answers 200 as an event stream, sends no event, and drops
// the connection when release is closed.
func hangingServer(t *testing.T) (p config.Provider, started <-chan struct{}, release chan struct{}) {
	t.Helper()
	begun := make(chan struct{})
	release = make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(w).Flush()
		close(begun)
		<-release
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(server.Close)
	return config.Provider{BaseURL: server.URL}, begun, release
}

func TestAStreamCutOffIsRetried(t *testing.T) {
	p, _, release := hangingServer(t)
	close(release)
	perr := streamError(t.Context(), t, newClient(t, p))
	require.Equal(t, provider.Retry, perr.Class)
	require.Equal(t, provider.ReasonNetwork, perr.Reason)
	require.Equal(t, http.StatusOK, perr.Status)
}

func TestACancelledStreamIsNotRetried(t *testing.T) {
	p, started, release := hangingServer(t)
	defer close(release)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-started
		cancel()
	}()
	perr := streamError(ctx, t, newClient(t, p))
	require.Equal(t, provider.Fail, perr.Class, "the caller cancelled, so nothing is sent again")
	require.ErrorIs(t, perr, context.Canceled)
}
