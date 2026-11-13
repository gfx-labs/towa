package chat

import (
	"encoding/json/jsontext"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/provider/providertest"
	"github.com/gfx-labs/towa/src/support/httpclient"
	"github.com/gfx-labs/towa/src/transcript"
)

// source is a server recordings are made from.
type source struct {
	name string
	// env names the variable that must be set for TestRecord to send
	// requests to the source. For OpenRouter it holds the key.
	env string
	// base is the source's base URL and path is the part of it a replay
	// server has to repeat.
	base, path string
	// token is sent when env holds no key.
	token     string
	provider  func(base string) config.Provider
	exchanges []exchange
}

// exchange is one request whose real response is recorded and replayed.
type exchange struct {
	name string
	req  provider.Request
	// failed says the provider answers this request with an error.
	failed bool
	check  func(t *testing.T, got outcome)
}

// outcome is everything one stream produced.
type outcome struct {
	text, thinking strings.Builder
	progress       map[string]*strings.Builder
	items          []transcript.Item
	usage          *transcript.Usage
	stop           provider.StopReason
	err            *provider.Error
}

func userSays(text string) []transcript.Item {
	return []transcript.Item{{User: &transcript.User{Parts: []transcript.Part{{Text: text}}}}}
}

var openRouterExchanges = []exchange{
	{
		name: "text",
		req:  provider.Request{Model: "openai/gpt-4o-mini", Items: userSays("Reply with the single word: ready"), MaxOutputTokens: 64},
		check: func(t *testing.T, got outcome) {
			require.Equal(t, provider.StopEndTurn, got.stop)
			require.Len(t, got.items, 1)
			require.NotEmpty(t, got.items[0].Text.Text)
		},
	},
	{
		name: "tool_call",
		req: provider.Request{
			Model: "openai/gpt-4o-mini", MaxOutputTokens: 256,
			System: []provider.SystemBlock{{Name: "base", Text: "Use the shell tool to answer."}},
			Items:  userSays("List the files in /tmp."),
			Tools: []provider.Tool{{
				Name: "shell", Description: "Run a shell command.",
				InputSchema: jsontext.Value(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
			}},
		},
		check: checkShellCall,
	},
	{
		name: "reasoning",
		req:  provider.Request{Model: "deepseek/deepseek-r1", Items: userSays("What is 17 times 23?"), MaxOutputTokens: 2048, Effort: "low"},
		check: func(t *testing.T, got outcome) {
			require.Equal(t, provider.StopEndTurn, got.stop)
			require.NotNil(t, got.items[0].Thinking, "the reasoning comes before the text")
			require.NotEmpty(t, got.items[0].Opaque)
			require.Positive(t, got.usage.ThoughtTokens)
		},
	},
	{
		name:   "unknown_model",
		req:    provider.Request{Model: "towa/no-such-model", Items: userSays("hi")},
		failed: true,
		check: func(t *testing.T, got outcome) {
			require.Equal(t, provider.Fail, got.err.Class)
			require.Equal(t, provider.ReasonRequest, got.err.Reason)
		},
	},
}

// ollamaModel is the model the build machine's Ollama server has loaded.
const ollamaModel = "llama3.2:3b"

var ollamaExchanges = []exchange{
	{
		name: "text",
		req:  provider.Request{Model: ollamaModel, Items: userSays("Reply with the single word: ready"), MaxOutputTokens: 64},
		check: func(t *testing.T, got outcome) {
			require.Equal(t, provider.StopEndTurn, got.stop)
			require.Len(t, got.items, 1)
			require.NotEmpty(t, got.items[0].Text.Text)
		},
	},
	{
		name: "tool_call",
		req: provider.Request{
			Model: ollamaModel, MaxOutputTokens: 256,
			Items: userSays("Call the shell tool with the command: ls /tmp"),
			Tools: []provider.Tool{{
				Name: "shell", Description: "Run a shell command.",
				InputSchema: jsontext.Value(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
			}},
		},
		check: checkShellCall,
	},
	{
		name:   "unknown_model",
		req:    provider.Request{Model: "towa-no-such-model", Items: userSays("hi")},
		failed: true,
		check: func(t *testing.T, got outcome) {
			require.Equal(t, provider.Fail, got.err.Class)
			require.Equal(t, provider.ReasonRequest, got.err.Reason)
		},
	},
}

func checkShellCall(t *testing.T, got outcome) {
	require.Equal(t, provider.StopToolUse, got.stop)
	var calls int
	for _, item := range got.items {
		if item.ToolCall == nil {
			continue
		}
		calls++
		require.Equal(t, "shell", item.ToolCall.Name)
		require.Equal(t, jsontext.Kind('{'), item.ToolCall.Input.Kind())
		// The argument deltas add up to the finished argument.
		require.JSONEq(t, string(item.ToolCall.Input), got.progress[item.ToolCall.ID].String())
	}
	require.Positive(t, calls)
}

var sources = []source{
	{
		name: "openrouter", env: "OPENROUTER_API_KEY", base: "https://openrouter.ai/api/v1", path: "/api/v1",
		provider: func(base string) config.Provider {
			return config.Provider{
				ID: "openrouter", Kind: config.ProviderChat, BaseURL: base, CacheKeyField: config.CacheKeySessionID,
				Referer: "https://github.com/gfx-labs/towa", Title: "towa",
				OpenRouter: config.OpenRouter{Enabled: true},
			}
		},
		exchanges: openRouterExchanges,
	},
	{
		// The build machine's Ollama server. It needs no key, so the
		// variable only says that recording from it is wanted.
		name: "ollama", env: "TOWA_RECORD_OLLAMA", base: "http://127.0.0.1:11434/v1", path: "/v1", token: "ollama",
		provider: func(base string) config.Provider {
			return config.Provider{ID: "ollama", Kind: config.ProviderChat, BaseURL: base}
		},
		exchanges: ollamaExchanges,
	},
}

func recordingPath(src source, name string) string {
	return filepath.Join("testdata", src.name+"_"+name+".json")
}

func run(t *testing.T, c *Client, req provider.Request, token string) outcome {
	t.Helper()
	req.Credential = auth.Credential{Account: "main", Scheme: auth.SchemeAPIKey, Token: token}
	req.CacheKey = "recording"
	got := outcome{progress: map[string]*strings.Builder{}}
	for ev, err := range c.Stream(t.Context(), req) {
		if err != nil {
			var ok bool
			got.err, ok = errors.AsType[*provider.Error](err)
			require.True(t, ok, "every error is a *provider.Error")
			break
		}
		require.Empty(t, got.stop, "nothing follows the stop event")
		switch {
		case ev.TextDelta != "":
			got.text.WriteString(ev.TextDelta)
		case ev.ThinkingDelta != "":
			got.thinking.WriteString(ev.ThinkingDelta)
		case ev.ToolCall != nil:
			if got.progress[ev.ToolCall.ID] == nil {
				got.progress[ev.ToolCall.ID] = &strings.Builder{}
			}
			got.progress[ev.ToolCall.ID].WriteString(ev.ToolCall.InputDelta)
		case ev.Item != nil:
			require.NoError(t, ev.Item.Validate())
			got.items = append(got.items, *ev.Item)
		case ev.Usage != nil:
			got.usage = ev.Usage
		case ev.Stop != "":
			got.stop = ev.Stop
		default:
			require.Fail(t, "an event with nothing set")
		}
	}
	return got
}

// TestRecord sends each exchange of a source to it and writes the recording,
// for the sources whose variable is set. Run it by hand:
//
//	OPENROUTER_API_KEY=... go test ./src/provider/chat -run TestRecord -count=1
//	TOWA_RECORD_OLLAMA=1 go test ./src/provider/chat -run TestRecord -count=1
func TestRecord(t *testing.T) {
	for _, src := range sources {
		t.Run(src.name, func(t *testing.T) {
			value := os.Getenv(src.env)
			if value == "" {
				t.Skip(src.env + " is not set")
			}
			token, secrets := src.token, []string(nil)
			if token == "" {
				token, secrets = value, []string{value}
			}
			require.NoError(t, os.MkdirAll("testdata", 0o755))
			for _, ex := range src.exchanges {
				t.Run(ex.name, func(t *testing.T) {
					httpClient := httpclient.New()
					httpClient.Transport = &providertest.Recorder{Next: httpClient.Transport, Path: recordingPath(src, ex.name), Secrets: secrets}
					c, err := New(Config{HTTPClient: httpClient, Provider: src.provider(src.base), UserAgent: "towa/test"})
					require.NoError(t, err)
					got := run(t, c, ex.req, token)
					require.Equal(t, ex.failed, got.err != nil, "%v", got.err)
				})
			}
		})
	}
}

// TestReplay runs each recording through the client. The replay server
// fails the test when the client's request is not the recorded one.
func TestReplay(t *testing.T) {
	for _, src := range sources {
		for _, ex := range src.exchanges {
			t.Run(src.name+"/"+ex.name, func(t *testing.T) {
				path := recordingPath(src, ex.name)
				if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
					t.Skipf("%s is not recorded: run TestRecord with %s set", path, src.env)
				}
				server := providertest.Replay(t, path)
				c, err := New(Config{HTTPClient: httpclient.New(), Provider: src.provider(server.URL + src.path), UserAgent: "towa/test"})
				require.NoError(t, err)
				got := run(t, c, ex.req, "replayed")
				contextName, err := c.ContextName(auth.Credential{Scheme: auth.SchemeAPIKey})
				require.NoError(t, err)

				if ex.failed {
					require.NotNil(t, got.err)
					require.NotEqual(t, http.StatusOK, got.err.Status)
					ex.check(t, got)
					return
				}
				require.Nil(t, got.err)
				require.NotEmpty(t, got.stop)
				var text, thinking strings.Builder
				for _, item := range got.items {
					require.Equal(t, src.name, item.Provider)
					require.Equal(t, contextName, item.Provider)
					require.Equal(t, &transcript.Origin{Model: ex.req.Model, Account: "main"}, item.Origin)
					switch {
					case item.Text != nil:
						text.WriteString(item.Text.Text)
					case item.Thinking != nil:
						thinking.WriteString(item.Thinking.Text)
					}
				}
				require.Equal(t, got.text.String(), text.String(), "the deltas add up to the finished text")
				require.Equal(t, got.thinking.String(), thinking.String())
				require.NotNil(t, got.usage)
				require.Positive(t, got.usage.InputTokens)
				require.Positive(t, got.usage.OutputTokens)
				// Only OpenRouter says what a request cost.
				require.Equal(t, src.name == "openrouter", got.usage.Cost != nil)
				ex.check(t, got)
			})
		}
	}
}
