package cli_test

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
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/gfx-labs/towa/pkg/acp"
	"github.com/gfx-labs/towa/src/loop/looptest"
	"github.com/gfx-labs/towa/src/provider/providertest"
	"github.com/gfx-labs/towa/src/store"
	"github.com/gfx-labs/towa/src/transcript"
	"github.com/stretchr/testify/require"
)

// The process isolation and 30s failure deadline follow httpclient's proxy
// acceptance tests: Go caches environment proxies and system roots.
const (
	proxyTurnChild   = "TOWA_PROXY_TURN_CHILD"
	proxyTurnBase    = "TOWA_PROXY_TURN_BASE"
	proxyTurnReport  = "TOWA_PROXY_TURN_REPORT"
	proxyTurnHost    = "provider.invalid"
	proxyTurnTimeout = 30 * time.Second
)

type proxyTurnResult struct {
	Final              string                  `json:"final"`
	Stop               acp.StopReason          `json:"stop"`
	StoredResults      []transcript.ToolResult `json:"stored_results"`
	CertificateFailure bool                    `json:"certificate_failure"`
	Error              string                  `json:"error"`
}

func TestARecordedTurnThroughHTTPSProxy(t *testing.T) {
	if os.Getenv(proxyTurnChild) == "1" {
		runProxyTurnChild(t)
		return
	}
	// A recording's own handler checks request headers and bodies and writes
	// its actual response chunks. Neither the provider nor the loop is faked.
	handlers := make([]http.Handler, looptest.ReadTurn.Requests)
	for i := range handlers {
		handlers[i] = providertest.Replay(t, looptest.RecordingPath(looptest.ReadTurn, i+1)).Config.Handler
	}
	var served atomic.Int64
	ca, certificate := proxyTurnCertificate(t)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(served.Add(1)) - 1
		if n >= len(handlers) {
			t.Errorf("unexpected model request %d", n+1)
			http.Error(w, "no recording", http.StatusInternalServerError)
			return
		}
		handlers[n].ServeHTTP(w, r)
	}))
	upstream.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
	upstream.StartTLS()
	t.Cleanup(upstream.Close)
	_, port, err := net.SplitHostPort(upstream.Listener.Addr().String())
	require.NoError(t, err)
	authority := net.JoinHostPort(proxyTurnHost, port)

	var mu sync.Mutex
	var connects []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != authority {
			http.Error(w, "only the test provider is reachable", http.StatusBadGateway)
			return
		}
		mu.Lock()
		connects = append(connects, r.Host)
		mu.Unlock()
		proxyTurnTunnel(w, upstream.Listener.Addr().String())
	}))
	t.Cleanup(proxy.Close)
	seen := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), connects...) }

	// Pass only the chosen environment. Ambient NO_PROXY, provider keys and
	// certificate overrides cannot accidentally make the test succeed.
	child := func(trust bool) proxyTurnResult {
		dir := t.TempDir()
		reportPath := filepath.Join(dir, "report.json")
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestARecordedTurnThroughHTTPSProxy$", "-test.timeout="+proxyTurnTimeout.String())
		cmd.Env = []string{proxyTurnChild + "=1", proxyTurnBase + "=https://" + authority + "/v1", proxyTurnReport + "=" + reportPath, "HTTPS_PROXY=" + proxy.URL, "SSL_CERT_DIR=" + dir}
		if trust {
			cmd.Env = append(cmd.Env, "SSL_CERT_FILE="+ca)
		}
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "child output: %s", output)
		body, err := os.ReadFile(reportPath)
		require.NoError(t, err, "child output: %s", output)
		var result proxyTurnResult
		require.NoError(t, json.Unmarshal(body, &result))
		return result
	}
	got := child(true)
	require.Empty(t, got.Error)
	require.False(t, got.CertificateFailure)
	require.Equal(t, acp.StopReasonEndTurn, got.Stop)
	require.Contains(t, got.Final, "7421")
	require.Len(t, got.StoredResults, 1)
	require.Equal(t, looptest.ReadToolName, got.StoredResults[0].Name)
	require.Equal(t, transcript.OutcomeCompleted, got.StoredResults[0].Outcome)
	require.Len(t, got.StoredResults[0].Parts, 1)
	require.Equal(t, looptest.ReadTurn.Files["notes.txt"], got.StoredResults[0].Parts[0].Text)
	require.EqualValues(t, looptest.ReadTurn.Requests, served.Load())
	before := seen()
	require.NotEmpty(t, before, "a direct connection must not satisfy acceptance")
	for _, host := range before {
		require.Equal(t, authority, host)
	}

	// The same real turn must fail at TLS when its CA is absent. A successful
	// CONNECT alone cannot satisfy acceptance and verification is never skipped.
	untrusted := child(false)
	require.True(t, untrusted.CertificateFailure, untrusted.Error)
	require.NotEmpty(t, untrusted.Error)
	require.Empty(t, untrusted.Final)
	require.Empty(t, untrusted.StoredResults)
	require.Greater(t, len(seen()), len(before))
	require.EqualValues(t, looptest.ReadTurn.Requests, served.Load(), "untrusted client reached an HTTP replay handler")
}

func runProxyTurnChild(t *testing.T) {
	t.Helper()
	f := looptest.At(t, looptest.ReadTurn, os.Getenv(proxyTurnBase))
	id := uuid.NewV7()
	turn := f.Turn(acp.SessionId(id.String()), func(context.Context, acp.SessionUpdate) error { return nil })
	result, err := f.Loop.Run(t.Context(), turn)
	report := proxyTurnResult{Final: result.Final, Stop: result.StopReason}
	if err != nil {
		report.Error = err.Error()
		var certificate *tls.CertificateVerificationError
		report.CertificateFailure = errors.As(err, &certificate)
	}
	entries, err := f.Store.History(t.Context(), id, store.Range{})
	require.NoError(t, err)
	for entry, err := range entries {
		require.NoError(t, err)
		if entry.Item.ToolResult != nil {
			report.StoredResults = append(report.StoredResults, *entry.Item.ToolResult)
		}
	}
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(os.Getenv(proxyTurnReport), encoded, 0o600))
}

func proxyTurnCertificate(t *testing.T) (string, tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	now := time.Now()
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "towa turn test CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	require.NoError(t, err)
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{proxyTurnHost}, NotBefore: root.NotBefore, NotAfter: root.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	serverDER, err := x509.CreateCertificate(rand.Reader, leaf, root, &serverKey.PublicKey, key)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600))
	return path, tls.Certificate{Certificate: [][]byte{serverDER}, PrivateKey: serverKey}
}

func proxyTurnTunnel(w http.ResponseWriter, target string) {
	upstream, err := net.DialTimeout("tcp", target, proxyTurnTimeout)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer upstream.Close() //nolint:errcheck // the connection is closed in either outcome
	client, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer client.Close() //nolint:errcheck // the connection is closed in either outcome
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, buffered); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
	_ = upstream.Close()
	_ = client.Close()
	<-done
}
