package conformance

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

// Until the CLI wires its socket listeners, bridge a real unix socket to
// the binary's stdio. This checks the SDK over the socket, not src/listen or
// the CLI's per-connection session ownership.
func TestUnixSocketClient(t *testing.T) {
	a := start(t)
	path := filepath.Join(t.TempDir(), "acp.sock")
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		copied := make(chan struct{})
		go func() {
			_, _ = io.Copy(a.stdin, conn)
			_ = a.stdin.Close()
			close(copied)
		}()
		_, _ = io.Copy(conn, a.stdout)
		_ = conn.Close()
		<-copied
	}()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = conn.Close()
		<-done
	})
	socket := &agent{t: t, sent: &wire{}, received: &wire{}, stdin: conn}
	socket.stdout = io.TeeReader(conn, socket.received)
	client, updates := socket.connect()
	initialize(ctx, t, client)
	session := newSession(ctx, t, client)
	resp, err := client.Prompt(ctx, acp.PromptRequest{
		SessionId: session.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("unix socket conformance")},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Equal(t, []string{"available_commands_update", "agent_message_chunk"}, kinds(updates.take()))
	require.Empty(t, notDecoded(t, socket.lastResult(), resp), "raw result: %s", socket.lastResult())
}
