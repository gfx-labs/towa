# Shared contracts, JSON v2, buffers and HTTP clients

2026-11-11

This is the base the parallel build starts from. It also takes in the eight asks in protocol7's `docs/acpharness-needs.md`.

## Moves

- The command line is in `src/cli`. `cmd/towa/main.go` sets up signals and calls `cli.Run`.
- The ACP package is `pkg/acp`, because protocol7's worker imports it as the client side. It imports only the standard library, so a client pulls in none of our other dependencies.

## Contracts

Written before the lanes so they do not each invent one.

| package | what it fixes |
|---|---|
| `src/transcript` | `Entry` and `Item`: the one conversation format. Provider-only data rides in `Item.Opaque`, tagged with `Item.Provider` |
| `src/provider` | `Provider.Stream` returns `iter.Seq2[Event, error]`. Every error is a `*provider.Error` with a `Class` (fail, retry, fallback) and a `Reason` |
| `src/auth` | `Credential` and `Source`. `Credential.Proxied` marks an account whose token the harness never holds |
| `src/tools` | `Tool`, `Call`, `Result`, `Set`, and the `Effect` that decides what may run at once |
| `src/store` | `Store` and `Meta` |
| `src/httpclient` | the one constructor for HTTP clients |
| `src/loop` | `Result` and `StopError` |
| `pkg/acp/harness` | `Stop`: why a turn stopped, as a type a client can import |

## Typed stop

ACP has five stop reasons and no field for a rate limit. A turn that stops for a reason the client can act on carries `harness.Stop` under the `towa` key:

- in the prompt response's `_meta`, when the turn still has a stop reason (a configured limit answers `max_turn_requests`);
- in the error's `data`, when the turn failed (rate limit, auth, overload, context full, network, invalid request). A rejected credential answers with code `-32000`, the others with `-32603`.

`Stop` has the kind, the reset time, the retry-after in seconds, the limit's config key, the account and the model. `TestStoppedTurnCarriesATypedReason` sends one through a real connection and reads it back with `harness.FromError` and `harness.FromMeta`.

## JSON v2

Go 1.27.3 has `encoding/json/v2` and `encoding/json/jsontext` without an experiment flag. Probed with small programs in a scratch module:

| behaviour | result |
|---|---|
| nil slice, nil map | `[]`, `{}` |
| `omitzero` on a nil slice, a zero struct, a zero time | left out |
| `omitempty` on a zero int | kept, as `0` |
| HTML characters in a string | not escaped |
| member name in another case | not matched |
| unknown member | accepted, rejected with `json.RejectUnknownMembers(true)` |
| duplicate member name, invalid UTF-8 | error |
| `time.Duration` | error: "no default representation". The `format` tag options tried (`units`, `nano`, `sec`, `iso8601`) are all unsupported |
| `uuid.UUID`, `[]byte`, `jsontext.Value` | a string, base64, embedded as is |
| `json.RawMessage` | the same type as `jsontext.Value` |
| JSON `null` into a slice | nil |

The text of a v2 error is not stable. The same binary printed "cannot marshal" in one run and "unable to marshal" in the next. `encoding/json/v2/errors.go` picks the verb once per process from a map iteration, on purpose, so that nobody matches on the text.

`pkg/acp` and `tools/acpgen` still use v1. The lint rule excludes them until they move.

## Lint rules

- `depguard` rejects `encoding/json` outside `pkg/acp`.
- `forbidigo` rejects `http.Transport`, `http.DefaultTransport`, `http.DefaultClient`, `http.Get`, `Head`, `Post` and `PostForm` outside `src/httpclient`.

Both were checked against a throwaway file that breaks them, with the import aliased. The first version of the `forbidigo` rule matched nothing: the pattern has to name the package as it is written in the source (`http.`) with the import path in `pkg`. The rule also flags the `Transport:` key of an `http.Client` literal. `c.Do` and methods on a client are not flagged.

Lint sees only our code. Dependencies that make their own client when given none, read from their sources:

| dependency | default | what we do |
|---|---|---|
| OTLP trace, metric and log exporters (v1.47.0, v0.23.0) | their own transport with `Proxy: http.ProxyFromEnvironment` and the system roots | nothing: they already honour the proxy |
| `coder/websocket` v1.8.15 | `http.DefaultClient` | pass `HTTPClient` from `src/httpclient` |
| `modelcontextprotocol/go-sdk` v1.8.0 | `http.DefaultClient` | pass a client from `src/httpclient` to both HTTP transports |

`http.DefaultClient` uses `http.DefaultTransport`, which reads the proxy from the environment, so these two would work today. We still pass our client so that the behaviour does not depend on a default.

## macOS

`GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build ./...` and `go vet` pass on this commit, and CI now runs both. Nothing is run on macOS. `flock`, process groups and anything else that differs goes in `_linux.go` and `_darwin.go` files.

## Dependencies

Versions looked up on 2026-11-11. They are all in `go.mod` now, marked indirect until a package imports them, so that packages built in parallel do not each edit `go.mod`. `go mod tidy` runs once the build-out is integrated and drops any that ended up unused.

| module | version | for |
|---|---|---|
| `github.com/valyala/bytebufferpool` | v1.0.0 (2016, no dependencies) | pooled per-message buffers. The user asked for it |
| `github.com/gfx-labs/sse` | v0.0.0-20261111101910-02cd9ac15d6e | SSE decoding in the provider clients |
| `github.com/coder/websocket` | v1.8.15 | the OpenAI WebSocket transport |
| `github.com/modelcontextprotocol/go-sdk` | v1.8.0 | the MCP client |
| `github.com/klauspost/compress` | v1.20.4 | zstd for request bodies and stored sessions |
| `golang.org/x/sys` | v0.50.0 | `flock` and process groups |

The MCP SDK brings seven modules of its own: `google/jsonschema-go` v0.4.3, `segmentio/encoding` v0.5.4 with `segmentio/asm`, `yosida95/uritemplate/v3`, `golang.org/x/oauth2`, `x/sync` and `x/time`. A throwaway package importing all six pinned modules built for Linux, for a static Linux binary and for `darwin/arm64`. What the SDK adds to the binary is measured by the MCP work.

## Not done here

- No buffer is pooled yet and `pkg/acp` is not on v2. Both come with a before and after benchmark.
- The proxy test that runs a whole turn needs a provider and the loop.
