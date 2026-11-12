package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/gfx-labs/towa/pkg/acp"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/embed"
	"github.com/gfx-labs/towa/src/httpclient"
	"github.com/gfx-labs/towa/src/loop/looptest"
	"github.com/gfx-labs/towa/src/provider/providertest"
	"github.com/gfx-labs/towa/src/telemetry"
	"github.com/stretchr/testify/require"
)

// Recording requires separate approval for exactly one local embedding request.
const recordRuntimeEmbedding = "TOWA_RECORD_RUNTIME_EMBEDDING"

func TestRuntimeRetainsSemanticEmbeddingAcrossTurns(t *testing.T) {
	fixture := looptest.Replay(t, looptest.ReadTurn)
	id := uuid.NewV7()
	turn := fixture.Turn(acp.SessionId(id.String()), func(context.Context, acp.SessionUpdate) error { return nil })
	_, err := fixture.Loop.Run(t.Context(), turn)
	require.NoError(t, err)
	require.Equal(t, looptest.ReadTurn.Requests, fixture.Served())

	// No new model reply is invented. A failed main request appends no Usage,
	// so the next turn sees the same completed request to embed.
	var mainRequests atomic.Int64
	main := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mainRequests.Add(1)
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(main.Close)
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	cfg := loaded.Config
	// The second entry is the first with an effort, so that it is another
	// model to select. The turns start on the last entry, which has no
	// entry to fall back to.
	cfg.Models = []string{"abort:local/gpt-5.5#low", "abort:local/gpt-5.5"}
	cfg.ModelContextWindow, cfg.ModelMaxOutputTokens = looptest.ContextSize, 1024
	turn.Model = cfg.Models[1]
	cfg.Providers = []config.Provider{{ID: "abort", Kind: config.ProviderResponses, BaseURL: main.URL + "/v1"}}
	cfg.Accounts = []config.Account{{ID: "local", Provider: "abort", Kind: config.AccountAPIKey, APIKeyEnv: "RUNTIME_TEST_KEY"}}
	cfg.Limits.Retry.MaxRetries = 0
	cfg.Compaction.Mode, cfg.Compaction.OpenAINative = config.CompactionSemantic, config.NativeOff
	cfg.Compaction.ProactiveFloorTokens = 0
	cfg.Compaction.ProactiveFloor = cfg.Compaction.ProactiveThreshold

	path := filepath.Join("testdata", "ollama_runtime_turn_embedding.json")
	client := httpclient.New()
	t.Cleanup(client.CloseIdleConnections)
	transport := &runtimeEmbeddingTransport{next: client.Transport, limit: 2}
	base := looptest.OllamaBase
	recording := os.Getenv(recordRuntimeEmbedding) != ""
	if recording {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		transport.limit = 1
		transport.next = &providertest.Recorder{Next: client.Transport, Path: path}
	} else {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			t.Skip("single-turn runtime embedding has not been recorded")
		}
		for range transport.limit {
			server := providertest.Replay(t, path)
			u, err := url.Parse(server.URL)
			require.NoError(t, err)
			transport.hosts = append(transport.hosts, u)
		}
	}
	client.Transport = transport
	cfg.Embeddings.BaseURL, cfg.Embeddings.Model = base, looptest.Model
	cfg.Embeddings.Account, cfg.Embeddings.Dimensions = "", 0

	home := t.TempDir()
	host := Host{Stdout: io.Discard, Stderr: io.Discard, Environ: []string{
		"HOME=" + home, "XDG_STATE_HOME=" + filepath.Join(home, "state"), "RUNTIME_TEST_KEY=local-placeholder",
	}}
	tel, err := telemetry.Start(t.Context(), cfg.Telemetry, version(), io.Discard)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tel.Shutdown(context.Background())) })
	factory, err := newRuntimeFactory(t.Context(), cfg, host, tel)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, factory.Close()) })
	factory.store = fixture.Store
	factory.embeddings, err = embed.New(embed.Options{Config: cfg.Embeddings, HTTPClient: client})
	require.NoError(t, err)
	runtime, err := factory.open(t.Context(), turn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })

	for range 2 {
		_, err = runtime.Run(t.Context(), turn)
		require.Error(t, err)
		require.EqualValues(t, 1, transport.requests.Load(), "the same preceding Usage must not be embedded again")
	}
	require.EqualValues(t, 2, mainRequests.Load(), "both turns must reach the real main HTTP failure boundary")
	exchange, err := providertest.Load(path)
	require.NoError(t, err, "a successful embedding must have persisted a readable exchange")
	require.Equal(t, http.StatusOK, exchange.Response.Status)
	if !recording {
		turn.Model = cfg.Models[0]
		_, err = runtime.Run(t.Context(), turn)
		require.Error(t, err)
		require.EqualValues(t, 2, transport.requests.Load(), "changing the configured model resets semantic state")
		require.EqualValues(t, 3, mainRequests.Load())
	}
}

type runtimeEmbeddingTransport struct {
	next     http.RoundTripper
	hosts    []*url.URL
	limit    int64
	requests atomic.Int64
}

func (r *runtimeEmbeddingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	n := r.requests.Add(1)
	if n > r.limit {
		return nil, fmt.Errorf("runtime embedding request %d exceeds the authorized count %d", n, r.limit)
	}
	if len(r.hosts) > 0 {
		req = req.Clone(req.Context())
		req.URL.Scheme, req.URL.Host, req.Host = r.hosts[n-1].Scheme, r.hosts[n-1].Host, ""
	}
	return r.next.RoundTrip(req)
}
