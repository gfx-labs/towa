package cli

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"uuid"

	"github.com/gfx-labs/towa/pkg/acp"
	"github.com/gfx-labs/towa/src/agent/extras"
	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/prompt/agentctx"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/provider/hostedsearch"
	"github.com/gfx-labs/towa/src/provider/models"
	"github.com/gfx-labs/towa/src/support/telemetry"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/core"
	"github.com/gfx-labs/towa/src/tools/fsops"
	"github.com/gfx-labs/towa/src/tools/mcp"
	"github.com/gfx-labs/towa/src/transcript"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestRuntimeOwnsMCPInventoryAndReadAuthority(t *testing.T) {
	server := newRuntimeMCP(t)
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	cfg := loaded.Config
	cfg.MCP.SessionServersDirect = true
	home := t.TempDir()
	host := Host{Stdout: io.Discard, Stderr: io.Discard, Environ: []string{"HOME=" + home, "XDG_STATE_HOME=" + filepath.Join(home, "state")}}
	tel, err := telemetry.Start(t.Context(), cfg.Telemetry, version(), io.Discard)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tel.Shutdown(context.Background())) })
	factory, err := newRuntimeFactory(t.Context(), cfg, host, tel)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, factory.Close()) })
	parentTurn := loop.Turn{
		SessionID: acp.SessionId(uuid.NewV7().String()), Cwd: home,
		Update:     func(context.Context, acp.SessionUpdate) error { return nil },
		McpServers: []acp.McpServer{{Http: &acp.McpServerHttpInline{Name: "inventory", Url: server.url}}},
	}
	parent, err := factory.open(t.Context(), parentTurn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, parent.Close(context.Background())) })
	require.Equal(t, []string{"first"}, runtimeToolNames(parent.frozen))
	opened, closed := server.sessions()
	require.Len(t, opened, 1)
	require.Empty(t, closed)
	parentSession := opened[0]

	server.add("second")
	childTurn := parentTurn
	childTurn.ParentID = parentTurn.SessionID
	childTurn.SessionID = acp.SessionId(uuid.NewV7().String())
	child, err := factory.open(t.Context(), childTurn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, child.Close(context.Background())) })
	require.ElementsMatch(t, []string{"first", "second"}, runtimeToolNames(child.frozen))
	require.Equal(t, []string{"first"}, runtimeToolNames(parent.frozen), "the parent's inventory must remain frozen")
	opened, closed = server.sessions()
	require.Len(t, opened, 2)
	require.NotEqual(t, parentSession, opened[1])
	require.Empty(t, closed)
	childSession := opened[1]

	path := filepath.Join(home, "authority.txt")
	require.NoError(t, os.WriteFile(path, []byte("before\n"), 0o600))
	roots := fsops.RootsOf(tools.Env{Cwd: home}, cfg.Tools.Files)
	_, err = core.ReadFile(t.Context(), roots, cfg.Tools.Files, parent.reads, fsops.ReadInput{Path: path})
	require.NoError(t, err)
	var unread *core.UnreadError
	err = child.reads.Edit(t.Context(), path, func() error { return os.WriteFile(path, []byte("child\n"), 0o600) })
	require.ErrorAs(t, err, &unread)
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "before\n", string(body))
	require.NoError(t, parent.reads.Edit(t.Context(), path, func() error { return os.WriteFile(path, []byte("parent\n"), 0o600) }))

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	require.NoError(t, child.Close(context.WithoutCancel(canceled)))
	_, closed = server.sessions()
	require.Equal(t, []string{childSession}, closed, "closing a child must not close the parent's MCP session")
	_, err = child.manager.ListTools(t.Context(), "inventory")
	require.ErrorIs(t, err, mcp.ErrClosed)
	// No model is configured: Run opens the actual child runtime, fails
	// preparation before a provider request, then closes that runtime.
	failedTurn := childTurn
	failedTurn.SessionID = acp.SessionId(uuid.NewV7().String())
	failedTurn.Prompt = []acp.ContentBlock{{Text: &acp.ContentBlockText{Type: "text", Text: "hi"}}}
	_, err = factory.Run(t.Context(), failedTurn)
	var noModel *models.NoModelError
	require.ErrorAs(t, err, &noModel)
	opened, closed = server.sessions()
	require.Len(t, opened, 3)
	require.ElementsMatch(t, []string{childSession, opened[2]}, closed)
	result, err := parent.frozen[0].Run(t.Context(), tools.Call{Input: jsontext.Value(`{}`)})
	require.NoError(t, err)
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)
	require.NotEmpty(t, result.Parts)
	require.Contains(t, result.Parts[0].Text, "first")
	require.NoError(t, parent.Close(context.Background()))
	_, closed = server.sessions()
	require.ElementsMatch(t, []string{childSession, opened[2], parentSession}, closed)
}

func runtimeToolNames(list []tools.Tool) []string {
	names := make([]string, len(list))
	for i, tool := range list {
		names[i] = tool.Spec.Name
	}
	return names
}

// The tools and rules sections name only the tools a request is sent: a
// disabled tool has no line, and an entry whose provider searches gets the
// lines of that provider's tool where another entry gets ours.
func TestPromptToolsSectionFollowsTheToolsOfTheEntry(t *testing.T) {
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	cfg := loaded.Config
	cfg.ConversationSearch.Enabled = false
	cfg.WebSearch.Enabled = true
	cfg.Providers = []config.Provider{{ID: "other", Kind: config.ProviderResponses, BaseURL: "http://127.0.0.1:1/v1"}}
	cfg.Accounts = []config.Account{
		{ID: "chat", Provider: config.ProviderCodex, Kind: config.AccountProxiedOAuth},
		{ID: "local", Provider: "other", Kind: config.AccountAPIKey, APIKeyEnv: "RUNTIME_TEST_KEY"},
	}
	const plainModel, hostedModel = "other:local/gpt-5.5", "codex:chat/gpt-5.5#medium"
	cfg.Models = []string{plainModel, hostedModel}
	cfg.ModelContextWindow, cfg.ModelMaxOutputTokens = 100_000, 1024
	home := t.TempDir()
	host := Host{Stdout: io.Discard, Stderr: io.Discard, Environ: []string{"HOME=" + home, "XDG_STATE_HOME=" + filepath.Join(home, "state"), "RUNTIME_TEST_KEY=placeholder"}}
	tel, err := telemetry.Start(t.Context(), cfg.Telemetry, version(), io.Discard)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tel.Shutdown(context.Background())) })
	factory, err := newRuntimeFactory(t.Context(), cfg, host, tel)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, factory.Close()) })

	section := func(model, effort string) string {
		turn := loop.Turn{
			SessionID: acp.SessionId(uuid.NewV7().String()), Cwd: home, Model: model,
			Update: func(context.Context, acp.SessionUpdate) error { return nil },
		}
		loaded, err := factory.loadContext(turn)
		require.NoError(t, err)
		prepared, err := factory.prepare(t.Context(), turn, nil, &core.ReadTracker{}, loaded, &promptCache{})
		require.NoError(t, err)
		// The entry's effort goes with the turn's requests as written.
		require.Equal(t, effort, prepared.resolved.Effort)
		var text string
		for _, block := range prepared.system {
			if block.Name == agentctx.SectionTools || block.Name == agentctx.SectionRules {
				text += block.Text + "\n"
			}
		}
		require.Contains(t, text, "<tools>")
		require.Contains(t, text, "<rules>")
		return text
	}
	plain, hosted := section(plainModel, ""), section(hostedModel, "medium")
	require.Contains(t, plain, "- "+extras.NameWebSearch+": ")
	require.Contains(t, hosted, "- "+hostedsearch.Name+": ")
	require.Contains(t, plain, "with "+extras.NameWebSearch+".")
	require.Contains(t, hosted, "with "+hostedsearch.Name+".")
	require.NotContains(t, hosted, extras.NameWebSearch)
	for _, text := range []string{plain, hosted} {
		require.Contains(t, text, "no terminal")
		require.NotContains(t, text, extras.NameConversationSearch)
	}
}

// A session's prompt holds its context files as they were when it opened. A
// file changed during the session is in the prompt of the next session.
func TestContextFilesAreReadWhenTheSessionOpens(t *testing.T) {
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	cfg := loaded.Config
	cfg.ConversationSearch.Enabled = false
	cfg.Providers = []config.Provider{{ID: "other", Kind: config.ProviderResponses, BaseURL: "http://127.0.0.1:1/v1"}}
	cfg.Accounts = []config.Account{{ID: "local", Provider: "other", Kind: config.AccountNone}}
	const model = "other:local/gpt-5.5"
	cfg.Models = []string{model}
	cfg.ModelContextWindow, cfg.ModelMaxOutputTokens = 100_000, 1024
	home := t.TempDir()
	rules := filepath.Join(home, "AGENTS.md")
	require.NoError(t, os.WriteFile(rules, []byte("first rule\n"), 0o600))
	host := Host{Stdout: io.Discard, Stderr: io.Discard, Environ: []string{"HOME=" + home, "XDG_STATE_HOME=" + filepath.Join(home, "state")}}
	tel, err := telemetry.Start(t.Context(), cfg.Telemetry, version(), io.Discard)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tel.Shutdown(context.Background())) })
	factory, err := newRuntimeFactory(t.Context(), cfg, host, tel)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, factory.Close()) })

	open := func() *sessionRuntime {
		runtime, err := factory.open(t.Context(), loop.Turn{
			SessionID: acp.SessionId(uuid.NewV7().String()), Cwd: home, Model: model,
			Update: func(context.Context, acp.SessionUpdate) error { return nil },
		})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
		return runtime
	}
	projectContext := func(r *sessionRuntime) string {
		turn := loop.Turn{SessionID: acp.SessionId(uuid.NewV7().String()), Cwd: home, Model: model}
		prepared, err := factory.prepare(t.Context(), turn, r.frozen, r.reads, r.loaded, &r.prompts)
		require.NoError(t, err)
		for _, block := range prepared.system {
			if block.Name == agentctx.SectionProjectContext {
				return block.Text
			}
		}
		t.Fatal("the prompt has no project context section")
		return ""
	}

	first := open()
	require.Contains(t, projectContext(first), "first rule")
	require.NoError(t, os.WriteFile(rules, []byte("second rule\n"), 0o600))
	require.Contains(t, projectContext(first), "first rule")
	require.NotContains(t, projectContext(first), "second rule")
	require.Contains(t, projectContext(open()), "second rule")
}

// runtimeMCP observes real SDK sessions, not replacement cleanup callbacks.
type runtimeMCP struct {
	server *sdk.Server
	url    string
	mu     sync.Mutex
	opened []string
	closed []string
}

func newRuntimeMCP(t *testing.T) *runtimeMCP {
	t.Helper()
	m := &runtimeMCP{server: sdk.NewServer(&sdk.Implementation{Name: "runtime-isolation", Version: "1"}, nil)}
	m.add("first")
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return m.server }, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			m.mu.Lock()
			m.closed = append(m.closed, r.Header.Get("Mcp-Session-Id"))
			m.mu.Unlock()
		}
		handler.ServeHTTP(w, r)
		if r.Method == http.MethodPost && r.Header.Get("Mcp-Session-Id") == "" {
			if id := w.Header().Get("Mcp-Session-Id"); id != "" {
				m.mu.Lock()
				m.opened = append(m.opened, id)
				m.mu.Unlock()
			}
		}
	}))
	m.url = server.URL
	t.Cleanup(server.Close)
	return m
}

func (m *runtimeMCP) add(name string) {
	sdk.AddTool(m.server, &sdk.Tool{Name: name, Description: "Return the name of this test server tool."},
		func(context.Context, *sdk.CallToolRequest, map[string]any) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: name}}}, nil, nil
		})
}

func (m *runtimeMCP) sessions() (opened, closed []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.opened), slices.Clone(m.closed)
}

// A request that the selected entry's account cannot make goes to the entry
// listed after it, with that entry's model and effort. An entry without an
// effort sends none.
func TestAFallbackSendsTheNextEntrysEffort(t *testing.T) {
	type sent struct {
		Model     string `json:"model"`
		Reasoning *struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	var mu sync.Mutex
	var requests []sent
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body sent
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		requests = append(requests, body)
		mu.Unlock()
		// No model reply is invented: every entry is refused.
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	cfg := loaded.Config
	cfg.Providers = []config.Provider{{ID: "local", Kind: config.ProviderResponses, BaseURL: server.URL + "/v1"}}
	cfg.Accounts = []config.Account{{ID: "key", Provider: "local", Kind: config.AccountAPIKey, APIKeyEnv: "RUNTIME_TEST_KEY"}}
	cfg.Models = []string{"local:key/first#high", "local:key/second", "local:key/third#low"}
	cfg.ModelContextWindow, cfg.ModelMaxOutputTokens = 100_000, 1024
	cfg.Limits.Retry.MaxRetries = 0
	home := t.TempDir()
	host := Host{Stdout: io.Discard, Stderr: io.Discard, Environ: []string{"HOME=" + home, "XDG_STATE_HOME=" + filepath.Join(home, "state"), "RUNTIME_TEST_KEY=placeholder"}}
	tel, err := telemetry.Start(t.Context(), cfg.Telemetry, version(), io.Discard)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tel.Shutdown(context.Background())) })
	factory, err := newRuntimeFactory(t.Context(), cfg, host, tel)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, factory.Close()) })

	run := func(model string) []sent {
		mu.Lock()
		requests = nil
		mu.Unlock()
		turn := loop.Turn{
			SessionID: acp.SessionId(uuid.NewV7().String()), Cwd: home, Model: model,
			Prompt: []acp.ContentBlock{{Text: &acp.ContentBlockText{Type: "text", Text: "hello"}}},
			Update: func(context.Context, acp.SessionUpdate) error { return nil },
		}
		runtime, err := factory.open(t.Context(), turn)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
		_, err = runtime.Run(t.Context(), turn)
		require.Error(t, err)
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(requests)
	}
	efforts := func(list []sent) (out []string) {
		for _, r := range list {
			effort := ""
			if r.Reasoning != nil {
				effort = r.Reasoning.Effort
			}
			out = append(out, r.Model+"#"+effort)
		}
		return slices.Compact(out)
	}
	// The session's first model goes down the whole list.
	require.Equal(t, []string{"first#high", "second#", "third#low"}, efforts(run("")))
	// A model chosen further down starts there and does not go back up.
	require.Equal(t, []string{"second#", "third#low"}, efforts(run("local:key/second")))
}

func TestPromptIsBuiltAgainOnlyWhenItsInputsChange(t *testing.T) {
	t.Parallel()
	var cache promptCache
	prompt := agentctx.Prompt{Instructions: "be brief", Rules: []string{"read before editing"}, WorkDir: "/work", AdditionalDirs: []string{"/other"}}
	text := func(blocks []provider.SystemBlock) string {
		var b strings.Builder
		for _, block := range blocks {
			b.WriteString(block.Text)
		}
		return b.String()
	}
	first, err := cache.build(false, prompt)
	require.NoError(t, err)
	again, err := cache.build(false, prompt)
	require.NoError(t, err)
	require.Same(t, &first[0], &again[0])

	// The session's MCP tools arrive after its first prompt was built.
	prompt.Rules = []string{"read before editing", "ask the tracker for issues"}
	changed, err := cache.build(false, prompt)
	require.NoError(t, err)
	require.Contains(t, text(changed), "ask the tracker for issues")

	// A kept prompt does not follow a caller that reuses its slice.
	dirs := []string{"/other"}
	prompt.AdditionalDirs = dirs
	_, err = cache.build(false, prompt)
	require.NoError(t, err)
	dirs[0] = "/elsewhere"
	moved, err := cache.build(false, prompt)
	require.NoError(t, err)
	require.Contains(t, text(moved), "/elsewhere")

	// Provider-run web search has its own prompt.
	hosted, err := cache.build(true, prompt)
	require.NoError(t, err)
	require.NotSame(t, &moved[0], &hosted[0])
}
