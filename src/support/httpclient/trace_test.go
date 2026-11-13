package httpclient_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/gfx-labs/towa/src/support/httpclient"
)

// A request is one client span under the caller's span, with the phases of
// the request as events. The server gets the URL and headers the caller
// wrote and no trace header, and the span holds neither the key in the
// header nor the one in the query.
func TestARequestIsASpanThatHoldsNoCredential(t *testing.T) {
	const headerKey, queryKey = "header-s3cret", "query-s3cret"
	var got *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.WithoutCancel(r.Context()))
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	// The test's own context is already cancelled when cleanups run.
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	ctx, parent := provider.Tracer("test").Start(t.Context(), "turn")

	client := httpclient.New()
	defer client.CloseIdleConnections()
	for range 2 {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat?api_key="+queryKey, strings.NewReader("{}"))
		require.NoError(t, err)
		req.Header.Set("X-Api-Key", headerKey)
		req.Header.Set("Authorization", "Bearer "+headerKey)
		resp, err := client.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, "ok", string(body))

		require.Equal(t, queryKey, got.URL.Query().Get("api_key"))
		require.Equal(t, headerKey, got.Header.Get("X-Api-Key"))
		require.Empty(t, got.Header.Get("Traceparent"))
		require.Empty(t, got.Header.Get("Baggage"))
	}
	parent.End()

	spans := recorder.Ended()
	require.Len(t, spans, 3)
	reused := make([]bool, 0, 2)
	for _, span := range spans[:2] {
		require.Equal(t, trace.SpanKindClient, span.SpanKind())
		require.Equal(t, parent.SpanContext().SpanID(), span.Parent().SpanID())
		url := ""
		for _, kv := range span.Attributes() {
			require.NotContains(t, kv.Value.String(), headerKey, kv.Key)
			require.NotContains(t, kv.Value.String(), queryKey, kv.Key)
			if kv.Key == "url.full" {
				url = kv.Value.AsString()
			}
		}
		require.Equal(t, server.URL+"/v1/chat?redacted", url)

		events := map[string]bool{}
		for _, event := range span.Events() {
			events[event.Name] = true
			for _, kv := range event.Attributes {
				require.NotContains(t, kv.Value.String(), headerKey, kv.Key)
				require.NotContains(t, kv.Value.String(), queryKey, kv.Key)
				if kv.Key == "http.conn.reused" {
					reused = append(reused, kv.Value.AsBool())
				}
			}
		}
		// The connection, the request written and the first byte back.
		for _, name := range []string{"http.getconn.done", "http.send.done", "http.receive.start"} {
			require.True(t, events[name], "%s is not among %v", name, events)
		}
	}
	// The second request went out on the first one's connection, which the
	// span says: a new connection per request is what a trace is read for.
	require.Equal(t, []bool{false, true}, reused)
}
