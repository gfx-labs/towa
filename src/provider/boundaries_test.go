package provider_test

import (
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/provider/anthropic"
	"github.com/gfx-labs/towa/src/provider/chat"
	"github.com/gfx-labs/towa/src/provider/openai"
	"github.com/gfx-labs/towa/src/support/httpclient"
	"github.com/gfx-labs/towa/src/transcript"
)

// boundaryClient is the client of one wire API, sending to baseURL.
func boundaryClient(t *testing.T, api, baseURL string) provider.Provider {
	t.Helper()
	var client provider.Provider
	var err error
	switch api {
	case "responses":
		client, err = openai.New(openai.Config{HTTPClient: httpclient.New(), BaseURL: baseURL, CodexBaseURL: baseURL, Provider: "configured-responses"})
	case "messages":
		client, err = anthropic.New(anthropic.Config{HTTPClient: httpclient.New(), Version: "test", BaseURL: baseURL, Provider: "configured-messages"})
	case "chat":
		client, err = chat.New(chat.Config{HTTPClient: httpclient.New(), UserAgent: "test", Provider: config.Provider{ID: "configured-chat", Kind: config.ProviderChat, BaseURL: baseURL}})
	}
	require.NoError(t, err)
	require.NotNil(t, client, api)
	return client
}

// A tool's result is stored before a request is built from it. A result
// holding content the API has no form for, which an MCP tool can return,
// must still build: a refusal here repeats in every later turn of the
// session. The endpoint sends only a 503 status.
func TestAToolResultTheAPICannotCarryIsStillSent(t *testing.T) {
	archive := []byte("PK\x05\x06 not text, an image or a PDF")
	for _, api := range []string{"responses", "messages", "chat"} {
		t.Run(api, func(t *testing.T) {
			bodies := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body jsontext.Value
				if err := json.UnmarshalRead(r.Body, &body); err != nil {
					t.Errorf("read request: %v", err)
				}
				bodies <- string(body)
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer server.Close()
			req := provider.Request{
				Model: "test", MaxOutputTokens: 64,
				Credential: auth.Credential{Account: "test", Scheme: auth.SchemeAPIKey, Token: "not-a-secret"},
				Items: []transcript.Item{
					{Body: transcript.User{Parts: []transcript.Part{{Text: "build it"}}}},
					{Body: transcript.ToolCall{ID: "call-1", Name: "build", Input: jsontext.Value(`{}`)}},
					{Body: transcript.ToolResult{CallID: "call-1", Name: "build", Outcome: transcript.OutcomeCompleted, Parts: []transcript.Part{
						{Text: "the build output"},
						{Audio: &transcript.Blob{MimeType: "audio/x-towa-test", Data: []byte{1, 2, 3}}},
						{Resource: &transcript.Resource{URI: "file:///out.zip", MimeType: "application/zip", Data: archive}},
						{Resource: &transcript.Resource{URI: "file:///empty.txt", MimeType: "text/plain"}},
					}}},
				},
			}
			var got *provider.Error
			for _, err := range boundaryClient(t, api, server.URL).Stream(t.Context(), req) {
				if err != nil {
					require.ErrorAs(t, err, &got)
				}
			}
			require.NotNil(t, got)
			require.Equal(t, http.StatusServiceUnavailable, got.Status, "the request was built and sent: %v", got)
			body := <-bodies
			// The model is told what the tool returned and did not get.
			require.Contains(t, body, "the build output")
			require.Contains(t, body, "file:///out.zip")
			require.NotContains(t, body, base64.StdEncoding.EncodeToString(archive))
			// Chat Completions carries audio, in a user message.
			if api == "chat" {
				require.Contains(t, body, "input_audio")
			} else {
				require.Contains(t, body, "audio/x-towa-test")
			}
		})
	}
}

// These checks stop at the request boundary. The endpoint sends only a 503
// status, not an invented provider response.
func TestRequestProvenanceBoundaries(t *testing.T) {
	for _, api := range []string{"responses", "messages", "chat"} {
		t.Run(api, func(t *testing.T) {
			var requests atomic.Int64
			bodies := make(chan jsontext.Value, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				var body jsontext.Value
				if err := json.UnmarshalRead(r.Body, &body); err != nil {
					t.Errorf("read request: %v", err)
				}
				bodies <- body
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer server.Close()
			client := boundaryClient(t, api, server.URL)
			var err error
			namer, ok := client.(provider.ContextNamer)
			require.True(t, ok)
			cred := auth.Credential{Account: "test", Scheme: auth.SchemeAPIKey, Token: "not-a-secret"}
			req := provider.Request{Model: "test", Credential: cred, MaxOutputTokens: 64, Items: []transcript.Item{{Body: transcript.User{Role: "developer", Parts: []transcript.Part{{Text: "preserve this priority"}}}}}}
			streamError := func(req provider.Request) *provider.Error {
				t.Helper()
				var got *provider.Error
				for _, err := range client.Stream(t.Context(), req) {
					if err != nil {
						require.ErrorAs(t, err, &got)
					}
				}
				require.NotNil(t, got)
				return got
			}
			got := streamError(req)
			if api == "messages" {
				require.Equal(t, provider.Fail, got.Class)
				require.Equal(t, provider.ReasonRequest, got.Reason)
				require.Zero(t, requests.Load())
			} else {
				require.Equal(t, int64(1), requests.Load())
				var body struct {
					Input []struct {
						Role string `json:"role"`
					} `json:"input"`
					Messages []struct {
						Role string `json:"role"`
					} `json:"messages"`
				}
				require.NoError(t, json.Unmarshal(<-bodies, &body))
				if api == "responses" {
					require.Len(t, body.Input, 1)
					require.Equal(t, "developer", body.Input[0].Role)
				} else {
					require.Len(t, body.Messages, 1)
					require.Equal(t, "developer", body.Messages[0].Role)
				}
			}
			before := requests.Load()
			bad := cred
			bad.Scheme = "unknown"
			_, err = namer.ContextName(bad)
			var perr *provider.Error
			require.ErrorAs(t, err, &perr)
			require.Equal(t, provider.Fail, perr.Class)
			require.Equal(t, provider.ReasonRequest, perr.Reason)
			req.Credential = bad
			system := req.Items[0].Body.(transcript.User)
			system.Role = ""
			req.Items[0].Body = system
			got = streamError(req)
			require.Equal(t, provider.Fail, got.Class)
			require.Equal(t, provider.ReasonRequest, got.Reason)
			require.Equal(t, before, requests.Load())
			req.Credential = cred
			req.Credential.Account = ""
			got = streamError(req)
			require.Equal(t, provider.Fail, got.Class)
			require.Equal(t, provider.ReasonRequest, got.Reason)
			require.Equal(t, before, requests.Load())
			req.Credential = cred
			native := jsontext.Value(`{"type":"custom","name":"apply_patch","description":"Apply a patch. The input is the patch text."}`)
			ownAPI := map[string]provider.NativeAPI{"responses": provider.NativeAPIResponses, "messages": provider.NativeAPIMessages, "chat": provider.NativeAPIChatCompletions}[api]
			for _, tag := range []provider.NativeAPI{"", provider.NativeAPIResponses, provider.NativeAPIMessages, provider.NativeAPIChatCompletions} {
				if tag == ownAPI {
					continue
				}
				req.Tools = []provider.Tool{{Name: "apply_patch", Native: native, NativeAPI: tag}}
				got = streamError(req)
				require.Equal(t, provider.Fail, got.Class)
				require.Equal(t, provider.ReasonRequest, got.Reason)
				require.Equal(t, before, requests.Load())
			}
			req.Tools = []provider.Tool{{NativeAPI: ownAPI}}
			got = streamError(req)
			require.Equal(t, provider.Fail, got.Class)
			require.Equal(t, provider.ReasonRequest, got.Reason)
			require.Equal(t, before, requests.Load())
			req.Tools = nil
			own, err := namer.ContextName(cred)
			require.NoError(t, err)
			for _, source := range []struct {
				name   string
				origin *transcript.Origin
			}{
				{own, nil},
				{own, &transcript.Origin{Model: "another-model", Account: cred.Account}},
				{own, &transcript.Origin{Model: req.Model, Account: "another-account"}},
				{"another-provider", &transcript.Origin{Model: req.Model, Account: cred.Account}},
			} {
				// This string is deliberately not wire metadata. Incompatible
				// provenance must be decided before opaque data is decoded.
				opaque := jsontext.Value(`"untrusted-opaque-marker"`)
				for _, item := range []transcript.Item{
					{Body: transcript.Native{Kind: "untrusted"}, Provider: source.name, Origin: source.origin, Opaque: opaque},
					{Body: transcript.ToolCall{ID: "opaque-call", Name: "tool", Input: jsontext.Value(`"text input"`)}, Provider: source.name, Origin: source.origin, Opaque: opaque},
				} {
					req.Items = append(req.Items[:1], item)
					got = streamError(req)
					require.Equal(t, provider.Fail, got.Class)
					require.Equal(t, provider.ReasonRequest, got.Reason)
					require.Equal(t, before, requests.Load())
					if responses, ok := client.(*openai.Client); ok {
						_, err := responses.Compact(t.Context(), req)
						var perr *provider.Error
						require.ErrorAs(t, err, &perr)
						require.Equal(t, provider.Fail, perr.Class)
						require.Equal(t, provider.ReasonRequest, perr.Reason)
						require.Equal(t, before, requests.Load())
					}
				}
				// A structural call goes as a plain call without the extensions.
				req.Items = append(req.Items[:1],
					transcript.Item{Body: transcript.Thinking{Text: "incompatible-thinking"}, Provider: source.name, Origin: source.origin, Opaque: opaque},
					transcript.Item{Body: transcript.Text{Text: "portable-answer"}, Provider: source.name, Origin: source.origin, Opaque: opaque},
					transcript.Item{Body: transcript.ToolCall{ID: "portable-call", Name: "tool", Input: jsontext.Value(`{}`)}, Provider: source.name, Origin: source.origin, Opaque: opaque},
					transcript.Item{Body: transcript.ToolResult{CallID: "portable-call", Outcome: transcript.OutcomeCompleted, Parts: []transcript.Part{{Text: "done"}}}},
				)
				got = streamError(req)
				require.Equal(t, http.StatusServiceUnavailable, got.Status)
				body := string(<-bodies)
				require.Contains(t, body, "portable-answer")
				require.Contains(t, body, "portable-call")
				require.NotContains(t, body, "untrusted-opaque-marker")
				require.NotContains(t, body, "incompatible-thinking")
				before = requests.Load()
			}
			req.Items = req.Items[:1]
			if api == "responses" {
				req.Tools = []provider.Tool{{Name: "apply_patch", Native: native, NativeAPI: provider.NativeAPIResponses}}
				got = streamError(req)
				require.Equal(t, http.StatusServiceUnavailable, got.Status)
				var body struct {
					Tools []jsontext.Value `json:"tools"`
				}
				require.NoError(t, json.Unmarshal(<-bodies, &body))
				require.Len(t, body.Tools, 1)
				require.JSONEq(t, string(native), string(body.Tools[0]))
				before = requests.Load()
				req.Tools = nil
				req.Credential = cred
				call := transcript.Item{Provider: "foreign", Body: transcript.ToolCall{ID: "call", Name: "apply_patch", Input: jsontext.Value(`"patch text"`)}, Opaque: jsontext.Value(`{"type":"custom_tool_call"}`)}
				req.Items = append(req.Items, call)
				got = streamError(req)
				require.Equal(t, provider.Fail, got.Class)
				require.Equal(t, provider.ReasonRequest, got.Reason)
				require.Equal(t, before, requests.Load())
				own, err := namer.ContextName(cred)
				require.NoError(t, err)
				req.Items[1].Provider = own
				req.Items[1].Origin = &transcript.Origin{Model: req.Model, Account: cred.Account}
				for _, typ := range []string{"function_call", "custom_tool_call"} {
					req.Items[1].Opaque, err = json.Marshal(map[string]string{"type": typ})
					require.NoError(t, err)
					got = streamError(req)
					require.Equal(t, http.StatusServiceUnavailable, got.Status)
					var body struct {
						Input []struct {
							Type      string `json:"type"`
							Input     string `json:"input"`
							Arguments string `json:"arguments"`
						} `json:"input"`
					}
					require.NoError(t, json.Unmarshal(<-bodies, &body))
					require.Len(t, body.Input, 2)
					require.Equal(t, typ, body.Input[1].Type)
					if typ == "function_call" {
						require.Equal(t, "patch text", body.Input[1].Arguments)
					} else {
						require.Equal(t, "patch text", body.Input[1].Input)
					}
				}
				before = requests.Load()
				object := req.Items[1].Body.(transcript.ToolCall)
				object.Input = jsontext.Value(`{"not":"text"}`)
				req.Items[1].Body = object
				got = streamError(req)
				require.Equal(t, provider.Fail, got.Class)
				require.Equal(t, provider.ReasonRequest, got.Reason)
				require.Equal(t, before, requests.Load())
			}
		})
	}
}
