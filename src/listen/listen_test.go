package listen_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/listen"
)

// timeout bounds waits in these tests so a broken connection cannot hang them.
const timeout = 30 * time.Second

// echo answers a request with its own params. The params type is left to
// the compiler, so this file does not name the JSON package pkg/acp uses.
func echo[P any](_ context.Context, _ string, params P) (any, *acp.RequestError) {
	return params, nil
}

// server is one running listen.Serve.
type server struct {
	t         *testing.T
	listeners *listen.Listeners
	cancel    context.CancelFunc
	served    chan error
	stopOnce  sync.Once
	stopErr   error

	started atomic.Int64
	mu      sync.Mutex
	ended   []error
}

// serve opens cfg's listeners and serves each connection with an ACP
// connection that echoes, which is how the agent itself reads a socket.
func serve(t *testing.T, cfg config.Listen) *server {
	t.Helper()
	return serveWith(t, cfg, func(ctx context.Context, conn net.Conn) error {
		return acp.NewConnection(echo, conn, conn, nil).Serve(ctx)
	})
}

func serveWith(t *testing.T, cfg config.Listen, handle listen.Handler) *server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	listeners, err := listen.Open(ctx, cfg, nil)
	if err != nil {
		cancel()
		require.NoError(t, err)
	}
	s := &server{t: t, listeners: listeners, cancel: cancel, served: make(chan error, 1)}
	go func() {
		s.served <- listeners.Serve(ctx, func(ctx context.Context, conn net.Conn) error {
			s.started.Add(1)
			err := handle(ctx, conn)
			s.mu.Lock()
			defer s.mu.Unlock()
			s.ended = append(s.ended, err)
			return err
		})
	}()
	t.Cleanup(func() {
		if err := s.stop(); err != nil {
			t.Errorf("Serve returned %v", err)
		}
	})
	return s
}

// stop shuts the server down and returns what Serve returned.
func (s *server) stop() error {
	s.stopOnce.Do(func() {
		s.cancel()
		select {
		case s.stopErr = <-s.served:
		case <-time.After(timeout):
			s.stopErr = errors.New("Serve did not return after its context was cancelled")
		}
	})
	return s.stopErr
}

func (s *server) endings() []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]error(nil), s.ended...)
}

// client is an ACP connection over a dialled socket.
type client struct {
	t    *testing.T
	conn net.Conn
	acp  *acp.Connection
}

func newClient(t *testing.T, conn net.Conn) *client {
	t.Helper()
	c := &client{t: t, conn: conn, acp: acp.NewConnection(echo, conn, conn, nil)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.acp.Serve(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		_ = conn.Close()
		<-done
	})
	return c
}

// ask sends text and returns what came back.
func (c *client) ask(text string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	got, err := acp.SendRequest[map[string]string](ctx, c.acp, "echo", map[string]string{"text": text})
	return got["text"], err
}

func dialUnix(t *testing.T, path string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("unix", path, timeout)
	require.NoError(t, err)
	return conn
}

func unixConfig(t *testing.T) config.Listen {
	t.Helper()
	return config.Listen{Unix: config.ListenUnix{Path: filepath.Join(t.TempDir(), "acp.sock"), Mode: "0600"}}
}

// authority is a certificate authority made for one test.
type authority struct {
	t    *testing.T
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	// file holds the authority's certificate in PEM.
	file   string
	serial int64
}

func newAuthority(t *testing.T, name string) *authority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	a := &authority{t: t, cert: cert, key: key, file: filepath.Join(t.TempDir(), "ca.pem"), serial: 1}
	require.NoError(t, os.WriteFile(a.file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	return a
}

// issue writes a certificate for localhost and 127.0.0.1 and its key to
// certFile and keyFile, replacing what is there.
func (a *authority) issue(usage x509.ExtKeyUsage, certFile, keyFile string) {
	a.t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(a.t, err)
	a.serial++
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(a.serial),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}, a.cert, &key.PublicKey, a.key)
	require.NoError(a.t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(a.t, err)
	require.NoError(a.t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(a.t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600))
}

// trust is a client TLS config that trusts a and presents the certificates
// given.
func (a *authority) trust(certs ...tls.Certificate) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(a.cert)
	return &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS13, Certificates: certs}
}

// askTLS dials, completes the handshake and sends one request. With TLS 1.3
// a server that refuses the client's certificate says so only after the
// client's handshake has returned, so a refusal can surface at either step.
func askTLS(t *testing.T, network, addr string, cfg *tls.Config, text string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := (&tls.Dialer{Config: cfg}).DialContext(ctx, network, addr)
	if err != nil {
		return "", err
	}
	return newClient(t, conn).ask(text)
}

func TestTwoClientsShareOneUnixSocket(t *testing.T) {
	cfg := unixConfig(t)
	cfg.Unix.Mode = "0660"
	s := serve(t, cfg)

	info, err := os.Lstat(cfg.Unix.Path)
	require.NoError(t, err)
	require.Equal(t, fs.ModeSocket|0o660, info.Mode())

	// Both are connected before either asks, and they ask in turn, so each
	// connection is shown to be served while the other is open.
	one, two := newClient(t, dialUnix(t, cfg.Unix.Path)), newClient(t, dialUnix(t, cfg.Unix.Path))
	for _, round := range []string{"first", "second"} {
		got, err := one.ask("one " + round)
		require.NoError(t, err)
		require.Equal(t, "one "+round, got)
		got, err = two.ask("two " + round)
		require.NoError(t, err)
		require.Equal(t, "two "+round, got)
	}
	require.EqualValues(t, 2, s.started.Load())
}

func TestTCPWithTLS(t *testing.T) {
	ca := newAuthority(t, "listen test CA")
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	ca.issue(x509.ExtKeyUsageServerAuth, certFile, keyFile)
	s := serve(t, config.Listen{
		HandshakeTimeoutSeconds: 10,
		TCP: config.ListenTCP{
			Address: "127.0.0.1:0",
			TLS:     config.ListenTLS{CertFile: certFile, KeyFile: keyFile},
		},
	})
	addr := s.listeners.Addrs()[0].String()

	got, err := askTLS(t, "tcp", addr, ca.trust(), "over tls")
	require.NoError(t, err)
	require.Equal(t, "over tls", got)

	// The listener speaks only TLS: a client that sends JSON gets no answer
	// and no handler.
	plain, err := net.DialTimeout("tcp", addr, timeout)
	require.NoError(t, err)
	_, err = newClient(t, plain).ask("in the clear")
	require.Error(t, err)

	// A client that trusts another authority refuses the server.
	_, err = askTLS(t, "tcp", addr, newAuthority(t, "another CA").trust(), "wrong authority")
	require.Error(t, err)
	require.EqualValues(t, 1, s.started.Load())
}

func TestClientCertificate(t *testing.T) {
	ca := newAuthority(t, "listen test CA")
	other := newAuthority(t, "another CA")
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	ca.issue(x509.ExtKeyUsageServerAuth, certFile, keyFile)

	pair := func(a *authority, name string) tls.Certificate {
		c, k := filepath.Join(dir, name+".pem"), filepath.Join(dir, name+"-key.pem")
		a.issue(x509.ExtKeyUsageClientAuth, c, k)
		loaded, err := tls.LoadX509KeyPair(c, k)
		require.NoError(t, err)
		return loaded
	}
	good, foreign := pair(ca, "good"), pair(other, "foreign")

	tests := []struct {
		name     string
		required bool
		certs    []tls.Certificate
		accepted bool
	}{
		{"required and presented", true, []tls.Certificate{good}, true},
		{"required and absent", true, nil, false},
		{"required and signed by another authority", true, []tls.Certificate{foreign}, false},
		{"optional and absent", false, nil, true},
		{"optional and presented", false, []tls.Certificate{good}, true},
		{"optional and signed by another authority", false, []tls.Certificate{foreign}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := unixConfig(t)
			cfg.HandshakeTimeoutSeconds = 10
			cfg.Unix.TLS = config.ListenTLS{
				CertFile: certFile, KeyFile: keyFile,
				ClientCAFile: ca.file, RequireClientCert: tt.required,
			}
			s := serve(t, cfg)
			clientTLS := ca.trust(tt.certs...)
			if len(tt.certs) > 0 {
				// Send the chosen certificate even when its issuer is absent
				// from the server's list, rather than sending none.
				clientTLS.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
					return &tt.certs[0], nil
				}
			}
			got, err := askTLS(t, "unix", cfg.Unix.Path, clientTLS, "hello")
			if !tt.accepted {
				require.Error(t, err)
				require.Zero(t, s.started.Load(), "a refused client reached the handler")
				return
			}
			require.NoError(t, err)
			require.Equal(t, "hello", got)
		})
	}
}

// The files are read again for each handshake, so a certificate replaced on
// disk serves the next connection.
func TestARotatedCertificateServesTheNextConnection(t *testing.T) {
	before, after := newAuthority(t, "CA before"), newAuthority(t, "CA after")
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	before.issue(x509.ExtKeyUsageServerAuth, certFile, keyFile)
	s := serve(t, config.Listen{
		TCP: config.ListenTCP{Address: "127.0.0.1:0", TLS: config.ListenTLS{CertFile: certFile, KeyFile: keyFile}},
	})
	addr := s.listeners.Addrs()[0].String()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := (&tls.Dialer{Config: before.trust()}).DialContext(ctx, "tcp", addr)
	require.NoError(t, err)
	old := newClient(t, conn)
	_, err = old.ask("before")
	require.NoError(t, err)

	after.issue(x509.ExtKeyUsageServerAuth, certFile, keyFile)

	got, err := askTLS(t, "tcp", addr, after.trust(), "after")
	require.NoError(t, err)
	require.Equal(t, "after", got)
	_, err = askTLS(t, "tcp", addr, before.trust(), "old authority")
	require.Error(t, err)
	// The connection made before the change goes on.
	got, err = old.ask("still here")
	require.NoError(t, err)
	require.Equal(t, "still here", got)
}

func TestACertificateThatCannotBeReadFailsAtOpen(t *testing.T) {
	cfg := unixConfig(t)
	cfg.Unix.TLS = config.ListenTLS{CertFile: filepath.Join(t.TempDir(), "absent.pem"), KeyFile: filepath.Join(t.TempDir(), "absent-key.pem")}
	_, err := listen.Open(context.Background(), cfg, nil)
	require.ErrorIs(t, err, fs.ErrNotExist)
	_, err = os.Lstat(cfg.Unix.Path)
	require.ErrorIs(t, err, fs.ErrNotExist, "a socket file was left behind")
}

func TestAStaleSocketIsReplaced(t *testing.T) {
	cfg := unixConfig(t)
	// What a killed process leaves: a bound socket file nobody listens on.
	dead, err := net.Listen("unix", cfg.Unix.Path)
	require.NoError(t, err)
	dead.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, dead.Close())
	_, err = os.Lstat(cfg.Unix.Path)
	require.NoError(t, err)

	serve(t, cfg)
	got, err := newClient(t, dialUnix(t, cfg.Unix.Path)).ask("after the stale file")
	require.NoError(t, err)
	require.Equal(t, "after the stale file", got)
}

func TestALiveSocketIsNotReplaced(t *testing.T) {
	t.Run("served by this package", func(t *testing.T) {
		cfg := unixConfig(t)
		serve(t, cfg)
		_, err := listen.Open(context.Background(), cfg, nil)
		require.ErrorIs(t, err, listen.ErrInUse)

		got, err := newClient(t, dialUnix(t, cfg.Unix.Path)).ask("still served")
		require.NoError(t, err)
		require.Equal(t, "still served", got)
	})

	// A listener that holds no lock file is found by connecting to it.
	t.Run("served by another program", func(t *testing.T) {
		cfg := unixConfig(t)
		live, err := net.Listen("unix", cfg.Unix.Path)
		require.NoError(t, err)
		defer func() { _ = live.Close() }()

		_, err = listen.Open(context.Background(), cfg, nil)
		require.ErrorIs(t, err, listen.ErrInUse)

		accepted := make(chan error, 1)
		go func() {
			conn, err := live.Accept()
			if err == nil {
				err = conn.Close()
			}
			accepted <- err
		}()
		conn := dialUnix(t, cfg.Unix.Path)
		require.NoError(t, <-accepted)
		require.NoError(t, conn.Close())
	})

	t.Run("a file that is not a socket", func(t *testing.T) {
		cfg := unixConfig(t)
		require.NoError(t, os.WriteFile(cfg.Unix.Path, []byte("keep me"), 0o600))
		_, err := listen.Open(context.Background(), cfg, nil)
		require.Error(t, err)
		kept, err := os.ReadFile(cfg.Unix.Path)
		require.NoError(t, err)
		require.Equal(t, "keep me", string(kept))
	})
}

func TestShutdownWithAConnectionOpen(t *testing.T) {
	cfg := unixConfig(t)
	s := serve(t, cfg)
	c := newClient(t, dialUnix(t, cfg.Unix.Path))
	_, err := c.ask("hello")
	require.NoError(t, err)

	require.NoError(t, s.stop())

	// The handler was cancelled and has returned, and the client was hung
	// up on.
	require.Len(t, s.endings(), 1)
	require.True(t, errors.Is(s.endings()[0], context.Canceled) || errors.Is(s.endings()[0], net.ErrClosed))
	select {
	case <-c.acp.Done():
	case <-time.After(timeout):
		t.Fatal("the client's connection was not closed")
	}
	_, err = os.Lstat(cfg.Unix.Path)
	require.ErrorIs(t, err, fs.ErrNotExist, "the socket file was not removed")
}

// Closing the sockets must unblock a handler that is blocked in a read.
func TestShutdownUnblocksAConnectionRead(t *testing.T) {
	cfg := unixConfig(t)
	s := serveWith(t, cfg, func(_ context.Context, conn net.Conn) error {
		_, err := io.Copy(io.Discard, conn)
		return err
	})
	conn := dialUnix(t, cfg.Unix.Path)
	defer func() { _ = conn.Close() }()
	require.Eventually(t, func() bool { return s.started.Load() == 1 }, timeout, time.Millisecond)

	require.NoError(t, s.stop())
	require.Len(t, s.endings(), 1)
	_, err := os.Lstat(cfg.Unix.Path)
	require.ErrorIs(t, err, fs.ErrNotExist, "the socket file was not removed")
}

func TestHalfALineThenDisconnect(t *testing.T) {
	cfg := unixConfig(t)
	s := serve(t, cfg)

	conn := dialUnix(t, cfg.Unix.Path)
	_, err := conn.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"ec`))
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	require.Eventually(t, func() bool { return len(s.endings()) == 1 }, timeout, time.Millisecond)
	require.ErrorIs(t, s.endings()[0], io.ErrUnexpectedEOF)

	// That connection's error ended nothing else.
	got, err := newClient(t, dialUnix(t, cfg.Unix.Path)).ask("next client")
	require.NoError(t, err)
	require.Equal(t, "next client", got)
}

func TestMaxConnections(t *testing.T) {
	cfg := unixConfig(t)
	cfg.MaxConnections = 1
	s := serve(t, cfg)

	firstConn := dialUnix(t, cfg.Unix.Path)
	first := newClient(t, firstConn)
	_, err := first.ask("first")
	require.NoError(t, err)

	// The second connects, because the kernel queues it, and is not served.
	second := newClient(t, dialUnix(t, cfg.Unix.Path))
	answered := make(chan error, 1)
	go func() {
		_, err := second.ask("second")
		answered <- err
	}()
	select {
	case err := <-answered:
		t.Fatalf("the second connection was served while the first was open (error %v)", err)
	case <-time.After(200 * time.Millisecond):
	}
	require.EqualValues(t, 1, s.started.Load())

	require.NoError(t, firstConn.Close())
	select {
	case err := <-answered:
		require.NoError(t, err)
	case <-time.After(timeout):
		t.Fatal("the second connection was not served after the first closed")
	}
}

func TestNoListenerConfigured(t *testing.T) {
	_, err := listen.Open(context.Background(), config.Listen{Stdio: true}, nil)
	require.ErrorIs(t, err, listen.ErrNoListener)
}
