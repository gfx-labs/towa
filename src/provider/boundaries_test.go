package provider_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/auth"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/httpclient"
	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/provider/anthropic"
	"github.com/gfx-labs/acpharness/src/provider/chat"
	"github.com/gfx-labs/acpharness/src/provider/openai"
	"github.com/gfx-labs/acpharness/src/transcript"
)

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
			var client provider.Provider
			var err error
			switch api {
			case "responses":
				client, err = openai.New(openai.Config{HTTPClient: httpclient.New(), BaseURL: server.URL, CodexBaseURL: server.URL, Provider: "configured-responses"})
			case "messages":
				client, err = anthropic.New(anthropic.Config{HTTPClient: httpclient.New(), Version: "test", BaseURL: server.URL, Provider: "configured-messages"})
			case "chat":
				client, err = chat.New(chat.Config{HTTPClient: httpclient.New(), UserAgent: "test", Provider: config.Provider{ID: "configured-chat", Kind: config.ProviderChat, BaseURL: server.URL}})
			}
			require.NoError(t, err)
			namer, ok := client.(provider.ContextNamer)
			require.True(t, ok)
			cred := auth.Credential{Account: "test", Scheme: auth.SchemeAPIKey, Token: "not-a-secret"}
			req := provider.Request{Model: "test", Credential: cred, MaxOutputTokens: 64, Items: []transcript.Item{{User: &transcript.User{Role: "developer", Parts: []transcript.Part{{Text: "preserve this priority"}}}}}}
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
			req.Items[0].User.Role = ""
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
			native, err := os.ReadFile(filepath.Join("..", "tools", "codex", "specs", "apply_patch.json"))
			require.NoError(t, err)
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
					{Native: &transcript.Native{Kind: "untrusted"}, Provider: source.name, Origin: source.origin, Opaque: opaque},
					{ToolCall: &transcript.ToolCall{ID: "opaque-call", Name: "tool", Input: jsontext.Value(`{}`)}, Provider: source.name, Origin: source.origin, Opaque: opaque},
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
				req.Items = append(req.Items[:1],
					transcript.Item{Thinking: &transcript.Thinking{Text: "incompatible-thinking"}, Provider: source.name, Origin: source.origin, Opaque: opaque},
					transcript.Item{Text: &transcript.Text{Text: "portable-answer"}, Provider: source.name, Origin: source.origin, Opaque: opaque},
				)
				got = streamError(req)
				require.Equal(t, http.StatusServiceUnavailable, got.Status)
				body := string(<-bodies)
				require.Contains(t, body, "portable-answer")
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
				call := transcript.Item{Provider: "foreign", ToolCall: &transcript.ToolCall{ID: "call", Name: "apply_patch", Input: jsontext.Value(`"patch text"`)}, Opaque: jsontext.Value(`{"type":"custom_tool_call"}`)}
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
				req.Items[1].ToolCall.Input = jsontext.Value(`{"not":"text"}`)
				got = streamError(req)
				require.Equal(t, provider.Fail, got.Class)
				require.Equal(t, provider.ReasonRequest, got.Reason)
				require.Equal(t, before, requests.Load())
			}
		})
	}
}
