package cli

import (
	"context"
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/config"
)

// A client that stops reading must not keep process shutdown waiting for a
// response write. Only the explicitly handed-off stream is closed.
func TestOwnedStdioCancellationUnblocksResponse(t *testing.T) {
	host := transportHost(t)
	server, client := net.Pipe()
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serveListeners(ctx, config.Listen{Stdio: true}, host,
			Host{Stdin: server, Stdout: server}, nil, server.Close)
	}()
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(client, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":1}}\n")
		written <- err
	}()
	require.NoError(t, awaitTransport(t, written))
	// There is deliberately no reader for the response.
	cancel()
	require.NoError(t, awaitTransport(t, done))
}
