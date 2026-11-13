package cli

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/agent/session"
	"github.com/gfx-labs/towa/src/config"
)

const transportTestTimeout = 10 * time.Second

// transportLog captures actual bound addresses and stdio errors without
// polling files or reserving a TCP port before the server binds it.
type transportLog struct {
	addresses chan net.Addr
	warnings  chan string
}

func (*transportLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *transportLog) Handle(_ context.Context, record slog.Record) error {
	var network, address string
	record.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "network":
			network = a.Value.String()
		case "address":
			address = a.Value.String()
		}
		return true
	})
	if address != "" {
		l.addresses <- &net.UnixAddr{Net: network, Name: address}
	}
	if record.Level >= slog.LevelWarn {
		l.warnings <- record.Message
	}
	return nil
}
func (l *transportLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *transportLog) WithGroup(string) slog.Handler      { return l }

func transportHost(t *testing.T) *session.Host {
	t.Helper()
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	host, err := session.NewHost(session.Options{
		Version: "test", Turn: loop.Stub, Session: loaded.Config.Session,
		Logger: slog.New(slog.DiscardHandler),
		Tracer: tracenoop.NewTracerProvider().Tracer("test"),
		Meter:  metricnoop.NewMeterProvider().Meter("test"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, host.Close(context.Background())) })
	return host
}

func awaitTransport[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(transportTestTimeout):
		t.Fatal("transport did not reach the expected state")
		var zero T
		return zero
	}
}

func initializeTransport(t *testing.T, conn net.Conn) {
	t.Helper()
	require.NoError(t, conn.SetDeadline(time.Now().Add(transportTestTimeout)))
	_, err := io.WriteString(conn, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":1}}\n")
	require.NoError(t, err)
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	require.NoError(t, err)
	var response struct {
		Result struct {
			ProtocolVersion int `json:"protocolVersion"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(line, &response))
	require.Equal(t, 1, response.Result.ProtocolVersion)
	require.NoError(t, conn.SetDeadline(time.Time{}))
}

func TestServeListenersRunsStdioUnixAndTCP(t *testing.T) {
	host := transportHost(t)
	stdioServer, stdioClient := net.Pipe()
	defer func() { _ = stdioClient.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &transportLog{addresses: make(chan net.Addr, 2), warnings: make(chan string, 4)}
	cfg := config.Listen{
		Stdio: true,
		Unix:  config.ListenUnix{Path: filepath.Join(t.TempDir(), "acp.sock"), Mode: "0600"},
		TCP:   config.ListenTCP{Address: "127.0.0.1:0"},
	}
	var closeCalls atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- serveListeners(ctx, cfg, host, Host{Stdin: stdioServer, Stdout: stdioServer}, slog.New(logs), func() error {
			closeCalls.Add(1)
			return stdioServer.Close()
		})
	}()
	initializeTransport(t, stdioClient)
	var clients []net.Conn
	for range 2 {
		addr := awaitTransport(t, logs.addresses)
		conn, err := net.DialTimeout(addr.Network(), addr.String(), transportTestTimeout)
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()
		clients = append(clients, conn)
		initializeTransport(t, conn)
	}

	cancel()
	require.NoError(t, awaitTransport(t, done))
	require.EqualValues(t, 1, closeCalls.Load())
	// The helper has joined every transport before the caller closes host.
	for _, conn := range append(clients, stdioClient) {
		_ = conn.SetReadDeadline(time.Now().Add(transportTestTimeout))
		var b [1]byte
		_, err := conn.Read(b[:])
		require.Error(t, err)
		var timeout net.Error
		require.False(t, errors.As(err, &timeout) && timeout.Timeout(), "connection was not closed")
	}
}

type borrowedInput struct {
	io.Reader
	closed atomic.Bool
}

func (r *borrowedInput) Close() error { r.closed.Store(true); return nil }

func TestDaemonStdioOwnership(t *testing.T) {
	for _, input := range []string{"", `{"jsonrpc":`} {
		t.Run(input, func(t *testing.T) {
			host := transportHost(t)
			reader := &borrowedInput{Reader: strings.NewReader(input)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			logs := &transportLog{addresses: make(chan net.Addr, 1), warnings: make(chan string, 4)}
			cfg := config.Listen{Stdio: true, Unix: config.ListenUnix{Path: filepath.Join(t.TempDir(), "acp.sock"), Mode: "0600"}}
			done := make(chan error, 1)
			go func() {
				done <- serveListeners(ctx, cfg, host, Host{Stdin: reader, Stdout: io.Discard}, slog.New(logs), nil)
			}()
			addr := awaitTransport(t, logs.addresses)
			if input != "" {
				require.Equal(t, "stdio connection ended with an error", awaitTransport(t, logs.warnings))
			}
			conn, err := net.DialTimeout(addr.Network(), addr.String(), transportTestTimeout)
			require.NoError(t, err)
			defer func() { _ = conn.Close() }()
			initializeTransport(t, conn)
			select {
			case err := <-done:
				t.Fatalf("daemon ended after stdio ended: %v", err)
			default:
			}
			cancel()
			require.NoError(t, awaitTransport(t, done))
			require.False(t, reader.closed.Load(), "borrowed input was closed without ownership handoff")
		})
	}
}

func TestStdioOnlyReturnsItsResult(t *testing.T) {
	err := serveListeners(context.Background(), config.Listen{Stdio: true}, transportHost(t),
		Host{Stdin: strings.NewReader(`{"jsonrpc":`), Stdout: io.Discard}, nil, nil)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}
