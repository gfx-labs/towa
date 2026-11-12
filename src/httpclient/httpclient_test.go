package httpclient_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json/v2"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/httpclient"
)

// Go reads the proxy variables and the root pool once per process, so the
// client under test runs in a child: this test binary, started again with
// the two variables below set. TestMain sends such a start to runChild.
const (
	envChildMode = "TOWA_HTTPCLIENT_CHILD"
	envChildURL  = "TOWA_HTTPCLIENT_URL"
)

// upstreamHost is the name the client asks for. Go never sends a request to
// localhost or a loopback address through a proxy, so the server needs
// another name. The name does not resolve: the proxy knows where it is.
const upstreamHost = "upstream.test"

const (
	greeting = "hello through the proxy"
	// childTimeout bounds the one request a child makes. It only matters
	// when something is broken.
	childTimeout = 30 * time.Second
)

// report is what a child prints: what it got, or why it got nothing.
type report struct {
	Body string `json:"body,omitzero"`
	// Failure is "certificate", "dns" or "other".
	Failure string `json:"failure,omitzero"`
	Error   string `json:"error,omitzero"`
}

func TestMain(m *testing.M) {
	if mode := os.Getenv(envChildMode); mode != "" {
		os.Exit(runChild(mode, os.Getenv(envChildURL)))
	}
	os.Exit(m.Run())
}

// runChild makes one request with the client every part of towa uses
// and prints the outcome.
func runChild(mode, url string) int {
	ctx, cancel := context.WithTimeout(context.Background(), childTimeout)
	defer cancel()
	client := httpclient.New()
	var body string
	var err error
	switch mode {
	case "get":
		body, err = get(ctx, client, url)
	case "websocket":
		body, err = echo(ctx, client, url)
	default:
		err = fmt.Errorf("mode %q is not defined", mode)
	}
	out := report{Body: body}
	if err != nil {
		out = report{Failure: "other", Error: err.Error()}
		var cert *tls.CertificateVerificationError
		var dns *net.DNSError
		switch {
		case errors.As(err, &cert):
			out.Failure = "certificate"
		case errors.As(err, &dns):
			out.Failure = "dns"
		}
	}
	if err := json.MarshalWrite(os.Stdout, out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func get(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	body, err := io.ReadAll(resp.Body)
	return string(body), errors.Join(err, resp.Body.Close())
}

// echo sends one message over a WebSocket and returns what comes back. The
// OpenAI WebSocket transport dials the same way.
func echo(ctx context.Context, client *http.Client, url string) (string, error) {
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: client})
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close() //nolint:errcheck // the handshake response has no body to lose
	}
	if err != nil {
		return "", err
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(greeting)); err != nil {
		return "", err
	}
	_, got, err := conn.Read(ctx)
	if err != nil {
		return "", err
	}
	return string(got), conn.Close(websocket.StatusNormalClosure, "")
}

// network is an HTTPS server whose certificate is signed by a CA made for
// the test, and a CONNECT proxy that is the only way to reach it by name.
type network struct {
	port     string
	caFile   string
	proxyURL string

	mu       sync.Mutex
	connects []string
}

func (n *network) seen() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.connects...)
}

func start(t *testing.T) *network {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "towa test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serverDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: upstreamHost},
		DNSNames:     []string{upstreamHost},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, caTemplate, &serverKey.PublicKey, caKey)
	require.NoError(t, err)

	n := &network{caFile: filepath.Join(t.TempDir(), "ca.pem")}
	require.NoError(t, os.WriteFile(n.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600))

	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, greeting)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		kind, msg, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		if err := conn.Write(r.Context(), kind, msg); err != nil {
			return
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &http.Server{
		Handler: mux,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{{Certificate: [][]byte{serverDER}, PrivateKey: serverKey}},
		},
		ReadHeaderTimeout: childTimeout,
	}
	go func() { _ = server.ServeTLS(upstream, "", "") }()
	t.Cleanup(func() { _ = server.Close() })
	_, n.port, err = net.SplitHostPort(upstream.Addr().String())
	require.NoError(t, err)

	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	proxy := &http.Server{
		ReadHeaderTimeout: childTimeout,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodConnect {
				http.Error(w, "this proxy only tunnels", http.StatusMethodNotAllowed)
				return
			}
			n.mu.Lock()
			n.connects = append(n.connects, r.Host)
			n.mu.Unlock()
			if r.Host != net.JoinHostPort(upstreamHost, n.port) {
				http.Error(w, "no route to "+r.Host, http.StatusBadGateway)
				return
			}
			tunnel(w, upstream.Addr().String())
		}),
	}
	go func() { _ = proxy.Serve(proxyListener) }()
	t.Cleanup(func() { _ = proxy.Close() })
	n.proxyURL = "http://" + proxyListener.Addr().String()
	return n
}

// tunnel answers a CONNECT and copies bytes both ways until one side ends.
func tunnel(w http.ResponseWriter, target string) {
	out, err := net.Dial("tcp", target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer out.Close() //nolint:errcheck // the tunnel is over either way
	in, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer in.Close() //nolint:errcheck // the tunnel is over either way
	if _, err := io.WriteString(in, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(out, buffered)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(in, out)
		done <- struct{}{}
	}()
	<-done
}

// child runs the client in a new process with exactly the given
// environment, so nothing is inherited from whoever runs the tests.
func (n *network) child(t *testing.T, mode, url string, env ...string) report {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append([]string{envChildMode + "=" + mode, envChildURL + "=" + url}, env...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	require.NoError(t, err)
	var got report
	require.NoError(t, json.Unmarshal(out, &got), string(out))
	return got
}

func (n *network) url(scheme, path string) string {
	return scheme + "://" + net.JoinHostPort(upstreamHost, n.port) + path
}

func TestAWebSocketGoesThroughTheProxyAndTrustsTheSystemPool(t *testing.T) {
	n := start(t)
	got := n.child(t, "websocket", n.url("wss", "/ws"), "HTTPS_PROXY="+n.proxyURL, "SSL_CERT_FILE="+n.caFile)
	require.Equal(t, report{Body: greeting}, got)
	require.Equal(t, []string{net.JoinHostPort(upstreamHost, n.port)}, n.seen())
}

// Without the CA in the pool the tunnel is made and the handshake inside it
// is refused: the client does not skip verification behind a proxy.
func TestACertificateFromAnUnknownCAIsRefused(t *testing.T) {
	n := start(t)
	got := n.child(t, "websocket", n.url("wss", "/ws"), "HTTPS_PROXY="+n.proxyURL)
	require.Equal(t, "certificate", got.Failure, got.Error)
	require.Empty(t, got.Body)
	require.Len(t, n.seen(), 1)
}

// With the host in NO_PROXY the client goes straight to it. The name does
// not resolve, so the request fails at the lookup, and the proxy is never
// asked.
func TestNoProxyKeepsTheHostAwayFromTheProxy(t *testing.T) {
	n := start(t)
	for _, c := range []struct{ mode, url string }{
		{"get", n.url("https", "/hello")},
		{"websocket", n.url("wss", "/ws")},
	} {
		got := n.child(t, c.mode, c.url, "HTTPS_PROXY="+n.proxyURL, "SSL_CERT_FILE="+n.caFile, "NO_PROXY="+upstreamHost)
		require.Equal(t, "dns", got.Failure, got.Error)
	}
	require.Empty(t, n.seen())
}
