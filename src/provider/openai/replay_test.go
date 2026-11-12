package openai_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/auth"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/httpclient"
	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/provider/hostedsearch"
	"github.com/gfx-labs/acpharness/src/provider/openai"
	"github.com/gfx-labs/acpharness/src/provider/providertest"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// Recordings are made from a real account and never written by hand:
//
//	ACPHARNESS_RECORD_OPENAI=1 OPENAI_API_KEY=... go test ./src/provider/openai -run TestExchange/api_
//	ACPHARNESS_RECORD_OPENAI=1 ACPHARNESS_CODEX_TOKEN=... ACPHARNESS_CODEX_ACCOUNT_ID=... \
//	    go test ./src/provider/openai -run TestExchange/codex_
//
// ACPHARNESS_OPENAI_MODEL and ACPHARNESS_CODEX_MODEL name the models, and a
// replay reads the model back from the recording. ACPHARNESS_OPENAI_BASE_URL
// points the API key scenarios at another Responses-compatible server, and
// ACPHARNESS_OPENAI_EFFORT, when set, replaces their reasoning effort: an
// empty value sends none, for a model that has no reasoning.
// Without the record variable a scenario replays its file, and is skipped
// when the file does not exist.
//
// The api_ recordings in testdata were made from a local Ollama server, not
// from OpenAI: see docs/notes/2026-11-11-provider-openai.md for what that
// does and does not show.
const (
	recordEnv     = "ACPHARNESS_RECORD_OPENAI"
	cacheKey      = "acpharness-recording"
	accountInTest = "recorded"
)

type scenario struct {
	name   string
	scheme auth.Scheme
	effort string
	prompt string
	// model replaces the model from the environment when recording.
	model string
	tools []provider.Tool
	// maxOutput provokes an incomplete response when set.
	maxOutput int64
	check     func(t *testing.T, events []provider.Event, err error)
}

var readTool = provider.Tool{
	Name:        "read",
	Description: "Read a file and return its content.",
	InputSchema: jsontext.Value(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
}

// patchTool is a free-text tool, which only the Responses API has.
var patchTool = provider.Tool{NativeAPI: provider.NativeAPIResponses, Native: jsontext.Value(
	`{"type":"custom","name":"apply_patch","description":"Apply a patch. The input is the patch text."}`)}

// checkHostedSearch expects the provider to have searched: an item that is
// kept whole, that reads as a finished search, and an answer after it.
func checkHostedSearch(t *testing.T, events []provider.Event, err error) {
	require.NoError(t, err)
	var searches int
	var answer strings.Builder
	var items []transcript.Item
	for _, ev := range events {
		if ev.Item == nil {
			continue
		}
		items = append(items, *ev.Item)
		if ev.Item.Text != nil {
			answer.WriteString(ev.Item.Text.Text)
		}
		if search, ok := hostedsearch.Read(*ev.Item); ok {
			searches++
			require.NotEmpty(t, search.ID)
			require.True(t, search.Done)
			require.False(t, search.Failed)
			require.NotEmpty(t, search.Output)
		}
	}
	require.Positive(t, searches, "the response holds a search")
	require.NotEmpty(t, answer.String())
	require.Equal(t, provider.StopEndTurn, events[len(events)-1].Stop)

	// A request that does not declare the hosted tool, as after a fallback
	// to another provider, gets the search as text and no item of this API.
	portable := hostedsearch.Portable(items, "another-provider", transcript.Origin{Model: "m", Account: "a"}, false)
	var standIns int
	for _, item := range portable {
		_, hosted := hostedsearch.Read(item)
		require.False(t, hosted)
		if item.Text != nil && strings.HasPrefix(item.Text.Text, "["+hostedsearch.Name+" for ") {
			standIns++
			require.Empty(t, item.Opaque)
		}
	}
	require.Equal(t, searches, standIns)
	// The request that made them, with the tool declared, gets them back whole.
	same := hostedsearch.Portable(items, items[0].Provider, *items[0].Origin, true)
	require.Equal(t, items, same)
}

func scenarios() []scenario {
	searchTool, ok, err := hostedsearch.Tool(config.WebSearch{Enabled: true, Hosted: true}, config.ProviderCodex, "")
	if err != nil || !ok {
		panic("the codex provider has a hosted search tool")
	}
	var out []scenario
	for _, ep := range []struct {
		prefix string
		scheme auth.Scheme
	}{{"api_", auth.SchemeAPIKey}, {"codex_", auth.SchemeOAuth}} {
		out = append(out,
			scenario{
				name: ep.prefix + "web_search", scheme: ep.scheme, effort: "low",
				prompt: "Search the web for the Agent Client Protocol and reply with the address of its website.",
				tools:  []provider.Tool{searchTool},
				check:  checkHostedSearch,
			},
			scenario{
				name: ep.prefix + "text", scheme: ep.scheme, effort: "low",
				prompt: "Reply with the single word: ready",
				check:  checkText,
			},
			scenario{
				name: ep.prefix + "function_call", scheme: ep.scheme, effort: "low",
				prompt: "Read the file notes.txt with the read tool.", tools: []provider.Tool{readTool},
				check: checkToolCall('{'),
			},
			scenario{
				name: ep.prefix + "custom_call", scheme: ep.scheme, effort: "low",
				prompt: "Use apply_patch to add a file hello.txt containing the line hello.", tools: []provider.Tool{patchTool},
				check: checkToolCall('"'),
			},
		)
	}
	// The Codex backend takes no output limit, so only the API can be cut
	// short on purpose.
	return append(out, scenario{
		name: "api_max_output_tokens", scheme: auth.SchemeAPIKey, effort: "low", maxOutput: 16,
		prompt: "Write three paragraphs about rivers.",
		check: func(t *testing.T, events []provider.Event, err error) {
			require.NoError(t, err)
			require.Equal(t, provider.StopMaxTokens, events[len(events)-1].Stop)
		},
	}, scenario{
		name: "api_unknown_model", scheme: auth.SchemeAPIKey, model: "acpharness-no-such-model",
		prompt: "Reply with the single word: ready",
		check: func(t *testing.T, events []provider.Event, err error) {
			var perr *provider.Error
			require.ErrorAs(t, err, &perr)
			require.Empty(t, events)
			// Sending the same model name again cannot help.
			require.Equal(t, provider.Fail, perr.Class)
			require.Equal(t, provider.ReasonRequest, perr.Reason)
			require.NotZero(t, perr.Status)
			require.Contains(t, perr.Message, "acpharness-no-such-model", "the message is the server's own")
		},
	})
}

func checkText(t *testing.T, events []provider.Event, err error) {
	require.NoError(t, err)
	var deltas, finished strings.Builder
	for _, ev := range events {
		deltas.WriteString(ev.TextDelta)
		if ev.Item != nil && ev.Item.Text != nil {
			finished.WriteString(ev.Item.Text.Text)
		}
	}
	require.NotEmpty(t, finished.String())
	require.Equal(t, finished.String(), deltas.String(), "the deltas add up to the finished text")
	require.Equal(t, provider.StopEndTurn, events[len(events)-1].Stop)
	checkUsageBeforeStop(t, events)
}

// checkToolCall expects one call whose input is a JSON object for a
// function tool and a JSON string for a free-text tool.
func checkToolCall(kind jsontext.Kind) func(*testing.T, []provider.Event, error) {
	return func(t *testing.T, events []provider.Event, err error) {
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
			if ev.Item != nil && ev.Item.ToolCall != nil {
				call = ev.Item.ToolCall
			}
		}
		require.NotNil(t, call, "the response holds a tool call")
		require.True(t, call.Input.IsValid())
		require.Equal(t, kind, call.Input.Kind())
		require.Contains(t, progress, call.ID, "progress was reported under the call's id")
		require.Equal(t, provider.StopToolUse, events[len(events)-1].Stop)
		checkUsageBeforeStop(t, events)
	}
}

func checkUsageBeforeStop(t *testing.T, events []provider.Event) {
	require.GreaterOrEqual(t, len(events), 2)
	usage := events[len(events)-2].Usage
	require.NotNil(t, usage, "usage comes before the stop")
	require.Positive(t, usage.OutputTokens)
}

func (s scenario) request(model string, cred auth.Credential) provider.Request {
	req := provider.Request{
		Model: model, Credential: cred,
		System:          []provider.SystemBlock{{Name: "instructions", Text: "You are a coding agent under test. Follow the user's request exactly."}},
		Items:           []transcript.Item{{User: &transcript.User{Parts: []transcript.Part{{Text: s.prompt}}}}},
		Tools:           s.tools,
		Effort:          s.effort,
		CacheKey:        cacheKey,
		MaxOutputTokens: s.maxOutput,
	}
	return req
}

func TestExchange(t *testing.T) {
	recording := os.Getenv(recordEnv) != ""
	for _, s := range scenarios() {
		t.Run(s.name, func(t *testing.T) {
			path := filepath.Join("testdata", s.name+".json")
			cred := auth.Credential{Account: accountInTest, Scheme: s.scheme, Token: providertest.Redacted}
			model := os.Getenv("ACPHARNESS_OPENAI_MODEL")
			if s.scheme == auth.SchemeOAuth {
				model = os.Getenv("ACPHARNESS_CODEX_MODEL")
				cred.ChatGPTAccountID = providertest.Redacted
			}
			httpClient := httpclient.New()
			cfg := openai.Config{HTTPClient: httpClient}
			switch {
			case recording:
				cred.Token = os.Getenv("OPENAI_API_KEY")
				if s.scheme == auth.SchemeOAuth {
					cred.Token = os.Getenv("ACPHARNESS_CODEX_TOKEN")
					cred.ChatGPTAccountID = os.Getenv("ACPHARNESS_CODEX_ACCOUNT_ID")
				}
				if cred.Token == "" || model == "" {
					t.Skip("the credential or the model for this endpoint is not in the environment")
				}
				if s.model != "" {
					model = s.model
				}
				if s.scheme == auth.SchemeAPIKey {
					cfg.BaseURL = os.Getenv("ACPHARNESS_OPENAI_BASE_URL")
					if effort, ok := os.LookupEnv("ACPHARNESS_OPENAI_EFFORT"); ok {
						s.effort = effort
					}
				}
				require.NoError(t, os.MkdirAll("testdata", 0o755))
				httpClient.Transport = &providertest.Recorder{
					Next: httpClient.Transport, Path: path,
					Secrets: []string{cred.Token, cred.ChatGPTAccountID},
				}
			default:
				ex, err := providertest.Load(path)
				if errors.Is(err, fs.ErrNotExist) {
					t.Skipf("%s has not been recorded", path)
				}
				require.NoError(t, err)
				model, s.effort = recordedSettings(t, ex)
				server := providertest.Replay(t, path)
				cfg.BaseURL, cfg.CodexBaseURL = server.URL+"/v1", server.URL+"/backend-api/codex"
			}
			client, err := openai.New(cfg)
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
			// from these items, and one this client cannot render would
			// end the session.
			for _, ev := range events {
				if ev.Item != nil {
					require.NoError(t, ev.Item.Validate())
					require.Equal(t, contextName, ev.Item.Provider)
					require.Equal(t, &transcript.Origin{Model: req.Model, Account: cred.Account}, ev.Item.Origin)
					req.Items = append(req.Items, *ev.Item)
				}
			}
			requireBuilds(t, req)
		})
	}
}

// requireBuilds sends req to an address nothing listens on. A request the
// client cannot build from its items fails before that, as an invalid
// request.
func requireBuilds(t *testing.T, req provider.Request) {
	t.Helper()
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	client, err := openai.New(openai.Config{HTTPClient: httpclient.New(), BaseURL: closed.URL, CodexBaseURL: closed.URL})
	require.NoError(t, err)
	for _, err := range client.Stream(t.Context(), req) {
		var perr *provider.Error
		require.ErrorAs(t, err, &perr)
		require.Equal(t, provider.ReasonNetwork, perr.Reason, "the request was built and sent: %v", err)
	}
}

// recordedSettings returns the model and the reasoning effort a recording
// was made with. They are the two parts of a request that the environment
// chose, so a replay takes them from the file.
func recordedSettings(t *testing.T, ex *providertest.Exchange) (model, effort string) {
	t.Helper()
	var body struct {
		Model     string `json:"model"`
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	require.NoError(t, json.Unmarshal(ex.Request.Body, &body))
	require.NotEmpty(t, body.Model, "the recorded request names no model")
	return body.Model, body.Reasoning.Effort
}
