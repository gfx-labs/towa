# provider-anthropic: the Messages API client

2026-11-11

`src/provider/anthropic` implements `provider.Provider` for the Anthropic Messages API, for API keys and Claude subscription OAuth tokens. Nothing in it has been sent to Anthropic: there is no recording yet (see "Not done").

## What was built

- `anthropic.go`: `Config`, `New`, `Stream`, the headers and the endpoint.
- `request.go`: the wire body from a `provider.Request`.
- `stream.go`: server-sent events into `provider.Event`s.
- `errors.go`: classification into `*provider.Error`.
- `replay_test.go`: the record and replay scenarios, and two tests that need no recording.

## Request

- Neighbouring items of one role share a message, because the API wants a tool result in the message right after its call.
- `max_tokens` is required by the API. A request with `MaxOutputTokens` of zero is an error of class Fail. The contract says zero is "not sent", which this API cannot do: the caller has to take the value from model metadata.
- Thinking is a per-model choice (adaptive, manual with a budget, or off) and is model metadata, which this package does not hold. `Config.Thinking` is a function from the model name to the parameter. `Request.Effort` is sent as `output_config.effort` when it is not empty.
- A `Thinking` item is sent only when `Item.Provider` is this client's and `Opaque` holds a signature or redacted data. `Opaque` is `{"signature":...}` or `{"redacted_data":...}`. The text is the item's own `Text`. Thinking from another provider is left out. A `Native` item from this provider is sent as the block it arrived as. One from another provider is left out. `Text` and `ToolCall` items are rendered from their plain fields whoever produced them.
- A tool call whose `Input` is a JSON string (a free-text tool of another provider, or JSON the model left unfinished) is sent as `{"input": "<the text>"}`, because `tool_use.input` has to be an object.
- A tool result is `is_error` for every outcome but `completed`.
- A `Stored` part is left out. Audio is an error. A resource is text, an image or a PDF, else an error. A link is sent as markdown text.
- `Tool.Native` is sent unchanged. The betas such tools need go in `Config.Betas`.

### Cache breakpoints

Four, which is the API's limit: the last tool acpharness defined, the last system block, the end of the last message and the end of the user message before it. jcode marks the last two assistant messages. Here the second mark sits where the previous request ended, so that a turn with more than twenty blocks of tool calls still finds the earlier entry (the API looks back twenty blocks from a mark). A native tool cannot carry a mark, since it is sent unchanged, so the tools mark is on the last tool that is not native. `Config.CacheTTL` is `5m`, `1h` or empty. `Request.CacheKey` is not used: this API has no cache key.

Not measured: no cache read has been observed, because nothing was sent.

## Stream

- Text and thinking deltas, tool call progress (one event with the id and name at the start, then one per argument piece), and a finished item when each content block stops, in block order.
- Usage is yielded at `message_start` and again at `message_delta`. Counts the later event leaves out keep their value. `input_tokens` already excludes cache reads and writes, as `transcript.Usage` wants. `ThoughtTokens` stays zero: the API does not report it.
- A block type this client does not know (`server_tool_use`, `web_search_tool_result`, later additions) becomes a `Native` item whose `Opaque` is the block. The input of a server tool's use arrives as deltas and is put back into the block.
- A tool call with no argument text has the input `{}`. Argument text that is not a JSON object, which a response cut at its token limit leaves, is kept as a JSON string.
- Stop reasons: `end_turn` and `stop_sequence`, `tool_use`, `max_tokens`, `refusal`, and `pause_turn` as `provider.StopPaused`. `model_context_window_exceeded` is an error with `ReasonContextFull`. Any other is an error of class Fail.

## OAuth shape

Selected by `Credential.Scheme == auth.SchemeOAuth`. Read from jcode 037163c7b (`crates/jcode-provider-anthropic/src/lib.rs`, `jcode-provider-core/src/anthropic.rs`, `jcode-provider-anthropic-runtime/src/lib.rs`, `jcode-base/src/provider/anthropic.rs`) and `docs/ANTHROPIC_OAUTH_IDENTITY_PROBE_2026_11_05.md`.

| piece | value |
|---|---|
| URL | `/v1/messages?beta=true` |
| first system block | `x-anthropic-billing-header: cc_version=2.1.280; cc_entrypoint=jcode; cch=33f85;`, with no cache mark |
| `Authorization` | `Bearer <token>` |
| `anthropic-beta` | `oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,prompt-caching-scope-2026-01-05,advisor-tool-2026-03-01,advanced-tool-use-2025-11-20,effort-2025-11-24`, plus `thinking-binding-controls-2026-08-01` when `Thinking.DropMismatchedBlocks` is set |
| `User-Agent` | `acpharness/<version>` |
| `Accept` | `application/json` |
| `x-client-request-id` | a new UUIDv4 per request |

No identity sentence, no `x-app`, no `X-Stainless-*`, no session header, no tool renaming. The client adds no text that names the agent: the system prompt is the caller's (`agentctx`), and it names acpharness.

**`cc_entrypoint` needs a live probe.** It is the constant `anthropic.BillingEntrypoint`. The checked-out jcode commit 037163c7b sends `jcode`, which is the value used. The probe document ran with `sdk-cli`, which is also what origin/master 8e11eb23e sends. No request from acpharness with either value has reached Anthropic. Before relying on it, record `oauth_text` on Opus or Sonnet (Haiku accepts anything), and if it answers 429 try `sdk-cli`.

Differences from jcode's OAuth request, all untested here:

- jcode sends `metadata.user_id` (device id, account uuid, session id), reading the first two from the official Claude client's files. acpharness reads no other tool's files and sends no `metadata`. The probe script sent none and was accepted.
- jcode sends `temperature: 1.0` on OAuth requests without thinking. It is not sent here, on the understanding that 1.0 is the API's default.
- jcode adds `context-1m-2025-08-07` for a model name ending in `[1m]`. Here that beta goes in `Config.Betas` when the caller wants it. The model name is sent as given.

An API key request sends `x-api-key`, `Accept: text/event-stream`, no billing block, and an `anthropic-beta` header only for thinking and `Config.Betas`. jcode's `prompt-caching-2024-07-31` beta is not sent, on the understanding that prompt caching no longer needs it. The `api_` recordings will show whether cache tokens are reported.

### Proxied credentials

The package never reads `Credential.Proxied`. `TestProxiedCredentialGetsTheSameRequest` sends the same request with and without the flag to a local server and compares headers, query and body, for both schemes. The request id is left out of the comparison because it is random.

## Errors

| case | class | reason |
|---|---|---|
| 401, 403, `authentication_error`, `permission_error`, an empty token | Fallback | auth |
| 429, `rate_limit_error` | Retry | rate_limit, with `RetryAfter` from `retry-after` and `ResetAt` from the reset headers |
| 402, `billing_error`, "credit balance is too low" | Fallback | rate_limit |
| 529, `overloaded_error` | Retry | overload |
| 400 or `invalid_request_error` whose message says the prompt is too long, and the stop reason `model_context_window_exceeded` | Fail | context_full |
| other 5xx, `api_error`, `timeout_error` | Retry | overload |
| 408 | Retry | network |
| the request did not get through, the stream was cut off before `message_stop` | Retry | network (Fail when the context was cancelled) |
| a 200 that is not an event stream, an event that is not JSON, blocks out of order | Fail | network |
| anything else | Fail | request |

An `error` event mid-stream goes through the same classification with status zero.

`ResetAt` reads `anthropic-ratelimit-unified-reset` (Unix seconds, subscriptions), else the latest `anthropic-ratelimit-<requests|tokens|input-tokens|output-tokens>-reset` (RFC 3339) among the limits whose `-remaining` is `0`. These header names and formats were written from what is known of Anthropic's documentation. They were not looked up again and are not from a recording, so they need checking against a real 429. jcode's crates only mention the `anthropic-ratelimit-unified-` prefix.

Context length has no error type: the match is on message text ("prompt is too long", "exceed context limit", "context window"). The phrases are unverified against a recording.

The probe found that an OAuth request the server does not accept as Claude Code is answered with 429 `rate_limit_error`. This client reports it as a rate limit like any other, since the response does not distinguish them.

## Checks

```
nice -n 10 go build ./src/provider/anthropic/... && go vet ./src/provider/anthropic/...
nice -n 10 go test -race ./src/provider/anthropic/...
nice -n 10 golangci-lint run ./src/provider/anthropic/...
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build ./src/provider/anthropic/...
CGO_ENABLED=0 go build ./src/provider/anthropic/...
```

All pass. The ten Anthropic `TestExchange` scenarios skip, because their recordings do not exist. The three `ollama_` scenarios replay (see below). While writing, a throwaway test fed the stream example from Anthropic's documentation (thinking with a signature, redacted thinking, text, a server tool use and result, a tool call, usage) through the client over HTTP, sent the items back, and tried each error status. It behaved as described above and was deleted: it was a hand-written stand-in, which the repo does not keep.

The request body is encoded into a `bytebufferpool` buffer for the length of the request. No benchmark: there was never an unpooled version to compare with.

## Recordings from Ollama

Three recordings were made from the local Ollama server (0.24.0, `llama3.2:3b`, `http://127.0.0.1:11434/v1/messages`) with three requests, as `docs/notes/2026-11-11-parallel-build.md` allows: `ollama_text`, `ollama_tool_call` and `ollama_unknown_model`. The command is at the top of `replay_test.go`. The parser read all three without a change.

They are Ollama's emulation of the Messages API. They show that the client's request is accepted by a real server and that the parser reads a real server's stream: text deltas adding up to the finished text, a tool call with its progress and input, usage at the start and at the end, the stops `end_turn` and `tool_use`, and a 404 `not_found_error` body classified as `request`. They show nothing about thinking blocks and signatures, cache usage fields, the OAuth billing block and beta headers, 429 and 529 responses, or reset headers. All of those stay untested until an Anthropic account is supplied. The OAuth shape was not sent to Ollama.

Seen in them: Ollama's `message_delta` repeats `input_tokens` with a larger value than `message_start` gave (171 after 61), and the later value is the one reported. The recordings are marked `incomplete` because the client stops reading at `message_stop`, before the end of the body. The files hold no host name, user name or home path (checked with grep), and the `x-api-key` placeholder is replaced.

## Not done

- **Recordings from Anthropic.** None exist. `ANTHROPIC_API_KEY` and `ACPHARNESS_CLAUDE_OAUTH_TOKEN` were not set, and no token store was read. Missing files, all under `src/provider/anthropic/testdata/`: `api_text`, `api_tool_call`, `api_thinking`, `api_max_tokens`, `api_rejected_credential`, and the same five with the `oauth_` prefix. The commands are at the top of `replay_test.go`. Not covered by any scenario, because they cannot be provoked on demand: a real 429 with its reset headers, a 529, a context-length rejection, a mid-stream `error` event, a cut-off stream, redacted thinking, a server tool.
- **The `cc_entrypoint` probe**, above.
- `pause_turn` is mapped to `provider.StopPaused` but has never been seen in a response: it needs a server tool that runs long.
- Citations (`citations_delta`) are dropped. Text with citations is sent back as plain text.
- No repair of a broken history (a tool call with no result, a duplicated result). jcode does a lot of this. Here the loop's guarantee that every call gets a result is relied on, and the API's 400 is reported as `request`.
- Server-side context management (`context_management`) and the `speed` and `service_tier` parameters are not sent.
- Model metadata (which thinking mode, max output, 1M context) is the caller's. Nothing wires `Config.Thinking` yet.
