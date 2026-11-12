// Package conformance checks acpharness's wire format with a second ACP
// implementation as the client: github.com/coder/acp-go-sdk. The tests
// build the real binary, start "acph serve" and talk to it over stdio,
// Unix sockets and TCP. Nothing of acpharness is imported.
//
// It is its own module so that the SDK is not a dependency of acpharness.
package conformance
