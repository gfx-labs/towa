package anthropic_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/provider/anthropic"
	"github.com/gfx-labs/towa/src/provider/hostedsearch"
	"github.com/gfx-labs/towa/src/provider/providertest"
	"github.com/gfx-labs/towa/src/support/httpclient"
	"github.com/gfx-labs/towa/src/transcript"
)

// Recordings are made from a real account and never written by hand:
//
//	TOWA_RECORD_ANTHROPIC=1 TOWA_ANTHROPIC_MODEL=... ANTHROPIC_API_KEY=... \
//	    go test ./src/provider/anthropic -run TestExchange/api_
//	TOWA_RECORD_ANTHROPIC=1 TOWA_CLAUDE_MODEL=... TOWA_CLAUDE_OAUTH_TOKEN=... \
//	    go test ./src/provider/anthropic -run TestExchange/oauth_
//
// The OAuth model has to be Opus or Sonnet: Haiku accepts a request without
// the billing block, so it proves nothing about the request shape. A
// recording holds the model it was made with. Without the record variable a
// scenario replays its file, and is skipped when the file does not exist.
//
// The ollama_ scenarios are recorded from a local Ollama server, which
// serves the Messages shape and needs no key:
//
//	TOWA_RECORD_ANTHROPIC=1 TOWA_OLLAMA_URL=http://127.0.0.1:11434 TOWA_OLLAMA_MODEL=llama3.2:3b \
//	    go test ./src/provider/anthropic -run TestExchange/ollama_
//
// They show that the parser reads a real server's stream. They are Ollama's
// emulation, and show nothing about Anthropic's thinking blocks, cache
// usage, OAuth shape, errors or headers.
const (
	recordEnv = "TOWA_RECORD_ANTHROPIC"
	// version is fixed so that the user agent of a replay matches the
	// recording.
	version = "recording"
)

type scenario struct {
	name     string
	scheme   auth.Scheme
	prompt   string
	tools    []provider.Tool
	thinking anthropic.Thinking
	effort   string
	// maxOutput is what the scenario needs. Thinking needs room.
	maxOutput int64
	// badToken sends a token the API rejects, to record the refusal.
	badToken bool
	// ollama records from the local Ollama server.
	ollama bool
	// model is sent in place of the model from the environment, to record
	// the answer to a model that does not exist.
	model string
	check func(t *testing.T, events []provider.Event, err error)
}

var readTool = provider.Tool{
	Name:        "read",
	Description: "Read a file and return its content.",
	InputSchema: jsontext.Value(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
}

// checkHostedSearch expects the provider to have searched: a use and its
// result, both kept whole and paired by id, and an answer after them.
func checkHostedSearch(t *testing.T, events []provider.Event, err error) {
	require.NoError(t, err)
	started, done := map[string]bool{}, map[string]bool{}
	var answer strings.Builder
	for _, ev := range events {
		if ev.Item == nil {
			continue
		}
		if transcript.Is[transcript.Text](*ev.Item) {
			answer.WriteString(ev.Item.Body.(transcript.Text).Text)
		}
		if search, ok := hostedsearch.Read(*ev.Item); ok {
			if search.Started {
				require.NotEmpty(t, search.Query, "the streamed input was put back into the block")
				started[search.ID] = true
			}
			if search.Done {
				require.False(t, search.Failed)
				done[search.ID] = true
			}
		}
	}
	require.NotEmpty(t, started, "the response holds a search")
	require.Equal(t, started, done, "every search has its result")
	require.NotEmpty(t, answer.String())
	require.Equal(t, provider.StopEndTurn, events[len(events)-1].Stop)
}

func scenarios() []scenario {
	searchTool, ok, err := hostedsearch.Tool(config.WebSearch{Enabled: true, Hosted: true, HostedMaxUses: 1}, config.ProviderClaude, "")
	if err != nil || !ok {
		panic("the claude provider has a hosted search tool")
	}
	var out []scenario
	for _, ep := range []struct {
		prefix string
		scheme auth.Scheme
	}{{"api_", auth.SchemeAPIKey}, {"oauth_", auth.SchemeOAuth}} {
		out = append(out,
			scenario{
				name: ep.prefix + "web_search", scheme: ep.scheme, maxOutput: 1024,
				prompt: "Search the web for the Agent Client Protocol and reply with the address of its website.",
				tools:  []provider.Tool{searchTool},
				check:  checkHostedSearch,
			},
			scenario{
				name: ep.prefix + "text", scheme: ep.scheme, maxOutput: 256,
				prompt: "Reply with the single word: ready",
				check:  checkText,
			},
			scenario{
				name: ep.prefix + "tool_call", scheme: ep.scheme, maxOutput: 1024,
				prompt: "Read the file notes.txt with the read tool.", tools: []provider.Tool{readTool},
				check: checkToolCall,
			},
			scenario{
				name: ep.prefix + "thinking", scheme: ep.scheme, maxOutput: 4096, effort: "high",
				thinking: anthropic.Thinking{Mode: anthropic.ThinkingAdaptive, Display: "summarized"},
				prompt:   "Think about which is larger, 2^10 or 10^3, then read the file notes.txt with the read tool.",
				tools:    []provider.Tool{readTool},
				check:    checkThinking,
			},
			scenario{
				name: ep.prefix + "max_tokens", scheme: ep.scheme, maxOutput: 16,
				prompt: "Write three paragraphs about rivers.",
				check: func(t *testing.T, events []provider.Event, err error) {
					require.NoError(t, err)
					require.Equal(t, provider.StopMaxTokens, events[len(events)-1].Stop)
				},
			},
			scenario{
				name: ep.prefix + "rejected_credential", scheme: ep.scheme, maxOutput: 16, badToken: true,
				prompt: "Reply with the single word: ready",
				check: func(t *testing.T, _ []provider.Event, err error) {
					var perr *provider.Error
					require.ErrorAs(t, err, &perr)
					require.Equal(t, provider.ReasonAuth, perr.Reason)
					require.Equal(t, provider.Fallback, perr.Class)
				},
			},
		)
	}
	return append(out,
		scenario{
			name: "ollama_text", scheme: auth.SchemeAPIKey, ollama: true, maxOutput: 256,
			prompt: "Reply with the single word: ready",
			check:  checkText,
		},
		scenario{
			name: "ollama_tool_call", scheme: auth.SchemeAPIKey, ollama: true, maxOutput: 1024,
			prompt: "Read the file notes.txt with the read tool.", tools: []provider.Tool{readTool},
			check: checkToolCall,
		},
		scenario{
			name: "ollama_unknown_model", scheme: auth.SchemeAPIKey, ollama: true, maxOutput: 16,
			model:  "towa-no-such-model",
			prompt: "Reply with the single word: ready",
			check: func(t *testing.T, _ []provider.Event, err error) {
				var perr *provider.Error
				require.ErrorAs(t, err, &perr)
				require.Equal(t, provider.ReasonRequest, perr.Reason)
				require.Equal(t, provider.Fail, perr.Class)
			},
		},
	)
}

func checkText(t *testing.T, events []provider.Event, err error) {
	require.NoError(t, err)
	var deltas, finished strings.Builder
	for _, ev := range events {
		deltas.WriteString(ev.TextDelta)
		if ev.Item != nil && transcript.Is[transcript.Text](*ev.Item) {
			finished.WriteString(ev.Item.Body.(transcript.Text).Text)
		}
	}
	require.NotEmpty(t, finished.String())
	require.Equal(t, finished.String(), deltas.String(), "the deltas add up to the finished text")
	require.Equal(t, provider.StopEndTurn, events[len(events)-1].Stop)
	checkUsageBeforeStop(t, events)
}

func checkToolCall(t *testing.T, events []provider.Event, err error) {
	require.NoError(t, err)
	progress := map[string]*strings.Builder{}
	var call *transcript.ToolCall
	for _, ev := range events {
		if ev.ToolCall != nil {
			if progress[ev.ToolCall.ID] == nil {
				progress[ev.ToolCall.ID] = &strings.Builder{}
			}
			progress[ev.ToolCall.ID].WriteString(ev.ToolCall.InputDelta)
		}
		if ev.Item == nil {
			continue
		}
		if body, ok := ev.Item.Body.(transcript.ToolCall); ok {
			call = &body
		}
	}
	require.NotNil(t, call, "the response holds a tool call")
	require.Equal(t, jsontext.Kind('{'), call.Input.Kind())
	require.Contains(t, progress, call.ID, "progress was reported under the call's id")
	require.JSONEq(t, string(call.Input), progress[call.ID].String(), "the deltas add up to the input")
	require.Equal(t, provider.StopToolUse, events[len(events)-1].Stop)
	checkUsageBeforeStop(t, events)
}

func checkThinking(t *testing.T, events []provider.Event, err error) {
	checkToolCall(t, events, err)
	var thinking *transcript.Item
	for _, ev := range events {
		if ev.Item != nil && transcript.Is[transcript.Thinking](*ev.Item) {
			thinking = ev.Item
		}
	}
	require.NotNil(t, thinking, "the response holds thinking")
	require.NotEmpty(t, thinking.Opaque, "the signature is kept for the next request")
}

func checkUsageBeforeStop(t *testing.T, events []provider.Event) {
	require.GreaterOrEqual(t, len(events), 2)
	usage := events[len(events)-2].Usage
	require.NotNil(t, usage, "usage comes before the stop")
	require.Positive(t, usage.OutputTokens)
	require.Positive(t, usage.InputTokens+usage.CacheReadTokens+usage.CacheWriteTokens)
}

func (s scenario) request(model string, cred auth.Credential) provider.Request {
	return provider.Request{
		Model: model, Credential: cred,
		System:          []provider.SystemBlock{{Name: "instructions", Text: "You are towa, a coding agent under test. Follow the user's request exactly."}},
		Items:           []transcript.Item{{Body: transcript.User{Parts: []transcript.Part{{Text: s.prompt}}}}},
		Tools:           s.tools,
		Effort:          s.effort,
		MaxOutputTokens: s.maxOutput,
	}
}

func (s scenario) config(httpClient *http.Client, baseURL string) anthropic.Config {
	return anthropic.Config{
		HTTPClient: httpClient, Version: version, BaseURL: baseURL,
		Thinking: func(string) anthropic.Thinking { return s.thinking },
	}
}

func TestExchange(t *testing.T) {
	recording := os.Getenv(recordEnv) != ""
	for _, s := range scenarios() {
		t.Run(s.name, func(t *testing.T) {
			path := filepath.Join("testdata", s.name+".json")
			cred := auth.Credential{Account: "recorded", Scheme: s.scheme, Token: providertest.Redacted}
			httpClient := httpclient.New()
			var model, baseURL string
			if recording {
				model, cred.Token = os.Getenv("TOWA_ANTHROPIC_MODEL"), os.Getenv("ANTHROPIC_API_KEY")
				if s.scheme == auth.SchemeOAuth {
					model, cred.Token = os.Getenv("TOWA_CLAUDE_MODEL"), os.Getenv("TOWA_CLAUDE_OAUTH_TOKEN")
				}
				if s.ollama {
					// Ollama reads no key. The header is replaced in the
					// recording by its name.
					model, cred.Token, baseURL = os.Getenv("TOWA_OLLAMA_MODEL"), "ollama", os.Getenv("TOWA_OLLAMA_URL")
				}
				if cred.Token == "" || model == "" || (s.ollama && baseURL == "") {
					t.Skip("the credential or the model for this scheme is not in the environment")
				}
				if s.badToken {
					cred.Token = "towa-rejected-credential"
				}
				if s.model != "" {
					model = s.model
				}
				require.NoError(t, os.MkdirAll("testdata", 0o755))
				secrets := []string{cred.Token}
				if s.ollama {
					// The placeholder is no secret, and replacing the word
					// everywhere would change what the server said.
					secrets = nil
				}
				httpClient.Transport = &providertest.Recorder{Next: httpClient.Transport, Path: path, Secrets: secrets}
			} else {
				ex, err := providertest.Load(path)
				if errors.Is(err, fs.ErrNotExist) {
					t.Skipf("%s has not been recorded", path)
				}
				require.NoError(t, err)
				var body struct {
					Model string `json:"model"`
				}
				require.NoError(t, json.Unmarshal(ex.Request.Body, &body))
				model = body.Model
				// The request id is new for every request.
				baseURL = providertest.Replay(t, path, providertest.IgnoreHeader("X-Client-Request-Id")).URL
			}
			client, err := anthropic.New(s.config(httpClient, baseURL))
			require.NoError(t, err)

			req := s.request(model, cred)
			contextName, err := client.ContextName(cred)
			require.NoError(t, err)
			var events []provider.Event
			var streamErr error
			for ev, err := range client.Stream(t.Context(), req) {
				if err != nil {
					streamErr = err
					break
				}
				events = append(events, ev)
			}
			s.check(t, events, streamErr)

			// What came back must be sendable: the next request is built
			// from these items.
			for _, ev := range events {
				if ev.Item != nil {
					require.NoError(t, ev.Item.Validate())
					require.Equal(t, anthropic.Provider, ev.Item.Provider)
					require.Equal(t, contextName, ev.Item.Provider)
					require.Equal(t, &transcript.Origin{Model: req.Model, Account: cred.Account}, ev.Item.Origin)
					req.Items = append(req.Items, *ev.Item)
				}
			}
			requireBuilds(t, s, req)
		})
	}
}

// requireBuilds sends req to an address nothing listens on. A request the
// client cannot build from its items fails before that, as an invalid
// request.
func requireBuilds(t *testing.T, s scenario, req provider.Request) {
	t.Helper()
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	client, err := anthropic.New(s.config(httpclient.New(), closed.URL))
	require.NoError(t, err)
	for _, err := range client.Stream(t.Context(), req) {
		var perr *provider.Error
		require.ErrorAs(t, err, &perr)
		require.Equal(t, provider.ReasonNetwork, perr.Reason, "the request was built and sent: %v", err)
	}
}

// sent is a request as it arrived at a server.
type sent struct {
	header http.Header
	query  string
	body   string
}

// capture sends req and returns what arrived. The server answers 500: only
// the request is looked at.
func capture(t *testing.T, cfg anthropic.Config, req provider.Request) sent {
	t.Helper()
	var got sent
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		got = sent{header: r.Header.Clone(), query: r.URL.RawQuery, body: string(body)}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	cfg.HTTPClient, cfg.Version, cfg.BaseURL = httpclient.New(), version, server.URL
	client, err := anthropic.New(cfg)
	require.NoError(t, err)
	for _, err := range client.Stream(t.Context(), req) {
		require.Error(t, err)
	}
	require.NotEmpty(t, got.body, "the request reached the server")
	return got
}

// A proxy that holds the account's token swaps the authorization and
// nothing else, so a proxied credential must not change any other byte.
func TestProxiedCredentialGetsTheSameRequest(t *testing.T) {
	for _, scheme := range []auth.Scheme{auth.SchemeOAuth, auth.SchemeAPIKey} {
		t.Run(string(scheme), func(t *testing.T) {
			s := scenario{prompt: "hello", tools: []provider.Tool{readTool}, maxOutput: 64}
			cfg := anthropic.Config{Thinking: func(string) anthropic.Thinking {
				return anthropic.Thinking{Mode: anthropic.ThinkingAdaptive}
			}}
			held := capture(t, cfg, s.request("model", auth.Credential{Account: "work", Scheme: scheme, Token: "placeholder"}))
			proxied := capture(t, cfg, s.request("model", auth.Credential{Account: "work", Scheme: scheme, Token: "placeholder", Proxied: true}))

			// The request id is new for every request.
			held.header.Del("X-Client-Request-Id")
			proxied.header.Del("X-Client-Request-Id")
			require.Equal(t, held, proxied)
		})
	}
}
