// Package conformance checks towa's wire format with a second ACP
// implementation as the client: github.com/coder/acp-go-sdk. The tests
// build the real binary, start "towa serve" and talk to it over stdio,
// Unix sockets and TCP. Nothing of towa is imported.
//
// It is its own module so that the SDK is not a dependency of towa.
package conformance
