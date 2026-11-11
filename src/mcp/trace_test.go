package mcp

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// One span per connect and per call, with the server, transport and tool,
// and nothing of what the call carried.
func TestSpansNameTheCallAndCarryNoContent(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	// The test's own context is already cancelled when cleanups run.
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

	m := newTestManager(t, Options{Servers: []Server{stdioServer(t, "s", modeServe)}, Tracer: provider.Tracer("test")})
	const secret = "s3cret-argument-text"
	res, err := m.CallTool(t.Context(), "s", "echo", jsontext.Value(`{"text":"`+secret+`"}`))
	require.NoError(t, err)
	require.Equal(t, secret, textOf(t, res))
	_, err = m.CallTool(t.Context(), "s", "fail", nil)
	require.NoError(t, err)
	// The server's refusal quotes the tool name it was sent.
	_, err = m.CallTool(t.Context(), "s", "no_such_tool", jsontext.Value(`{"text":"`+secret+`"}`))
	require.Error(t, err)

	spans := recorder.Ended()
	require.Len(t, spans, 4)
	attrs := func(i int) map[attribute.Key]attribute.Value {
		out := map[attribute.Key]attribute.Value{}
		for _, kv := range spans[i].Attributes() {
			out[kv.Key] = kv.Value
		}
		return out
	}

	// The connect ends first, inside the first call, and is its child.
	require.Equal(t, spanConnect, spans[0].Name())
	require.Equal(t, "s", attrs(0)[attrServer].AsString())
	require.Equal(t, "stdio", attrs(0)[attrTransport].AsString())
	require.Equal(t, spans[1].SpanContext().SpanID(), spans[0].Parent().SpanID())

	require.Equal(t, spanCall, spans[1].Name())
	require.Equal(t, "echo", attrs(1)[attrTool].AsString())
	require.Equal(t, "stdio", attrs(1)[attrTransport].AsString())
	require.False(t, attrs(1)[attrToolError].AsBool())
	require.Equal(t, codes.Unset, spans[1].Status().Code)

	// A tool that reports its own error is an answered call.
	require.True(t, attrs(2)[attrToolError].AsBool())
	require.Equal(t, codes.Unset, spans[2].Status().Code)

	require.Equal(t, "no_such_tool", attrs(3)[attrTool].AsString())
	require.Equal(t, codes.Error, spans[3].Status().Code)

	for _, span := range spans {
		require.NotContains(t, fmt.Sprint(span.Attributes(), span.Events(), span.Status()), secret)
	}
}
