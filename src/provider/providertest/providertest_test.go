package providertest_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/httpclient"
	"github.com/gfx-labs/towa/src/provider/providertest"
)

const testKey = "sk-test-2f9c1b7a"

// upstream is the server an exchange is recorded from. It answers in two
// chunks and waits between them until the client has read the first, so the
// chunk boundary is not left to timing. It echoes the key, as a provider's
// error body can.
func upstream(t *testing.T, firstRead <-chan struct{}, cutOff bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Set-Cookie", "session=abc123")
		w.Header().Set("X-Request-Id", "req_1")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		_, _ = fmt.Fprintf(w, "data: first %s\n\n", r.Header.Get("X-Api-Key"))
		_ = rc.Flush()
		<-firstRead
		if cutOff {
			panic(http.ErrAbortHandler)
		}
		_, _ = io.WriteString(w, "data: second\n\n")
	}))
	t.Cleanup(server.Close)
	return server
}

func send(t *testing.T, client *http.Client, base, body string, afterFirstRead func()) (string, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/chat/completions?api_key="+testKey+"&n=1", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("X-Api-Key", testKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	var got strings.Builder
	buf := make([]byte, 4096)
	for first := true; ; first = false {
		n, err := resp.Body.Read(buf)
		got.Write(buf[:n])
		if first && afterFirstRead != nil {
			afterFirstRead()
		}
		if err == io.EOF {
			return got.String(), nil
		}
		if err != nil {
			return got.String(), err
		}
	}
}

func record(t *testing.T, cutOff bool, moreSecrets ...string) (path, body string, readErr error) {
	t.Helper()
	firstRead := make(chan struct{})
	server := upstream(t, firstRead, cutOff)
	path = filepath.Join(t.TempDir(), "exchange.json")
	client := httpclient.New()
	recorder := &providertest.Recorder{Next: client.Transport, Path: path, Secrets: append([]string{testKey}, moreSecrets...)}
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := r.Clone(r.Context())
		req.URL.Scheme = "http"
		req.URL.Host = server.Listener.Addr().String()
		req.RequestURI = ""
		resp, err := recorder.RoundTrip(req)
		if err != nil {
			t.Errorf("record relay: %v", err)
			http.Error(w, "record relay", http.StatusBadGateway)
			return
		}
		defer func() { require.NoError(t, resp.Body.Close()) }()
		for name, values := range resp.Header {
			w.Header()[name] = values
		}
		w.WriteHeader(resp.StatusCode)
		buf := make([]byte, 4096)
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
				_ = http.NewResponseController(w).Flush()
			}
			if readErr == io.EOF {
				return
			}
			if readErr != nil {
				panic(http.ErrAbortHandler)
			}
		}
	}))
	t.Cleanup(relay.Close)
	body, readErr = send(t, client, relay.URL, `{"model":"m","stream":true}`, func() { close(firstRead) })
	return path, body, readErr
}

// failures stands in for the test given to Replay, so that a test can check
// that a wrong request is reported.
type failures struct {
	testing.TB
	errors []string
}

func (f *failures) Helper() {}

func (f *failures) Errorf(format string, args ...any) {
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}

func TestRecorderPreflightsParentBeforeEndpoint(t *testing.T) {
	var effects atomic.Int64
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		effects.Add(1)
		_, _ = io.WriteString(w, "endpoint executed")
	}))
	defer endpoint.Close()
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), "missing", "nested")
			if !valid {
				parent = filepath.Join(t.TempDir(), "not-a-directory")
				require.NoError(t, os.WriteFile(parent, []byte("occupied"), 0o600))
			}
			path := filepath.Join(parent, "exchange.json")
			client := httpclient.New()
			client.Transport = &providertest.Recorder{Next: client.Transport, Path: path}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint.URL, nil)
			require.NoError(t, err)
			before := effects.Load()
			response, err := client.Do(req)
			if !valid {
				var pathError *os.PathError
				require.True(t, errors.As(err, &pathError), "parent failure must retain its filesystem cause: %v", err)
				require.Equal(t, before, effects.Load(), "invalid output must be refused before the endpoint runs")
				return
			}
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			exchange, err := providertest.Load(path)
			require.NoError(t, err)
			require.Equal(t, body, exchange.Response.Body())
			require.Equal(t, before+1, effects.Load())
		})
	}
}

func TestRecordingHoldsTheExchangeWithoutSecrets(t *testing.T) {
	path, body, err := record(t, false)
	require.NoError(t, err)
	require.Equal(t, "data: first "+testKey+"\n\ndata: second\n\n", body, "the recorder must not change what the client reads")

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(raw), testKey)
	require.NotContains(t, string(raw), "abc123")

	ex, err := providertest.Load(path)
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, ex.Request.Method)
	require.Equal(t, []string{providertest.Redacted}, ex.Request.Header["Authorization"])
	require.Equal(t, []string{providertest.Redacted}, ex.Request.Header["X-Api-Key"])
	require.Equal(t, []string{providertest.Redacted}, ex.Response.Header["Set-Cookie"])
	require.Equal(t, []string{"req_1"}, ex.Response.Header["X-Request-Id"])
	require.Contains(t, ex.Request.URL, "api_key="+providertest.Redacted)
	require.JSONEq(t, `{"model":"m","stream":true}`, string(ex.Request.Body))
	require.Equal(t, []providertest.Bytes{
		providertest.Bytes("data: first " + providertest.Redacted + "\n\n"),
		providertest.Bytes("data: second\n\n"),
	}, ex.Response.Chunks, "chunk boundaries are kept")
	require.False(t, ex.Response.Incomplete)
}

// A value to keep out of a recording can arrive in two reads. It is replaced
// all the same, and the client still reads what the provider sent.
func TestRecordingReplacesASecretSplitAcrossTwoReads(t *testing.T) {
	const split = "\n\ndata: sec"
	path, body, err := record(t, false, split)
	require.NoError(t, err)
	require.Equal(t, "data: first "+testKey+"\n\ndata: second\n\n", body)

	ex, err := providertest.Load(path)
	require.NoError(t, err)
	require.Equal(t, []providertest.Bytes{
		providertest.Bytes("data: first " + providertest.Redacted + providertest.Redacted),
		providertest.Bytes("ond\n\n"),
	}, ex.Response.Chunks)
	require.NotContains(t, string(ex.Response.Body()), split)
}

func TestReplayServesTheRecordingToTheSameRequest(t *testing.T) {
	path, _, err := record(t, false)
	require.NoError(t, err)

	server := providertest.Replay(t, path)
	// Member order and spacing differ from the recorded body.
	body, err := send(t, httpclient.New(), server.URL, `{ "stream": true, "model": "m" }`, nil)
	require.NoError(t, err)
	require.Equal(t, "data: first "+providertest.Redacted+"\n\ndata: second\n\n", body)
}

func TestReplayReportsARequestThatDiffers(t *testing.T) {
	path, _, err := record(t, false)
	require.NoError(t, err)

	cases := []struct {
		name   string
		change func(*http.Request)
		body   string
		want   string
	}{
		{"body value", func(*http.Request) {}, `{"model":"other","stream":true}`, `body.model: sent "other", recorded "m"`},
		{"body member added", func(*http.Request) {}, `{"model":"m","stream":true,"n":2}`, `body.n: sent 2, not recorded`},
		{"header missing", func(r *http.Request) { r.Header.Del("Content-Type") }, `{"model":"m","stream":true}`, `header Content-Type: not sent`},
		{"header added", func(r *http.Request) { r.Header.Set("X-Title", "t") }, `{"model":"m","stream":true}`, `header X-Title: sent ["t"], not recorded`},
		{"secret missing", func(r *http.Request) { r.Header.Del("Authorization") }, `{"model":"m","stream":true}`, `header Authorization: not sent`},
		{"query", func(r *http.Request) { r.URL.RawQuery = "api_key=k&n=2" }, `{"model":"m","stream":true}`, `query n: sent ["2"], recorded ["1"]`},
		{"path", func(r *http.Request) { r.URL.Path = "/v1/responses" }, `{"model":"m","stream":true}`, `path: sent /v1/responses, recorded /v1/chat/completions`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &failures{TB: t}
			server := providertest.Replay(f, path)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/chat/completions?api_key=k&n=1", strings.NewReader(c.body))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer k")
			req.Header.Set("X-Api-Key", "k")
			req.Header.Set("Content-Type", "application/json")
			c.change(req)
			resp, err := httpclient.New().Do(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Len(t, f.errors, 1)
			require.Contains(t, f.errors[0], c.want)
		})
	}
}

func TestReplayReportsThatNoRequestWasSent(t *testing.T) {
	path, _, err := record(t, false)
	require.NoError(t, err)

	f := &failures{TB: t}
	t.Run("unused", func(t *testing.T) {
		// Cleanup functions registered through f run when this subtest ends.
		f.TB = t
		providertest.Replay(f, path)
	})
	require.Len(t, f.errors, 1)
	require.Contains(t, f.errors[0], "no request was sent")
}

func TestACutOffResponseIsReplayedCutOff(t *testing.T) {
	path, body, err := record(t, true)
	require.Error(t, err)
	require.Equal(t, "data: first "+testKey+"\n\n", body)

	ex, err := providertest.Load(path)
	require.NoError(t, err)
	require.True(t, ex.Response.Incomplete)
	require.NotEmpty(t, ex.Response.ReadError)

	server := providertest.Replay(t, path)
	body, err = send(t, httpclient.New(), server.URL, `{"model":"m","stream":true}`, nil)
	require.Error(t, err, "the replayed body must fail to read as the recorded one did")
	require.Equal(t, "data: first "+providertest.Redacted+"\n\n", body)
}

func TestRecorderRefusesASecondRequest(t *testing.T) {
	firstRead := make(chan struct{})
	close(firstRead)
	server := upstream(t, firstRead, false)
	client := httpclient.New()
	client.Transport = &providertest.Recorder{Next: client.Transport, Path: filepath.Join(t.TempDir(), "exchange.json")}
	_, err := send(t, client, server.URL, `{}`, nil)
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
	require.NoError(t, err)
	resp, err := client.Do(req)
	if resp != nil {
		require.NoError(t, resp.Body.Close())
	}
	require.Error(t, err, "a retry must not overwrite the recording")
}
