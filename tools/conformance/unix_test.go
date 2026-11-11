package conformance

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

// socketProcess serves stdio, unix and TCP on one actual binary. Stdio is
// /dev/null, so its EOF must not end the daemon. Addresses come from the
// process's structured startup logs, including the kernel-assigned TCP port.
type socketProcess struct {
	cmd      *exec.Cmd
	done     chan struct{}
	err      error
	stderr   *wire
	stdout   *wire
	unixPath string
	tcpAddr  string
}

// socketLog is written by exec's stderr copier. Letting exec own the pipe
// means Wait joins that copier before the exit error is reported.
type socketLog struct {
	wire      *wire
	addresses chan string
	pending   bytes.Buffer
}

func (l *socketLog) Write(p []byte) (int, error) {
	n, err := l.wire.Write(p)
	_, _ = l.pending.Write(p)
	for {
		end := bytes.IndexByte(l.pending.Bytes(), '\n')
		if end < 0 {
			return n, err
		}
		line := l.pending.Next(end + 1)
		var record struct {
			Network string `json:"network"`
			Address string `json:"address"`
		}
		if json.Unmarshal(line, &record) == nil && record.Network == "tcp" {
			l.addresses <- record.Address
		}
	}
}

func startSockets(t *testing.T) *socketProcess {
	t.Helper()
	dir := t.TempDir()
	p := &socketProcess{
		done:   make(chan struct{}),
		stderr: &wire{}, stdout: &wire{}, unixPath: filepath.Join(dir, "acp.sock"),
	}
	configFile := filepath.Join(dir, "config.toml")
	body := "[telemetry.logs.stderr]\nlevel = \"info\"\nformat = \"json\"\n" +
		"[listen]\nstdio = true\n[listen.unix]\npath = \"" + p.unixPath + "\"\n" +
		"[listen.tcp]\naddress = \"127.0.0.1:0\"\n"
	require.NoError(t, os.WriteFile(configFile, []byte(body), 0o600))
	p.cmd = exec.Command(binary, "--config", configFile, "serve")
	p.cmd.Env = []string{
		"XDG_CONFIG_HOME=" + filepath.Join(dir, "config"),
		"XDG_STATE_HOME=" + filepath.Join(dir, "state"), "HOME=" + dir,
	}
	p.cmd.Dir = dir
	p.cmd.Stdout = p.stdout
	addresses := make(chan string, 2)
	p.cmd.Stderr = &socketLog{wire: p.stderr, addresses: addresses}
	require.NoError(t, p.cmd.Start())
	go func() {
		p.err = p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		p.stop(t)
		require.Empty(t, p.stdout.lines(), "socket daemon wrote protocol data to stdout")
		if t.Failed() || os.Getenv(envWire) != "" {
			t.Logf("socket daemon stderr:\n%s", strings.Join(p.stderr.lines(), "\n"))
		}
	})
	select {
	case p.tcpAddr = <-addresses:
	case <-p.done:
		t.Fatalf("socket daemon exited before startup: %v\n%s", p.err, strings.Join(p.stderr.lines(), "\n"))
	case <-time.After(timeout):
		t.Fatal("socket daemon did not announce its bound TCP address")
	}
	return p
}

func (p *socketProcess) stop(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
	default:
		require.NoError(t, p.cmd.Process.Signal(syscall.SIGTERM))
	}
	select {
	case <-p.done:
		require.NoError(t, p.err, "stderr:\n%s", strings.Join(p.stderr.lines(), "\n"))
	case <-time.After(timeout):
		_ = p.cmd.Process.Kill()
		<-p.done
		t.Fatal("socket daemon did not stop after SIGTERM")
	}
}

func socketClient(t *testing.T, network, address string) (*agent, *acp.ClientSideConnection, *client, net.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
	require.NoError(t, err)
	a := &agent{t: t, sent: &wire{}, received: &wire{}, stdin: conn}
	a.stdout = io.TeeReader(conn, a.received)
	t.Cleanup(func() {
		_ = conn.Close()
		if t.Failed() || os.Getenv(envWire) != "" {
			t.Logf("%s client sent:\n%s", network, strings.Join(a.sent.lines(), "\n"))
			t.Logf("%s agent sent:\n%s", network, strings.Join(a.received.lines(), "\n"))
		}
	})
	connection, updates := a.connect()
	initialize(ctx, t, connection)
	return a, connection, updates, conn
}

func TestSocketClients(t *testing.T) {
	p := startSockets(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	oneWire, one, oneUpdates, oneSocket := socketClient(t, "unix", p.unixPath)
	twoWire, two, twoUpdates, _ := socketClient(t, "unix", p.unixPath)
	tcpWire, tcp, tcpUpdates, _ := socketClient(t, "tcp", p.tcpAddr)
	oneSession := newSession(ctx, t, one)
	twoSession := newSession(ctx, t, two)
	tcpSession := newSession(ctx, t, tcp)
	require.NotEqual(t, oneSession.SessionId, twoSession.SessionId)
	require.NotEqual(t, twoSession.SessionId, tcpSession.SessionId)

	// All three connections remain open. A session belongs to its owning
	// connection, even though all connections share the process host.
	_, err := two.Prompt(ctx, acp.PromptRequest{
		SessionId: oneSession.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock("not yours")},
	})
	require.Equal(t, -32002, requestError(t, err).Code)

	for _, c := range []struct {
		wire    *agent
		conn    *acp.ClientSideConnection
		updates *client
		session acp.SessionId
	}{
		{oneWire, one, oneUpdates, oneSession.SessionId},
		{twoWire, two, twoUpdates, twoSession.SessionId},
		{tcpWire, tcp, tcpUpdates, tcpSession.SessionId},
	} {
		response, err := c.conn.Prompt(ctx, acp.PromptRequest{
			SessionId: c.session, Prompt: []acp.ContentBlock{acp.TextBlock("socket conformance")},
		})
		require.NoError(t, err)
		require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
		require.Equal(t, []string{"available_commands_update", "agent_message_chunk"}, kinds(c.updates.take()))
		require.Empty(t, notDecoded(t, c.wire.lastResult(), response), "raw result: %s", c.wire.lastResult())
	}

	require.NoError(t, oneSocket.Close())
	// One client's EOF does not retire the other connection or its session.
	response, err := tcp.Prompt(ctx, acp.PromptRequest{
		SessionId: tcpSession.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock("still connected")},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
	require.Equal(t, []string{"agent_message_chunk"}, kinds(tcpUpdates.take()))

	// Shutdown is exercised with the other two clients still connected.
	p.stop(t)
	for _, conn := range []*acp.ClientSideConnection{two, tcp} {
		select {
		case <-conn.Done():
		case <-ctx.Done():
			t.Fatal("socket client did not observe process shutdown")
		}
	}
	_, err = os.Lstat(p.unixPath)
	require.ErrorIs(t, err, os.ErrNotExist, "unix socket file survived shutdown")
}
