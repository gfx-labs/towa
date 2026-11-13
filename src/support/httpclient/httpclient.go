// Package httpclient builds the HTTP client for every request towa
// sends: providers, OAuth, remote MCP, webfetch, embeddings, Jev and OTLP.
//
// towa may run where the only way out is a proxy that also holds the
// credentials. A transport built by hand has no proxy set and ignores
// HTTPS_PROXY, so one such client anywhere fails there. The lint
// configuration forbids http.Transport, http.DefaultTransport,
// http.DefaultClient and the http.Get family outside this package.
//
// Every request is also a client span (otelhttp), with the phases of the
// request as events on it (otelhttptrace). The span goes to the tracer
// provider of the span in the request's context, or to the process's global
// one, which src/support/telemetry sets when telemetry.otel is on. With it
// off the span records nothing. A span holds no request header and no URL
// query, since either may be a credential.
package httpclient

import (
	"context"
	"net/http"
	"net/http/httptrace"
	"net/url"

	"go.opentelemetry.io/contrib/instrumentation/net/http/httptrace/otelhttptrace"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
)

// New returns a client with its own connection pool. It takes its proxy
// from HTTP_PROXY, HTTPS_PROXY and NO_PROXY, and trusts the system
// certificate pool.
//
// SSL_CERT_FILE names the certificate file to trust in place of the
// system's default file, so a file that holds only a proxy's CA must also
// hold the public roots, unless they are in a certificate directory. On
// Linux the default directories, or the ones in SSL_CERT_DIR, are read as
// well. On macOS with Go 1.27, setting either variable turns the system
// verifier off and only the named file and directories are trusted.
//
// The client has no overall timeout, because a model stream has no bound on
// its length. Callers bound a request with its context.
func New() *http.Client {
	base := Transport()
	return &http.Client{Transport: traced{
		spans: otelhttp.NewTransport(realURL{base},
			otelhttp.WithClientTrace(clientTrace),
			// No trace context is sent: the servers are model providers and
			// other third parties, whatever the global propagator is.
			otelhttp.WithPropagators(propagation.NewCompositeTextMapPropagator()),
		),
		base: base,
	}}
}

// hiddenQuery stands in a span for the query of a request's URL.
const hiddenQuery = "redacted"

// traced is the transport with a span around each request.
type traced struct {
	spans *otelhttp.Transport
	base  *http.Transport
}

type urlKey struct{}

// RoundTrip keeps the query of the URL out of the span. A query can hold a
// key, as the URL of a remote MCP server often does, and otelhttp records
// the URL it is given in full. So it is given one without the query, and
// realURL puts the request's own URL back before it is sent.
func (t traced) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL == nil || r.URL.RawQuery == "" {
		return t.spans.RoundTrip(r)
	}
	shown := r.Clone(context.WithValue(r.Context(), urlKey{}, r.URL))
	shown.URL.RawQuery = hiddenQuery
	return t.spans.RoundTrip(shown)
}

// CloseIdleConnections is the method http.Client.CloseIdleConnections looks
// for, which otelhttp's transport does not have.
func (t traced) CloseIdleConnections() { t.base.CloseIdleConnections() }

// realURL sends a request to the URL its caller gave it.
type realURL struct{ base *http.Transport }

func (t realURL) RoundTrip(r *http.Request) (*http.Response, error) {
	if u, ok := r.Context().Value(urlKey{}).(*url.URL); ok {
		r = r.Clone(r.Context())
		r.URL = u
	}
	return t.base.RoundTrip(r)
}

// clientTrace adds where a request's time went to its span, as events: the
// connection and whether it was reused, DNS and TLS for a new one, the
// request written, and the first byte of the response.
//
// Request headers are left out. They hold the credential, and the library
// hides only the headers it knows by name, which x-api-key is not.
func clientTrace(ctx context.Context) *httptrace.ClientTrace {
	return otelhttptrace.NewClientTrace(ctx, otelhttptrace.WithoutSubSpans(), otelhttptrace.WithoutHeaders())
}

// Transport returns a transport with the same proxy and trust settings as
// New, for the callers that need a transport and not a client.
func Transport() *http.Transport {
	// The default transport reads the proxy from the environment and leaves
	// the root CAs unset, which means the system pool. Its dial, handshake
	// and idle timeouts are the ones net/http documents.
	return http.DefaultTransport.(*http.Transport).Clone()
}
