# Conformance: acp-go-sdk as the client

2026-11-11

`tools/conformance` checks the actual `acpharness serve` binary with `github.com/coder/acp-go-sdk` v0.13.5. It is a separate module and imports no acpharness package. Root dependencies are unchanged.

## Run

```
go -C tools/conformance vet ./...
go -C tools/conformance test -race -count=1 ./...
```

`TestMain` builds `./cmd/acpharness` once from the repository root. Building `../../cmd/acpharness` while inside the nested module is not supported by Go. `ACPHARNESS_CONFORMANCE_BINARY=<absolute path>` tests another built binary instead. `ACPHARNESS_CONFORMANCE_WIRE=1` with `-v` prints both directions of each conversation, including the raw bytes that failed a check.

Every subprocess gets private `HOME`, `XDG_CONFIG_HOME` and `XDG_STATE_HOME`, an explicit config file and a temporary working directory. Session working directories exist and belong to the test. No real token store is read, no login occurs, and no provider request is made.

## Runtime migration

The first recording checked the Stub implementation. The real runtime now opens the store and creates a session without a configured model. A prompt must fail with a structured harness request stop, not echo the prompt or claim `end_turn`. The SDK tests check the JSON-RPC error code and `data.acpharness.stop.kind`, not error wording.

The approved golden changes are the implemented runtime capabilities, removal of the Stub text/result, the typed prompt error, and a successful empty `session/list` result instead of Method not found. The list request filters to a fixed directory that no test session uses, so no new timestamp or path normalization is needed. `TestNewSession` separately lists the actual created sessions and checks their IDs and directories.

The initial runtime probe observed this untyped prompt error:

```
{"jsonrpc":"2.0","id":3,"error":{"code":-32603,"message":"Internal error","data":{"error":"loop: resolving model \"\": models: no model is configured"}}}
```

That untyped error does not satisfy the migrated tests. Commit `089ad34` maps the model-selection error to a request stop. The actual binary now sends:

```
{"jsonrpc":"2.0","id":3,"error":{"code":-32603,"message":"Internal error","data":{"acpharness":{"stop":{"kind":"request","message":"models: no model is configured"}},"error":"turn stopped: request: loop: resolving model \"\": models: no model is configured"}}}
```

The golden was regenerated only after coordinator review of the capability/error/list delta and a passing typed-error transport check.

## SDK decoding findings

The runtime initializer sends these capability bytes:

```
{"auth":{},"loadSession":true,"mcpCapabilities":{"http":true,"sse":true},"promptCapabilities":{},"sessionCapabilities":{"additionalDirectories":{},"archive":{},"close":{},"delete":{},"fork":{},"list":{},"options":{},"resume":{},"status":{},"systemPrompt":{"mode":{"override":{}}},"title":{}}}
```

SDK 0.13.5 drops exactly these five members:

- `agentCapabilities.sessionCapabilities.archive`
- `agentCapabilities.sessionCapabilities.options`
- `agentCapabilities.sessionCapabilities.status`
- `agentCapabilities.sessionCapabilities.systemPrompt`
- `agentCapabilities.sessionCapabilities.title`

The store also sends list entries shaped as:

```
{"archived":false,"cwd":"<test directory>","sessionId":"<session ID>","updatedAt":"2026-11-11T15:16:14Z"}
```

The SDK drops `sessions[].archived`. These losses are explicit assertions and reported findings, not alternate decoders or suppressed fields. Raw results remain available in failure output. No request or notification decode failure was observed during the initial runtime probe.

Other SDK details remain unchanged:

- Empty client capabilities encode as `{"auth":{},"fs":{}}`.
- A new-session request missing required members encodes as `{"cwd":"","mcpServers":null}`. The agent rejects it with `-32602`.
- Unset prompt content annotations are omitted.
- SDK error-level logs fail the tests, since the SDK can report notification decode errors only to its logger.

## Coverage

| test | contract |
|---|---|
| `TestInitialize` | protocol version negotiation, identity, runtime capabilities and the exact five dropped capability members |
| `TestNewSession` | modes/config options, distinct session IDs, stored-session listing and the exact dropped archived field |
| `TestPrompt` | typed no-model failure without text output, command announcement once, continued serving, unknown session `-32002` |
| `TestCancel` | idle and unknown-session cancellation are unanswered notifications, connection still serves requests |
| `TestClose` | close succeeds, subsequent prompt is `-32002` |
| `TestSetMode` | matching mode/config updates, redundant update omitted, invalid mode `-32602` |
| `TestSetConfigOption` | option result and current-mode update, invalid value/option `-32602` |
| `TestMethodNotFound` | `providers/list` and unknown extension request `-32601`, unknown extension notification unanswered |
| `TestInvalidParams` | required members, wrong types and non-object params rejected `-32602` |
| `TestWire` | all agent-written frames in one conversation, compared as JSON with the reviewed recording |
| `TestSocketClients` | actual binary, two concurrent Unix clients and one TCP client, separate sessions, cross-owner rejection, typed model-selection failures, continued serving after one client closes, daemon survives stdio EOF, SIGTERM closes remaining clients and removes Unix socket |

Session creation sends `available_commands_update` after its response. Tests do not require the client to observe that notification before its `NewSession` call returns. Subsequent protocol requests provide the observation boundary before checking collected updates.

The CLI's existing stdio acceptance test also uses private state and a real working directory. It checks the typed no-model stop through the public harness type and verifies that no agent text chunk was emitted.

## Golden and encoder changes

`TestWire` compares complete frames as JSON. Member order and whitespace do not count, but omitted members, null, empty lists and empty objects do. It normalizes only session IDs, the build version, and `error.data.error` text, which json v2 deliberately varies for decode errors. The typed stop kind remains literal.

After approval of an intentional wire change:

```
ACPHARNESS_CONFORMANCE_UPDATE=1 go -C tools/conformance test -run '^TestWire$' ./...
git diff -- tools/conformance/testdata/wire.golden
```

Wrong-type requests use the SDK's generic `SendRequest` because its typed methods cannot construct those invalid inputs. SDK types are encoded with the SDK's own v1 custom codecs under the `sdkjson` alias. Other JSON uses v2.

## Limits

- No successful real-runtime binary model turn is covered. Existing loop recordings have different system prompts/tool schemas and cannot truthfully stand in for the full CLI request. No external recording was authorized for this task.
- No running turn cancellation, permission request, compaction, retry, subagent or model-generated update is exercised by the no-model conversations.
- Archive/status/options/title and system-prompt extensions have no corresponding SDK 0.13.5 typed interface. Their advertised presence is recorded, not adapted around.
- Store load/resume/fork/delete methods are advertised but not exercised by this module. Listing is exercised.
- The SDK socket test uses loopback plaintext TCP. Real TLS/mTLS, certificate rotation and partial-line disconnects are covered by `src/listen` tests, not this SDK module.
- No TypeScript SDK, Zed or Rust client was checked.
- No macOS runtime check was performed. Darwin builds do not prove Unix socket path-length, permissions or flock behavior on macOS.

## Verification status

Checks after `089ad34` and the approved golden update:

```
go -C tools/conformance test -race -count=3 ./...
go -C tools/conformance test -race -count=40 -run '^(TestWire|TestNewSession|TestPrompt|TestSocketClients)$' ./...
go test -race -count=3 -run '^TestServeAnswersAClientOnStdio$' ./src/cli
go -C tools/conformance vet ./...
```

All passed. The module lint reported zero issues. `TestSocketClients` also passed ten runs against a separately race-built real agent, with the SDK test process race-instrumented too. Listener/config race tests passed three runs. CLI/listener Darwin arm64 and Linux CGO-disabled builds passed. No new test file or scaffolding was added by this migration. Existing protocol, socket and CLI acceptance tests were retained and updated.

A broader full-CLI race run found an unrelated in-progress `TestRuntimeRetainsSemanticEmbeddingAcrossTurns` failure at `src/cli/runtime_compact_test.go:105`: the Codex `apply_patch` native tool requires Responses but the test selected Chat Completions. This prevented its expected main HTTP requests and embedding replay. The compaction owner was notified. The scoped CLI no-model test passes.
