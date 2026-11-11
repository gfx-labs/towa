# Recorded turn through an HTTPS proxy

Date: 2026-11-11

`src/cli/proxy_test.go` adds one acceptance test,
`TestARecordedTurnThroughHTTPSProxy`, to normal root Go tests. It needs no
live key, model server or network access beyond loopback sockets.

## Boundary

The parent serves the two real `looptest.ReadTurn` recordings from `98425d2`.
These are the recorded Ollama `llama3.2:3b` exchanges, not constructed provider
responses. Each request is passed to `providertest.Replay`'s handler, which
compares its method, path, query, headers and body with the recording and
streams the recorded response chunks. The first response calls `read_file`,
the tool reads the fixture's real `notes.txt`, and the second response answers.

The test creates a CA and a leaf certificate for `provider.invalid`, and serves
those replay handlers over TLS. A real CONNECT proxy accepts only that name
and port, then tunnels bytes to the local TLS server. The client cannot
connect directly to the upstream under that name.

Each turn runs in a new process with an explicit environment. The trusted
process gets `HTTPS_PROXY` and `SSL_CERT_FILE` pointing to the proxy and test
CA. `SSL_CERT_DIR` points to an otherwise empty test directory. Ambient proxy,
NO_PROXY, credential and trust variables are not inherited. This follows
`src/httpclient/httpclient_test.go`: Go caches environment proxy configuration
and certificate roots, so changing them in a shared test process would be
unreliable and race with other tests. The child selects this one test by name,
not a second helper test.

`looptest.At` from `49b5c7d` builds the real shared httpclient, Chat
Completions provider,
auth source, router, loop, file capability and directory store at the HTTPS
base URL. There is no transport rewrite, fake loop or alternate HTTP client.
The test reuses the helper's prompt, system text, tool and fixture files.

## Assertions

With the CA trusted:

- CONNECT was observed for the expected authority.
- Both recorded requests reached the TLS replay handlers and matched.
- The turn ended normally and its final text contains the recorded answer.
- Reading the stored history returns one completed `read_file` result holding
  the contents of `notes.txt`.

A second process runs the same turn without that CA. CONNECT must still be
observed, but the returned error chain must hold a
`*tls.CertificateVerificationError`. No HTTP replay request may arrive, no
final answer may be returned, and no completed tool result may be stored.
This negative control distinguishes proxy reachability from certificate trust.

The deadline of 30 seconds is the failure deadline used by the existing
httpclient proxy tests, not a production turn limit. The proxy's copy workers
are both joined after either side closes.

## Checks

- The focused test passed normally, with `-race -count=3`, and with
  `CGO_ENABLED=0`.
- The complete `src/cli` race suite passed. CLI vet and Darwin/arm64 CGO-disabled
  vet passed, including compilation of this test for the headless target.
- `golangci-lint run src/cli/proxy_test.go` reports zero issues.
- Package-wide CLI lint currently reports unused MCP runtime helpers in
  `src/cli/mcp_runtime.go`, which is concurrent work outside this task.
- Root `go build ./...` passed. Root CI already runs `go test -json -race ./...`,
  which discovers this acceptance test without a special flag. CI itself was
  not run from this session.
- One acceptance test is retained. No scaffolding test or constructed provider
  response was added. No live model request or key was used.

## Scope

This test covers the common provider/router/loop/store path behind the proxy.
It does not cover every provider dialect, OAuth refresh, WebSockets, or CLI
flag and stdio wiring. Those are not implied by a passing recorded Chat
Completions turn. No production file is changed by this test task.
