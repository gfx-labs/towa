# HTTP client: proxy and trust tests

2026-11-11

protocol7 runs towa in a pod whose only way out is a proxy that presents its own certificate. Every outbound connection therefore has to go through `HTTPS_PROXY` and trust the system CA pool. `src/httpclient` had no test for either. It has now.

## Result

`src/httpclient` already does both, for plain requests and for a WebSocket. No code changed. The only edit to `httpclient.go` is the doc comment on `New`, which was wrong about `SSL_CERT_FILE` (see below).

## The tests

`src/httpclient/httpclient_test.go`. Nothing is mocked.

- Each test makes a CA with `crypto/x509`, signs a server certificate for the name `upstream.test` with it, and starts a real HTTPS server and a real CONNECT proxy on loopback ports.
- Go reads the proxy variables and the root pool once per process. The client therefore runs in a child: the test binary starts itself again with `HTTPS_PROXY` and `SSL_CERT_FILE` set, and `TestMain` sends that start to a function that makes one request with `httpclient.New()` and prints the outcome as JSON.
- The child gets exactly the environment the test gives it and nothing from whoever runs the tests. The suite passes with `HTTPS_PROXY`, `NO_PROXY` and `SSL_CERT_FILE` set to wrong values in the caller's environment.
- The server is asked for as `upstream.test`, which does not resolve. Go never sends a request for `localhost` or a loopback address through a proxy, so a loopback URL would have tested nothing. Only the proxy knows where `upstream.test` is, so a request that succeeds went through it.

| test | asserts |
|---|---|
| `TestARequestGoesThroughTheProxyAndTrustsTheSystemPool` | the body arrives, and the proxy saw one CONNECT for `upstream.test:<port>` |
| `TestAWebSocketGoesThroughTheProxyAndTrustsTheSystemPool` | the same for `websocket.Dial` with `DialOptions.HTTPClient` set to the client, with a message echoed back |
| `TestACertificateFromAnUnknownCAIsRefused` | without `SSL_CERT_FILE` both fail with a `*tls.CertificateVerificationError`, after the proxy made the tunnel |
| `TestNoProxyKeepsTheHostAwayFromTheProxy` | with `NO_PROXY=upstream.test` the proxy sees nothing, and both fail at the name lookup |

### Do the tests catch a broken client?

Checked by breaking `Transport()` twice by hand and restoring it:

| change | tests that failed |
|---|---|
| return `&http.Transport{}` (no proxy) | the two positive tests and the certificate test |
| add `InsecureSkipVerify: true` | the certificate test |

## What `SSL_CERT_FILE` does

Read from `crypto/x509/root.go` in Go 1.27.3 (`loadSystemRoots`, `loadOnDiskRoots`), not from documentation:

- Linux: `SSL_CERT_FILE` replaces the default certificate file. It is not added to it. The certificate directories (`/etc/ssl/certs`, `/etc/pki/tls/certs`, or those in `SSL_CERT_DIR`) are still read.
- So a pod that sets `SSL_CERT_FILE` to a file holding only the proxy's CA keeps the public roots only if they are in one of those directories. The safe file is the system bundle with the proxy's CA appended.
- macOS: with neither variable set, the system verifier is used. With either set, Go 1.27 turns the system verifier off and trusts only the named file and directories (`GODEBUG=x509sslcertoverrideplatform=0` restores the old behaviour, where the variables were ignored). So `SSL_CERT_FILE` is honoured on macOS with this Go version, and it drops the keychain's roots.

The macOS behaviour is from the source only. The tests compile for `darwin/arm64` and were not run there.

## Not covered

- `HTTP_PROXY` for a plain `http://` URL. The providers are all HTTPS.
- A proxy that needs credentials (`Proxy-Authorization` from the userinfo in the proxy URL).
- `SSL_CERT_DIR`.
- A client built by a dependency. `coder/websocket` is covered because the test passes it our client, which is what the OpenAI transport has to do. A dependency that is given no client makes its own, and only the turn-through-a-proxy test will catch that.
- Which HTTP version ran inside the tunnel was not asserted or looked at.

## Checks

```
nice -n 10 go test -race -count=5 ./src/httpclient/
nice -n 10 golangci-lint run ./src/httpclient/...
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go vet ./src/httpclient/
env HTTPS_PROXY=http://127.0.0.1:1 NO_PROXY=upstream.test SSL_CERT_FILE=/nonexistent go test -count=1 ./src/httpclient/
```

All passed on Linux. One run takes about 0.3 s without the race detector and about 7 s with it.
