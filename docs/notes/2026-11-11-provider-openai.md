# provider-openai lane

2026-11-11

`src/provider/openai` is the Responses API client. It implements `provider.Provider` and adds `Compact` and `Close`.

## What was built

| file | holds |
|---|---|
| `openai.go` | `Config`, `New`, endpoint selection by `Credential.Scheme`, headers, the SSE transport |
| `request.go` | the request body from `provider.Request` |
| `stream.go` | stream events into `provider.Event`s, for both transports |
| `errors.go` | classification into `*provider.Error` |
| `websocket.go` | the WebSocket transport with `coder/websocket` |
| `compact.go` | the compact call |
| `replay_test.go` | the recorder and the replay test on `providertest` |

Sources read: jcode `crates/jcode-provider-openai`, `crates/jcode-provider-openai-runtime`, `crates/jcode-base/src/auth/codex.rs`, `docs/OPENAI_WEBSOCKET_VALIDATION.md`, `docs/ANTHROPIC_OAUTH_IDENTITY_PROBE_2026_11_05.md`, and openai/codex at `ce0323f2` (`codex-api`, `model-provider`, `login`, `protocol`).

## The account id header, for protocol7

The header is `chatgpt-account-id`. The client sends it on an OAuth request when `Credential.ChatGPTAccountID` is not empty, and leaves it out otherwise.

- Where the value comes from: the `chatgpt_account_id` member of the `https://api.openai.com/auth` claim in the login's id token. jcode reads it there once at login and stores it (`extract_account_id` in `auth/codex.rs`). A refresh keeps the stored value when the grant returns no id token. openai/codex reads the same claim (`login/src/token_data.rs`).
- A proxy can therefore add it without being told: it holds the tokens, so it can read the claim. openai/codex also looks for that claim in the access token (`parse_chatgpt_account_user_id`), so a proxy holding only an access token likely can too. I did not decode a real access token to confirm.
- Whether the backend requires it: not established. jcode sends it only when it has one, and sends requests without it for tokens imported from another tool (`account_id: None` in `load_credentials`). That is code that permits the header to be absent, not a probe that the backend accepts the request. jcode's 2026-11-05 probe varied `originator` only.
- What to do: have the proxy set `chatgpt-account-id` from the claim whenever the harness did not send it, and leave a value the harness sent alone. That is correct whichever way the backend behaves. The harness side already exists: a `proxied_oauth` account has `chatgpt_account_id` in its config.

Everything else the proxy must leave alone: `originator: acpharness`, `x-codex-routing-hint: model=<model>`, `session-id: <cache key>`, and on the WebSocket handshake `OpenAI-Beta: responses_websockets=2026-02-06`. A proxied credential gets exactly these with the placeholder in `Authorization: Bearer`. The WebSocket handshake is an HTTP GET with `Upgrade`, sent through the same `*http.Client`, so the proxy must swap the authorization there too and pass the upgrade.

## Decisions

- Two provider tags. Items from the API key endpoint are tagged `Config.Provider` (default `openai`) and items from the Codex backend `openai-codex`. Encrypted reasoning and item ids are sent back only to the tag that made them. Whether one endpoint can read the other's encrypted reasoning is unknown, and a wrong guess is a rejected request. A second Responses-compatible endpoint must be given its own `Config.Provider`.
- `Opaque` for a text, thinking or tool call item holds only what the plain fields lack: the item id, the message phase, the encrypted reasoning, the summary in its parts, and the wire type of a tool call. A `Native` item's `Opaque` is the wire item unchanged. An output item of a type this client does not know becomes a `Native` item, so a server-side tool call or a compaction item survives the round trip.
- An item from another provider: text and tool calls are rendered from their plain fields without an id, thinking and native items are left out.
- A free-text tool call's input is stored as a JSON string, as the transcript contract says. A function call whose argument text is not valid JSON is also stored as a string, so that the argument repair sees what the model wrote. `Opaque.type` tells the two apart when the item goes back.
- A tool result goes back as `custom_tool_call_output` when its call was a free-text call, and `function_call_output` otherwise.
- `strict` is false on function tools. Strict mode needs every property required and no additional properties, which MCP tool schemas do not promise. jcode rewrites schemas to fit. That belongs in the dialect, not here.
- `max_output_tokens` is omitted on a Codex backend request, as jcode does. Whether the backend rejects it was not probed.
- `reasoning.summary` is `auto` whenever an effort is sent. Without summaries a long reasoning phase produces no events.
- `InputTokens` is `input_tokens` minus the cached and cache-write counts, because the transcript's count excludes the cache. Usage that reports fewer input tokens than cached ones is an error.
- Stop reasons: a response with a tool call is `tool_use`, one with a refusal part is `refusal`, otherwise `end_turn`. `response.incomplete` maps `max_output_tokens` to `max_tokens` and `content_filter` to `refusal`. Any other incomplete reason is an error, not a guessed stop.
- A stream that ends without `response.completed` is a retryable network error.
- Reasoning summary parts are joined with a blank line, in the deltas and in the finished text alike.

## Errors

| what the provider sent | class | reason |
|---|---|---|
| `context_length_exceeded`, or a 4xx whose message names the context window | fail | `context_full` |
| `usage_limit_reached` (ChatGPT plan window) | fallback | `rate_limit`, `ResetAt` from `resets_at`, or now plus `resets_in_seconds` |
| `insufficient_quota`, `usage_not_included`, the spend limit codes | fallback | `rate_limit` |
| `rate_limit_exceeded`, `slow_down`, other 429 | retry | `rate_limit`, `RetryAfter` from `retry-after-ms`, `retry-after`, or "try again in Ns" in the message |
| `server_is_overloaded`, 408, 5xx | retry | `overload` |
| 401, 403, `invalid_api_key`, `token_expired` | fallback | `auth` |
| `invalid_prompt`, the policy codes, other 4xx | fail | `request` |
| `previous_response_not_found`, `websocket_connection_limit_reached` | retry | `network` |
| a failed response with an unknown code | retry | `overload` (openai/codex retries these) |
| transport failure, cut-off stream | retry | `network` |
| cancelled context | fail | `network`, wrapping the context's error |

The codes are openai/codex's (`codex-api/src/sse/responses_error.rs`, `api_bridge.rs`). The `usage_limit_reached` body shape is from a body quoted in jcode's `openai_rate_limit_format.rs`. A usage limit is a fallback and not a retry because the reset is hours away.

The message of an error is the provider's own. An error body longer than `Config.ErrorBodyBytes` (default 1 MiB) is cut and the message says so.

## WebSocket transport

- `websocket.Dial` gets `Config.HTTPClient` as its `HTTPClient`, so the handshake uses the proxy and trust settings of every other request.
- One connection is kept per account, token, model and cache key, and at most `Config.MaxIdleSockets` in all. There is no default: the caller must set it for this transport. The connection idle longest is closed first.
- A kept connection continues from `previous_response_id` and sends only the new input items, when the request's input begins with what the server already holds. The client keeps a SHA-256 of that prefix, not the items. A rewritten history (a compaction, a model switch) does not match and is sent whole.
- A kept connection that fails before any event of the response was read is replaced once and the request sent whole. This is the server closing an idle connection.
- A request with no cache key gets its own connection, closed at the end.
- No prewarming and no permessage-deflate. jcode has both.
- The read limit is off. The library's default of 32 KiB is below one finished response.

## Compaction

- `Compact(ctx, req)` posts `model`, `instructions`, `input` and `store: false` to `/responses/compact` and returns the output items as `Native` items, with usage. A response without a compaction item is an error.
- `Config.CompactThreshold` sends `context_management: [{type: compaction, compact_threshold}]`, and the server's compaction item then arrives in the stream as a `Native` item of kind `compaction`.
- Both shapes are jcode's. openai/codex at `ce0323f2` has no client for the compact endpoint that I found, so there is no second source.

## Measurement

Encoding a request of 200 user messages of 2,000 characters, `go test -bench Encode -benchmem -count 3` on an i9-14900, in a scratch test that is not in the repo:

| | ns/op | B/op | allocs/op |
|---|---|---|---|
| `json.Marshal` | 510,631 to 592,243 | 457,249 to 458,487 | 401 |
| `json.MarshalWrite` into a pooled buffer | 469,847 to 496,658 | 39,177 to 39,364 | 400 |

The buffer is returned only after the transport has closed every reader of the body, because net/http may read a request body after the response arrives.

## Recordings from Ollama

`testdata/api_text.json`, `api_function_call.json` and `api_unknown_model.json` were recorded on 2026-11-11 from the local Ollama server (0.24.0, `llama3.2:3b`, `http://127.0.0.1:11434/v1`), through the recorder in `replay_test.go`:

```sh
ACPHARNESS_RECORD_OPENAI=1 OPENAI_API_KEY=placeholder-not-a-key ACPHARNESS_OPENAI_MODEL=llama3.2:3b \
  ACPHARNESS_OPENAI_EFFORT= ACPHARNESS_OPENAI_BASE_URL=http://127.0.0.1:11434/v1 \
  go test -count 1 -run 'TestExchange/api_(text|function_call|unknown_model)$' ./src/provider/openai/
```

Five requests were sent in all. `TestExchange` now replays the three files through the real client over HTTP, and fails if the request the client builds differs from the recorded one.

These are Ollama's emulation of the Responses API, not OpenAI. They show that the parser reads a real server's stream for a text reply and a function call, that the deltas add up to the finished text, that usage and the stop reason come out, and that a 404 with an error body becomes a `request` error of class fail. They show nothing about OpenAI's reasoning items, encrypted content, custom tools, usage-limit errors, context-length errors, rate limit headers, or the Codex backend.

What Ollama sent that is worth knowing:

- It answered the first two requests with HTTP 400, `"llama3.2:3b" does not support thinking`, because the request carried `reasoning`. The recordings were made without an effort (`ACPHARNESS_OPENAI_EFFORT=`), so no recording covers a request with `reasoning`. The client was not changed.
- Events this client does not read arrive and are skipped: `response.in_progress`, `response.content_part.added`, `response.content_part.done`, `response.output_text.done`, `response.function_call_arguments.done`.
- The function call's arguments came in one delta, so no recording covers arguments split over several deltas.
- The unknown model error is `{"error":{"message":...,"type":"not_found_error","param":null,"code":null}}` with status 404. A null `code` is read as no code.
- Usage has `cached_tokens: 0` and `reasoning_tokens: 0`. No recording covers the subtraction of cached tokens from input tokens.
- The parser needed no change.

The fixtures were searched for the host name, the user name and home paths, and hold none. They hold the URL `http://127.0.0.1:11434/v1/responses`, the model name, the prompts and a `Date` header.

## Config

`src/config/openai.go` has the `openai` section: `transport` (default `sse`), `max_idle_sockets` (no default, required for `websocket`), `compact_threshold` (default off), `parallel_tool_calls` (default false, as jcode sends) and `error_body_bytes` (default 1 MiB, a choice and not a measurement: the comment in the file gives the sizes seen). It is not wired into `config.go` yet.

## Checked

- `go build`, `go vet`, `go test -race`, `golangci-lint run` on `./src/provider/openai/...`, the `darwin/arm64` build and the `CGO_ENABLED=0` build pass.
- Scratch tests against a local `httptest` server, deleted afterwards because they feed the client events I wrote: the event sequence for reasoning, text, a function call, a custom call and a web search call, the round trip of those items into the next request on both endpoints, the headers of both endpoints, the usage-limit body, `response.failed` and `response.incomplete`, a cancelled context, the compact call, the WebSocket continuation, full replay after a rewritten history, the reconnect after a dropped idle connection, and the pooled body under `-race` through a redirect and an early error.

## Not checked

- Nothing has run against OpenAI. Every OpenAI wire shape here is read from jcode and openai/codex, not observed. The only real server the client has talked to is Ollama, for the three recordings above.
- Not recorded, and skipped by `TestExchange`: `api_custom_call`, `api_max_output_tokens`, `codex_text`, `codex_function_call`, `codex_custom_call`. They need a real account:
  - the API scenarios: `ACPHARNESS_RECORD_OPENAI=1 OPENAI_API_KEY=... ACPHARNESS_OPENAI_MODEL=... go test ./src/provider/openai -run TestExchange/api_`. This would also replace the three Ollama recordings with OpenAI's.
  - `codex_text`, `codex_function_call`, `codex_custom_call`: `ACPHARNESS_RECORD_OPENAI=1 ACPHARNESS_CODEX_TOKEN=... ACPHARNESS_CODEX_ACCOUNT_ID=... ACPHARNESS_CODEX_MODEL=... go test ./src/provider/openai -run TestExchange/codex_`
  - The recorder replaces the token and the account id.
- No recording exists for an OpenAI error response (usage limit, context length, 401), for the compact call, or for a second turn that sends encrypted reasoning back. Each needs its own scenario and a real account in that state.
- The WebSocket transport has no test in the repo and has never talked to a real server. `providertest` covers plain HTTP only.
- `x-codex-routing-hint` and `session-id` are sent because jcode and openai/codex send them. Whether the backend needs them was not probed.

## Not done

- Rate limit state from the `x-codex-*` response headers is not read. `provider.Event` has no field for it.
- Service tiers, `prompt_cache_retention`, `text.verbosity` and hosted tools other than through `Tool.Native`.
- Token refresh on 401. The client reports `auth` and the caller refreshes through `auth.Source`.
