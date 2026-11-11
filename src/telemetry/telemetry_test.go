package telemetry_test

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/telemetry"
)

// With OTel on, a span, a measurement and a log record made before Shutdown
// reach the OTLP endpoint named by the environment, and the stderr line for
// the record carries the span's trace id.
func TestShutdownExportsEverySignal(t *testing.T) {
	var mu sync.Mutex
	posts := map[string]int{}
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		posts[r.URL.Path]++
		mu.Unlock()
	}))
	defer collector.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)

	cfg := config.Telemetry{
		OTel: true,
		Logs: config.Logs{
			Stderr: config.StderrLog{Enabled: true, Level: slog.LevelInfo, Format: config.FormatText},
			OTel:   config.OTelLog{Enabled: true, Level: slog.LevelInfo},
		},
	}
	var stderr bytes.Buffer
	tel, err := telemetry.Start(t.Context(), cfg, "test", &stderr)
	require.NoError(t, err)

	ctx, span := tel.Tracer.Start(t.Context(), "turn")
	counter, err := tel.Meter.Int64Counter("turns")
	require.NoError(t, err)
	counter.Add(ctx, 1)
	tel.Logger.InfoContext(ctx, "turn finished")
	tel.Logger.DebugContext(ctx, "below both levels")
	span.End()

	require.NoError(t, tel.Shutdown(t.Context()))

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, map[string]int{"/v1/traces": 1, "/v1/metrics": 1, "/v1/logs": 1}, posts)
	require.Contains(t, stderr.String(), "trace_id="+span.SpanContext().TraceID().String())
	require.NotContains(t, stderr.String(), "below both levels")
}
