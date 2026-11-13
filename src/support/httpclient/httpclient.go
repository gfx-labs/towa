// Package httpclient builds the HTTP client for every request towa
// sends: providers, OAuth, remote MCP, webfetch, embeddings, Jev and OTLP.
//
// towa may run where the only way out is a proxy that also holds the
// credentials. A transport built by hand has no proxy set and ignores
// HTTPS_PROXY, so one such client anywhere fails there. The lint
// configuration forbids http.Transport, http.DefaultTransport,
// http.DefaultClient and the http.Get family outside this package.
package httpclient

import "net/http"

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
	return &http.Client{Transport: Transport()}
}

// Transport returns a transport with the same proxy and trust settings as
// New, for the callers that need a transport and not a client.
func Transport() *http.Transport {
	// The default transport reads the proxy from the environment and leaves
	// the root CAs unset, which means the system pool. Its dial, handshake
	// and idle timeouts are the ones net/http documents.
	return http.DefaultTransport.(*http.Transport).Clone()
}
