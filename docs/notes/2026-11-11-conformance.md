# Conformance: acp-go-sdk as the client

2026-11-11

`tools/conformance` checks what `acpharness serve` puts on the wire with a client we did not write: `github.com/coder/acp-go-sdk` v0.13.5. It is its own module, so the root `go.mod` gains nothing. Its tests import nothing of acpharness.

## The command for CI

```
go -C tools/conformance vet ./... && go -C tools/conformance test -race ./...
```

It builds `./cmd/acpharness` from the checkout itself, so it needs no earlier build step. It downloads the SDK, testify and `go.yaml.in/yaml/v3` (testify's) through the module proxy.

Two variables, for use by hand:

- `ACPHARNESS_CONFORMANCE_BINARY=<path>` tests that binary and builds none. This is how the binary of another branch is checked against the same tests and the same recorded file.
- `ACPHARNESS_CONFORMANCE_WIRE=1` with `-v` prints both directions of every conversation.

## How it works

- `TestMain` runs `go build -o <temp>/acpharness ./cmd/acpharness` in the repository root, once. The task said `go build ... ../../cmd/acpharness` from the module directory. That fails, because the package is outside the module `go` is run in, so the build's working directory is the root.
- Each test starts `acpharness --config <temp>/config.toml serve` with `XDG_CONFIG_HOME`, `XDG_STATE_HOME` and `HOME` inside its temp directory, and attaches `acp.NewClientSideConnection` to its pipes. The config file sets only the stderr log level.
- Both directions are copied as they pass, so a failure prints the exact bytes.
- The SDK reports a line it cannot parse and a notification it cannot decode only to its logger. The tests install a logger and fail on any error record.
- `notDecoded` compares the agent's raw result with what the SDK's type holds after decoding and encoding again. It lists the members the SDK dropped.
- The agent ends when its standard input closes. Every test checks that it exits without an error.

## Findings

Run against master at 7344c83 plus the working tree of 2026-11-11 14:00 UTC, which encodes with `encoding/json` v1.

**Nothing failed to decode in either direction.** Every request the SDK sent was accepted or answered with the expected error, and every response and notification the agent sent was decoded by the SDK.

One loss, which is expected. The SDK has no field for three members of the `initialize` result, so a client built on it does not see them:

```
{"jsonrpc":"2.0","id":1,"result":{"agentCapabilities":{"auth":{},"mcpCapabilities":{},"promptCapabilities":{},"sessionCapabilities":{"close":{},"options":{},"status":{},"title":{}}},"agentInfo":{"name":"acpharness","version":"v0.0.0-20261111135826-32713bfbfd05+dirty"},"authMethods":[],"protocolVersion":1}}
```

Dropped: `agentCapabilities.sessionCapabilities.options`, `.status` and `.title`. They are the capabilities of the unstable methods below. `TestInitialize` asserts exactly this list, so a new dropped member fails it.

Three things the SDK sends that are worth knowing. The agent accepts all of them:

- An empty `ClientCapabilities` goes out as `"clientCapabilities":{"auth":{},"fs":{}}`.
- A `NewSessionRequest` without its required members goes out as `{"cwd":"","mcpServers":null}`. The SDK does not validate what it sends. The agent answers `-32602` with `"mcpServers is required"`.
- An unset prompt content annotation is left out, not sent as null.

## What is covered

| test | what it checks |
|---|---|
| `TestInitialize` | protocol version 1, also when the client asks for 9999. `agentInfo`. No auth methods. No `loadSession`, prompt or MCP capability. `close` advertised, and `list`, `resume`, `fork`, `delete`, `additionalDirectories` not |
| `TestNewSession` | modes `auto` and `ask` with `auto` current. The `mode` config option as a select with category `mode` and the same two values. No update is sent. Two sessions get two ids |
| `TestPrompt` | `available_commands_update` (with `mode`) then one `agent_message_chunk` holding the prompt's text, then `end_turn`. A second prompt sends no commands again. An unknown session is `-32002` |
| `TestCancel` | `session/cancel` for a session with no turn running, and for an unknown session: no answer, and the next prompt works |
| `TestClose` | `session/close`, then a prompt on that session is `-32002` |
| `TestSetMode` | `current_mode_update` and `config_option_update` carry `ask`. Setting the same mode again sends nothing. An unknown mode is `-32602`, an unknown session `-32002` |
| `TestSetConfigOption` | the response carries the options with the new value, and only `current_mode_update` is sent. An unknown value or option is `-32602` |
| `TestMethodNotFound` | `-32601` for `session/list`, `providers/list` and an unregistered `_` extension method. An unregistered extension notification gets no answer |
| `TestInvalidParams` | `-32602` for a missing required member, a member of the wrong JSON type, params that are a list, and a prompt that is a string |
| `TestWire` | every line the agent writes in one conversation of nine requests, compared with `testdata/wire.golden` |

`TestWire` is the check for the move of `pkg/acp` to `encoding/json/v2`. The other tests pass as long as the SDK can decode, and the SDK decodes `null`, `[]` and a missing member alike. `TestWire` compares each line as JSON with the recorded one, so it fails when a member appears, disappears or changes between `null`, `[]` and `{}`. Member order and spacing do not count. Before comparing, session ids, `agentInfo.version` and the text in `error.data.error` are replaced with placeholders: the first two differ per run, and v2 changes the text of a decode error between runs on purpose.

To accept a change of output that is meant: `ACPHARNESS_CONFORMANCE_UPDATE=1 go -C tools/conformance test -run '^TestWire$' ./...`, then review the diff of the golden file.

The tests in the wrong-type cases send through the SDK's `acp.NewConnection` and `acp.SendRequest`, because its typed calls cannot build such a request.

## Left out, and why

- `session/status`, `session/options` and `session/set_title`: SDK 0.13.5 has no method or constant for them. They could be sent through `SendRequest` as untyped JSON, but then nothing of the SDK checks the answer, which is what this module is for.
- Cancelling a running turn. The stub turn answers before a cancel can arrive, so the `cancelled` stop reason and `$/cancel_request` are not reached. This needs a turn that waits.
- `session/request_permission`. The stub turn calls no tool, so the agent never asks.
- The gated update types (`plan_update`, `notice`, compaction, subagents). The stub sends none, and the SDK has no capability field to turn the last three on.
- `authenticate`, `logout`, `session/load` and the other store methods beyond `session/list`. They answer an error today. One method per error path is covered.
- Image, audio and embedded resource prompt blocks. None is advertised.
- A Unix socket or TCP listener. Only stdio is served on master.

## Not checked

- The `acp-layer` branch. Its worktree at 703cb74 has no file importing `encoding/json/v2` yet, so there is nothing to compare. When it has: build its binary and run the command above with `ACPHARNESS_CONFORMANCE_BINARY` set to it.
- Any other client: the TypeScript SDK, Zed, or the Rust crate.
- macOS. The module was not built for darwin.
- CI itself. The command was run on this machine only.

## Checks run

```
go -C tools/conformance vet ./...
go -C tools/conformance test -race -count=3 ./...
go -C tools/conformance test -race -count=40 -run '^(TestWire|TestSetMode|TestPrompt)$' ./...
golangci-lint run ./...        # in tools/conformance, with the repository's config
```

All pass, and lint reports no issues. The 40 runs are of the tests that depend on the order of notifications. The root `go.mod` and `go.sum` are unchanged.

`encoding/json` v1 is imported in the two test files under the alias `sdkjson`, with a `nolint` for the lint rule that forbids it. The SDK's types have v1 `MarshalJSON` and `UnmarshalJSON` methods, so they are coded with the encoder the SDK itself uses. Everything else in the module uses v2.
