# A span for every HTTP request

2026-11-12

## Why

On `oku-trade/trade` PR 4673 with `deepseek/deepseek-v4.1-flash-1017` through Morph on OpenRouter, towa took 219 and 302 seconds where pi took 84 and 86, with request counts of 14 and 23 against 12 and 14 (`docs/notes/2026-11-12-multi-edit.md`). Read from the ACP event streams of those runs, the difference is time waiting for the model, not time in tools:

| Run | Requests | Tools | Waiting on the model | Of that, before the first event |
|---|---|---|---|---|
| towa `c42f04c`, run 1 | 23 | 50 s | 253 s | 175 s |
| towa `c42f04c`, run 2 | 14 | 38 s | 181 s | 87 s |
| pi, run 1 | 12 | 30 s | 53 s | 12 s |
| pi, run 2 | 14 | 18 s | 69 s | 15 s |

- The median wait before the first event of a request was 4.9 to 5.9 s for towa and 0.9 to 1.0 s for pi.
- The event streams cannot say where that wait is: in towa before the request is sent, in a new connection, or at the provider. towa recorded a span per prompt turn and per MCP call and nothing about an HTTP request.

## What changed

- `httpclient.New` wraps its transport in `otelhttp`, so every request towa sends is a client span: providers, OAuth, remote MCP, web fetch, embeddings and Jev. It is a child of the span in the request's context, which for a model request is the prompt turn's.
- `otelhttptrace` adds the phases of the request to that span as events: `http.getconn.start` and `.done` (with `http.conn.reused`, `http.conn.wasidle` and the idle time), `http.dns`, `http.connect` and `http.tls` for a new connection, `http.send.done` when the request is written, and `http.receive.start` at the first byte of the response.
- The span ends when the response body is closed, so for a model request it covers the whole stream.
- `telemetry.Start` sets the global tracer and meter providers when `telemetry.otel` is on. `otelhttp` uses the provider of the span in the request's context, and the global one for a request outside any span, such as reading a model list at startup. It also records the `http.client.*` metrics on the global meter.
- With `telemetry.otel` off nothing is recorded: the global provider is the no-op one.

## What a span must not hold

- **No trace header is sent.** `otelhttp` injects the global propagator's headers into the request. The servers are model providers and other third parties, so the transport is given an empty propagator, whatever the global one is.
- **No request header is recorded.** `otelhttptrace` records every header by default and hides six it knows by name. `x-api-key`, which the Anthropic client sends, is not among them. `WithoutHeaders` is set.
- **No URL query is recorded.** `otelhttp` records the URL in full, less its user and password. A query can hold a key, as the URL of a remote MCP server often does. The span is given the URL with its query replaced by `redacted`, and the request is sent to the URL the caller wrote. `otelhttp` has no option for this, so the transport wraps it on both sides.

`src/support/httpclient/trace_test.go` sends a request with a key in a header and one in the query to a local server and checks all three, and that the second request's span says its connection was reused.

## Events, not child spans

`otelhttptrace` makes a span per phase by default. They are events on the request's span here (`WithoutSubSpans`), for two reasons read from the library and from `net/http/httptrace` at Go 1.27.3:

- Its `http.receive` span starts at the first response byte and ends in `PutIdleConn`, and `net/http/httptrace` says "For HTTP/2, this hook is not currently used." Model providers are reached over HTTP/2, so that span would never end and never be exported.
- A model request would be six spans in place of one.

## Dependencies

- `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` v0.72.0 and `.../httptrace/otelhttptrace` v0.72.0: the releases whose `go.mod` requires `go.opentelemetry.io/otel` v1.47.0, the version already pinned.
- They bring `github.com/felixge/httpsnoop` v1.1.0 as an indirect requirement of `otelhttp`'s server side, which towa does not use.
- The static binary (`CGO_ENABLED=0 go build ./cmd/towa`) grows from 38,367,714 to 38,688,969 bytes, 321,255 more (0.8%).
- `vendorHash` in `nix/package.nix` is updated for the new `go.sum`.

## The local collector

Traces are read in a local Jaeger (v2.23.0), one process run as a systemd user unit, listening on localhost only:

- UI and query API: `http://localhost:16686` (`/api/v3/services`, `/api/v3/traces`).
- OTLP: `http://localhost:4318` over HTTP, `localhost:4317` over gRPC.
- It keeps traces. Metrics and logs sent to the same endpoint are accepted and dropped.

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 towa serve --telemetry.otel
```
