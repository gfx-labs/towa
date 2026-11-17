package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	acp "github.com/gfx-labs/go-acp"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// acpProvider is the ACP client's side of the binding: it answers
// mcp/message with an MCP SDK server, over one in-memory connection that
// carries every request. It is what a client such as protocol7 runs.
type acpProvider struct {
	serverID acp.UnstableMcpServerAcpId
	conn     sdk.Connection

	mu      sync.Mutex
	waiting map[string]chan *jsonrpc.Response
	// requests counts the mcp/message requests it was sent, by method.
	requests map[string]int
}

func newACPProvider(t *testing.T, server *sdk.Server, id acp.UnstableMcpServerAcpId) *acpProvider {
	t.Helper()
	serverEnd, clientEnd := sdk.NewInMemoryTransports()
	_, err := server.Connect(t.Context(), serverEnd, nil)
	require.NoError(t, err)
	conn, err := clientEnd.Connect(t.Context())
	require.NoError(t, err)
	p := &acpProvider{serverID: id, conn: conn, waiting: map[string]chan *jsonrpc.Response{}, requests: map[string]int{}}
	go func() {
		for {
			msg, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			if response, ok := msg.(*jsonrpc.Response); ok {
				id, _ := response.ID.Raw().(string)
				p.mu.Lock()
				ch := p.waiting[id]
				delete(p.waiting, id)
				p.mu.Unlock()
				if ch != nil {
					ch <- response
				}
			}
		}
	}()
	t.Cleanup(func() { _ = conn.Close() })
	return p
}

// UnstableMessageMcp runs one inner request, with its logical request ID
// as the inner JSON-RPC ID, as the binding says.
func (p *acpProvider) UnstableMessageMcp(ctx context.Context, params acp.UnstableMessageMcpRequest) (acp.UnstableMessageMcpResponse, error) {
	if params.ServerId != p.serverID {
		return acp.UnstableMessageMcpResponse{}, acp.NewInvalidParams(map[string]any{"error": "unknown server"})
	}
	id := string(params.RequestId)
	innerID, err := jsonrpc.MakeID(id)
	if err != nil {
		return acp.UnstableMessageMcpResponse{}, err
	}
	raw, err := json.Marshal(params.Params)
	if err != nil {
		return acp.UnstableMessageMcpResponse{}, err
	}
	ch := make(chan *jsonrpc.Response, 1)
	p.mu.Lock()
	p.waiting[id] = ch
	p.requests[params.Method]++
	p.mu.Unlock()
	if err := p.conn.Write(ctx, &jsonrpc.Request{ID: innerID, Method: params.Method, Params: raw}); err != nil {
		return acp.UnstableMessageMcpResponse{}, err
	}
	select {
	case response := <-ch:
		if response.Error != nil {
			var wire *jsonrpc.Error
			if !errors.As(response.Error, &wire) {
				return acp.UnstableMessageMcpResponse{}, response.Error
			}
			return acp.UnstableMessageMcpResponse{Error: &acp.UnstableMessageMcpError{
				Error: acp.UnstableMcpError{Code: int(wire.Code), Message: wire.Message},
			}}, nil
		}
		var result any
		if err := json.Unmarshal(response.Result, &result); err != nil {
			return acp.UnstableMessageMcpResponse{}, err
		}
		return acp.UnstableMessageMcpResponse{Result: &acp.UnstableMessageMcpResult{Result: result}}, nil
	case <-ctx.Done():
		// The ACP request was cancelled. The inner server is told so.
		cancelled, _ := json.Marshal(map[string]any{"requestId": id})
		_ = p.conn.Write(context.Background(), &jsonrpc.Request{Method: "notifications/cancelled", Params: cancelled})
		return acp.UnstableMessageMcpResponse{}, ctx.Err()
	}
}

func (p *acpProvider) count(method string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[method]
}

// acpClient is an ACP client that provides one MCP server and nothing else.
type acpClient struct{ provider *acpProvider }

func (acpClient) CompleteElicitation(context.Context, acp.CompleteElicitationNotification) error {
	return nil
}

func (acpClient) CreateElicitation(context.Context, acp.CreateElicitationRequest) (acp.CreateElicitationResponse, error) {
	return acp.CreateElicitationResponse{}, errNotOffered
}

func (acpClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, errNotOffered
}

func (acpClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, errNotOffered
}

func (acpClient) RequestPermission(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	return acp.RequestPermissionResponse{}, errNotOffered
}

func (acpClient) SessionUpdate(context.Context, acp.SessionNotification) error { return nil }

var errNotOffered = errors.New("not offered")

func (acpClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errNotOffered
}

func (acpClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errNotOffered
}

func (acpClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errNotOffered
}

func (acpClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errNotOffered
}

func (acpClient) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errNotOffered
}

func (acpClient) UnstableAuthStatusUpdate(context.Context, acp.UnstableAuthStatusUpdateNotification) error {
	return nil
}

func (c acpClient) UnstableMessageMcp(ctx context.Context, params acp.UnstableMessageMcpRequest) (acp.UnstableMessageMcpResponse, error) {
	return c.provider.UnstableMessageMcp(ctx, params)
}

// connectACP joins an agent-side and a client-side ACP connection with
// pipes and returns the agent's end, whose UnstableMessageMcp reaches the
// provider through the real wire format.
func connectACP(t *testing.T, provider *acpProvider) *acp.AgentSideConnection {
	t.Helper()
	toClient, fromAgent := io.Pipe()
	toAgent, fromClient := io.Pipe()
	agentEnd := acp.NewAgentSideConnection(nil, fromAgent, toAgent, slog.New(slog.DiscardHandler))
	clientEnd := acp.NewClientSideConnection(acpClient{provider: provider}, fromClient, toClient, slog.New(slog.DiscardHandler))
	go func() { _ = agentEnd.Serve(context.Background()) }()
	go func() { _ = clientEnd.Serve(context.Background()) }()
	t.Cleanup(func() {
		_ = fromAgent.Close()
		_ = fromClient.Close()
	})
	return agentEnd
}

// A server the ACP client provides is listed and called over the session's
// ACP connection, with no second process or endpoint and no initialize.
// A tool's error comes back as the tool's result, and an unknown tool as
// the server's refusal.
func TestACPTransportListsAndCalls(t *testing.T) {
	ctx := t.Context()
	provider := newACPProvider(t, newTestServer(), "tools-1")
	conn := connectACP(t, provider)
	m := newTestManager(t, Options{
		Servers: []Server{{Name: "client", Transport: TransportACP, ACPServerID: "tools-1"}},
		ACP:     conn.UnstableMessageMcp,
	})

	list, err := m.ListTools(ctx, "client")
	require.NoError(t, err)
	assert.Contains(t, toolNames(list), "echo")

	res, err := m.CallTool(ctx, "client", "echo", []byte(`{"text":"hello"}`))
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Equal(t, "hello", textOf(t, res))

	res, err = m.CallTool(ctx, "client", "fail", nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)

	_, err = m.CallTool(ctx, "client", "missing", nil)
	var callErr *CallError
	require.ErrorAs(t, err, &callErr)

	assert.Zero(t, provider.count("initialize"), "the binding has no initialize")
	assert.Positive(t, provider.count("tools/call"))
}

// A call whose context ends is cancelled on the ACP connection: the
// provider's handler sees its context end.
func TestACPTransportCancelsACall(t *testing.T) {
	server := newTestServer()
	started := make(chan struct{})
	stopped := make(chan struct{})
	sdk.AddTool(server, &sdk.Tool{Name: "hang", Description: "Wait until cancelled."},
		func(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			close(started)
			<-ctx.Done()
			close(stopped)
			return nil, nil, ctx.Err()
		})
	provider := newACPProvider(t, server, "tools-1")
	conn := connectACP(t, provider)
	m := newTestManager(t, Options{
		Servers: []Server{{Name: "client", Transport: TransportACP, ACPServerID: "tools-1"}},
		ACP:     conn.UnstableMessageMcp,
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := m.CallTool(ctx, "client", "hang", nil)
		done <- err
	}()
	<-started
	cancel()
	require.Error(t, <-done)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the provider's handler was not cancelled")
	}
}
