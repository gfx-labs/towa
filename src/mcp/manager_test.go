package mcp

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"syscall"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/config"
)

func textOf(t *testing.T, res *sdk.CallToolResult) string {
	t.Helper()
	require.NotEmpty(t, res.Content)
	text, ok := res.Content[0].(*sdk.TextContent)
	require.True(t, ok, "content is %T", res.Content[0])
	return text.Text
}

func toolNames(list []*sdk.Tool) []string {
	names := make([]string, len(list))
	for i, t := range list {
		names[i] = t.Name
	}
	return names
}

// alive reports whether a process exists. A zombie counts as gone: it runs
// nothing, and its parent in the test is the server that was killed.
func alive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	stat, err := readProcState(pid)
	return err != nil || stat != 'Z'
}

func TestEveryTransportListsAndCalls(t *testing.T) {
	want := []string{"echo", "fail", "picture", "pids", "submit_result"}
	for _, transport := range []string{config.MCPTransportStdio, config.MCPTransportStreamableHTTP, config.MCPTransportSSE} {
		t.Run(transport, func(t *testing.T) {
			ctx := t.Context()
			var server Server
			var remote *httpServer
			client := newCountingClient()
			if transport == config.MCPTransportStdio {
				server = stdioServer(t, "s", modeServe)
			} else {
				remote = startHTTPServer(t, transport, nil)
				server = Server{Name: "s", Transport: transport, URL: remote.URL, Headers: map[string]string{"X-Test-Key": "k1"}}
			}
			m := newTestManager(t, Options{Servers: []Server{server}, HTTPClient: client.Client})

			require.False(t, m.Connected("s"), "nothing starts before the first use")
			list, err := m.ListTools(ctx, "s")
			require.NoError(t, err)
			require.ElementsMatch(t, want, toolNames(list))
			require.True(t, m.Connected("s"))

			res, err := m.CallTool(ctx, "s", "echo", jsontext.Value(`{"text":"héllo"}`))
			require.NoError(t, err)
			require.False(t, res.IsError)
			require.Equal(t, "héllo", textOf(t, res))

			res, err = m.CallTool(ctx, "s", "fail", nil)
			require.NoError(t, err)
			require.True(t, res.IsError)
			require.Contains(t, textOf(t, res), "the disk is full")

			_, err = m.CallTool(ctx, "s", "no_such_tool", nil)
			var callErr *CallError
			require.ErrorAs(t, err, &callErr)

			_, err = m.ListTools(ctx, "other")
			var unknown *UnknownServerError
			require.ErrorAs(t, err, &unknown)

			require.NoError(t, m.Close())
			require.False(t, m.Connected("s"))
			_, err = m.ListTools(ctx, "s")
			require.ErrorIs(t, err, ErrClosed)

			if remote != nil {
				// Every request the server got came through the client the
				// manager was given, and carried the server's header.
				require.Positive(t, remote.requests.Load())
				require.Equal(t, remote.requests.Load(), client.requests.Load())
				require.Equal(t, "k1", remote.lastKey.Load())
			}
		})
	}
}

func TestRemoteServerNeedsAClient(t *testing.T) {
	remote := Server{Name: "s", Transport: config.MCPTransportSSE, URL: "http://127.0.0.1:1/"}
	_, err := NewManager(Options{
		Config: defaultConfig(t), Servers: []Server{remote}, ClientName: "towa-test", ClientVersion: "0",
	})
	require.Error(t, err)

	// A client with no transport would use net/http's default one.
	m := newTestManager(t, Options{Servers: []Server{remote}, HTTPClient: &http.Client{}})
	_, err = m.ListTools(t.Context(), "s")
	require.ErrorIs(t, err, errDefaultTransport)
}

func TestIdleConnectionClosesAndReopens(t *testing.T) {
	for _, transport := range []string{config.MCPTransportStdio, config.MCPTransportStreamableHTTP, config.MCPTransportSSE} {
		t.Run(transport, func(t *testing.T) {
			ctx := t.Context()
			var server Server
			if transport == config.MCPTransportStdio {
				server = stdioServer(t, "s", modeServe)
			} else {
				server = Server{Name: "s", Transport: transport, URL: startHTTPServer(t, transport, nil).URL}
			}
			m := newTestManager(t, Options{Servers: []Server{server}, HTTPClient: newCountingClient().Client})
			m.idleClose = 100 * time.Millisecond

			var first pidsOutput
			if transport == config.MCPTransportStdio {
				first = callPids(t, m, "s")
			} else {
				_, err := m.ListTools(ctx, "s")
				require.NoError(t, err)
			}
			require.True(t, m.Connected("s"))
			require.Eventually(t, func() bool { return !m.Connected("s") }, 10*time.Second, 10*time.Millisecond)

			res, err := m.CallTool(ctx, "s", "echo", jsontext.Value(`{"text":"again"}`))
			require.NoError(t, err)
			require.Equal(t, "again", textOf(t, res))

			if transport == config.MCPTransportStdio {
				// The idle close ended the first process and its child, and
				// the next use started another.
				require.Eventually(t, func() bool { return !alive(first.PID) && !alive(first.Child) }, 10*time.Second, 10*time.Millisecond)
				require.NotEqual(t, first.PID, callPids(t, m, "s").PID)
			}
		})
	}
}

func TestCallInFlightHoldsTheConnectionOpen(t *testing.T) {
	m := newTestManager(t, Options{Servers: []Server{stdioServer(t, "s", modeServe)}})
	m.idleClose = 50 * time.Millisecond
	c, _, err := m.acquire(t.Context(), "s")
	require.NoError(t, err)
	// A use that started and ended meanwhile must not arm a close under
	// the one still running.
	_, err = m.ListTools(t.Context(), "s")
	require.NoError(t, err)
	time.Sleep(4 * m.idleClose)
	require.True(t, m.Connected("s"))
	c.release()
	require.Eventually(t, func() bool { return !m.Connected("s") }, 10*time.Second, 10*time.Millisecond)
}

func callPids(t *testing.T, m *Manager, server string) pidsOutput {
	t.Helper()
	res, err := m.CallTool(t.Context(), server, "pids", nil)
	require.NoError(t, err)
	require.False(t, res.IsError, "%v", res.Content)
	var out pidsOutput
	require.NoError(t, json.Unmarshal([]byte(textOf(t, res)), &out))
	require.True(t, alive(out.PID))
	require.True(t, alive(out.Child))
	return out
}

func TestCloseKillsTheStdioProcessGroup(t *testing.T) {
	// serve exits when its stdin closes and leaves its child. linger does
	// not exit, and is signalled after the grace period.
	for _, mode := range []string{modeServe, modeLinger} {
		t.Run(mode, func(t *testing.T) {
			m := newTestManager(t, Options{Servers: []Server{stdioServer(t, "s", mode)}})
			pids := callPids(t, m, "s")
			require.NoError(t, m.Close())
			// Close has reaped the server itself. The child is reaped by
			// whoever inherits it.
			assert.False(t, alive(pids.PID), "server %d is still running", pids.PID)
			require.Eventually(t, func() bool { return !alive(pids.Child) }, 10*time.Second, 10*time.Millisecond,
				"child %d is still running", pids.Child)
		})
	}
}

// A process the server started outside its group is not signalled, and it
// can hold the server's stdout or stderr for as long as it runs.
func TestCloseDoesNotWaitForAProcessOutsideTheGroup(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			m := newTestManager(t, Options{Servers: []Server{stdioServer(t, "s", modeDetach)}})
			res, err := m.CallTool(t.Context(), "s", "detach", jsontext.Value(`{"stream":"`+stream+`"}`))
			require.NoError(t, err)
			require.False(t, res.IsError, "%v", res.Content)
			var out pidsOutput
			require.NoError(t, json.Unmarshal([]byte(textOf(t, res)), &out))
			t.Cleanup(func() { _ = syscall.Kill(out.Child, syscall.SIGKILL) })

			closed := make(chan error, 1)
			go func() { closed <- m.Close() }()
			select {
			case err := <-closed:
				require.NoError(t, err)
			case <-time.After(10 * time.Second):
				t.Fatal("Close is waiting for the process that holds the pipe")
			}
			assert.False(t, alive(out.PID), "server %d is still running", out.PID)
			assert.True(t, alive(out.Child), "child %d left the group and should not have been signalled", out.Child)
		})
	}
}

func TestServerThatDiesIsStartedAgain(t *testing.T) {
	m := newTestManager(t, Options{Servers: []Server{stdioServer(t, "s", modeServe)}})
	first := callPids(t, m, "s")
	require.NoError(t, syscall.Kill(first.PID, syscall.SIGKILL))
	require.Eventually(t, func() bool { return !m.Connected("s") }, 10*time.Second, 10*time.Millisecond)
	second := callPids(t, m, "s")
	require.NotEqual(t, first.PID, second.PID)
	require.Eventually(t, func() bool { return !alive(first.Child) }, 10*time.Second, 10*time.Millisecond)
}

func TestConnectTimeout(t *testing.T) {
	m := newTestManager(t, Options{Servers: []Server{stdioServer(t, "s", modeMute)}})
	m.connectTimeout = 300 * time.Millisecond
	start := time.Now()
	_, err := m.ListTools(t.Context(), "s")
	require.Error(t, err)
	require.Less(t, time.Since(start), 5*time.Second)
	require.False(t, m.Connected("s"))

	// The caller's own cancellation ends a connect as well.
	m.connectTimeout = time.Minute
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	_, err = m.ListTools(ctx, "s")
	require.Error(t, err)
	require.True(t, errors.Is(ctx.Err(), context.DeadlineExceeded))
}
