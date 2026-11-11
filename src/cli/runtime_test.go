package cli

import (
	"context"
	"encoding/json/jsontext"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"uuid"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/loop"
	"github.com/gfx-labs/acpharness/src/mcp"
	"github.com/gfx-labs/acpharness/src/models"
	"github.com/gfx-labs/acpharness/src/telemetry"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/core"
	"github.com/gfx-labs/acpharness/src/tools/fsops"
	"github.com/gfx-labs/acpharness/src/transcript"
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
