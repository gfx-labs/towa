package mcp

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/config"
)

// newTestPool returns a pool that is closed when the test ends.
func newTestPool(t *testing.T, servers []Server) *Pool {
	t.Helper()
	p, err := NewPool(PoolOptions{
		Config: defaultConfig(t), Servers: servers, HTTPClient: newCountingClient().Client,
		ClientName: "acpharness-test", ClientVersion: "0",
	})
	require.NoError(t, err)
	p.m.terminateGrace = 200 * time.Millisecond
	t.Cleanup(func() { require.NoError(t, p.Close()) })
	return p
}

func TestSharedServerHasOneConnectionForEverySession(t *testing.T) {
	ctx := t.Context()
	shared := stdioServer(t, "shared", modeServe)
	shared.Shared = true
	own := stdioServer(t, "own", modeServe)
	servers := []Server{shared, own}
	pool := newTestPool(t, servers)

	a := newTestManager(t, Options{Servers: servers, Pool: pool})
	b := newTestManager(t, Options{Servers: servers, Pool: pool})
	sharedPids := callPids(t, a, "shared")
	require.Equal(t, sharedPids.PID, callPids(t, b, "shared").PID, "both sessions reach one process")
	ownPids := callPids(t, a, "own")
	require.NotEqual(t, ownPids.PID, callPids(t, b, "own").PID, "a server that is not shared runs once per session")

	// One session ends while the other is in a call on the shared server.
	c, _, err := b.acquire(ctx, "shared")
	require.NoError(t, err)
	require.NoError(t, a.Close())
	require.False(t, alive(ownPids.PID), "the session's own server ended with it")
	require.True(t, alive(sharedPids.PID))
	c.release()
	require.Equal(t, sharedPids.PID, callPids(t, b, "shared").PID)
	_, err = a.ListTools(ctx, "shared")
	require.ErrorIs(t, err, ErrClosed, "the ended session's manager stays closed")

	// Idle time is counted over every session, and the next use starts
	// the server again.
	pool.m.idleClose = 100 * time.Millisecond
	callPids(t, b, "shared")
	require.Eventually(t, func() bool { return !b.Connected("shared") && !alive(sharedPids.PID) }, 10*time.Second, 10*time.Millisecond)
	pool.m.idleClose = time.Hour
	again := callPids(t, b, "shared")
	require.NotEqual(t, sharedPids.PID, again.PID)

	// The pool closing ends the server for every session, for good.
	require.NoError(t, pool.Close())
	require.False(t, alive(again.PID))
	_, err = b.CallTool(ctx, "shared", "echo", jsontext.Value(`{"text":"x"}`))
	require.ErrorIs(t, err, ErrClosed)
}

func TestClosingASessionLeavesAnotherSessionsSharedCallRunning(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	server := newTestServer()
	sdk.AddTool(server, &sdk.Tool{Name: "hold", Description: "Wait until released."},
		func(ctx context.Context, _ *sdk.CallToolRequest, _ noInput) (*sdk.CallToolResult, any, error) {
			close(started)
			select {
			case <-finish:
				return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "finished"}}}, nil, nil
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(func() { ts.CloseClientConnections(); ts.Close() })
	shared := Server{Name: "s", Transport: config.MCPTransportStreamableHTTP, URL: ts.URL, Shared: true}
	pool := newTestPool(t, []Server{shared})
	a := newTestManager(t, Options{Servers: []Server{shared}, Pool: pool, HTTPClient: newCountingClient().Client})
	b := newTestManager(t, Options{Servers: []Server{shared}, Pool: pool, HTTPClient: newCountingClient().Client})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		res, err := b.CallTool(ctx, "s", "hold", nil)
		if err == nil && (res.IsError || textOf(t, res) != "finished") {
			err = fmt.Errorf("unexpected result: %+v", res)
		}
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.NoError(t, a.Close())
	close(finish)
	require.NoError(t, <-done)
}

func TestSharedServerThatDiesIsStartedAgain(t *testing.T) {
	shared := stdioServer(t, "shared", modeServe)
	shared.Shared = true
	pool := newTestPool(t, []Server{shared})
	m := newTestManager(t, Options{Servers: []Server{shared}, Pool: pool})
	first := callPids(t, m, "shared")
	require.NoError(t, syscall.Kill(first.PID, syscall.SIGKILL))
	require.Eventually(t, func() bool { return !m.Connected("shared") }, 10*time.Second, 10*time.Millisecond)
	require.NotEqual(t, first.PID, callPids(t, m, "shared").PID)
}

func TestSharedServerNeedsItsPool(t *testing.T) {
	shared := stdioServer(t, "shared", modeServe)
	shared.Shared = true
	opts := Options{Config: defaultConfig(t), Servers: []Server{shared}, ClientName: "t", ClientVersion: "0"}

	_, err := NewManager(opts)
	require.Error(t, err, "no pool")

	opts.Pool = newTestPool(t, nil)
	_, err = NewManager(opts)
	require.Error(t, err, "a pool without the server")

	// The same name with another definition is not the pool's server.
	opts.Pool = newTestPool(t, []Server{shared})
	other := shared
	other.Args = []string{"--other"}
	opts.Servers = []Server{other}
	_, err = NewManager(opts)
	require.Error(t, err, "a different definition")

	// A session's server of the same name replaces the configured one and
	// is not shared, so it gets its own connection.
	cfg := defaultConfig(t)
	cfg.Servers = []config.MCPServer{{Name: "shared", Transport: config.MCPTransportStdio, Command: shared.Command, Shared: true}}
	merged, err := Servers(cfg, []acp.McpServer{{Stdio: &acp.McpServerStdio{Name: "shared", Command: shared.Command}}}, noEnv)
	require.NoError(t, err)
	require.Len(t, merged, 1)
	require.True(t, merged[0].FromSession)
	require.False(t, merged[0].Shared)
}
