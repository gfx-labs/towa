# provider-chat: Chat Completions client and providertest

2026-11-11

## What was built

- `src/provider/providertest`: `Recorder` and `Replay`.
- `src/provider/chat`: the Chat Completions streaming client, implementing `provider.Provider`.
- `src/config/providers.go`: the provider fields the client reads, with validation.

## providertest

`Recorder` wraps an `http.RoundTripper` and writes one exchange to a JSON file when the response body reaches its end or is closed. It keeps each `Read` of the body as one chunk. A second request through the same recorder is an error, so a retry cannot overwrite a recording.

Secrets: a header or query parameter whose name contains `auth`, `cookie`, `key`, `token`, `secret`, `password`, `credential`, `session` or `signature` is replaced with `REDACTED`. Every value listed in `Secrets` is replaced wherever it appears, including in bodies. If a listed secret is split across two chunks of the response it cannot be replaced, and the recording is not written.

`Replay` serves a recording from an `httptest.Server`. It fails the test when the method, path, query, headers or body differ from the recording, and when no request arrives. JSON bodies are compared as JSON. A redacted value only has to be present. The host is not compared. Headers the transport adds after the recorder (`Accept-Encoding`, `Content-Length`) are skipped. Chunks are written with a flush after each. A recording that ended in a read error is replayed by dropping the connection.

It covers plain HTTP only, not a WebSocket.

## Chat client decisions

- The system blocks are joined into one `system` message. Chat Completions has no cache breakpoints, so the names are not used.
- Thinking, text and tool calls that follow one another become one assistant message.
- A model's reasoning goes back only to the provider that wrote it, in the member it arrived in (`reasoning`, `reasoning_content` or `reasoning_details`). The item's `Opaque` records which. Reasoning from another provider is left out.
- OpenRouter `reasoning_details` pieces are merged by `index`, with `text`, `summary` and `data` concatenated, and sent back as merged.
- Tool call arguments that are not valid JSON are stored in a JSON string, as `transcript.ToolCall.Input` allows, and go back to the model unquoted.
- A tool result with an image, audio or binary resource is an error. A tool message is text, and a user message placed between tool results is rejected by endpoints.
- A `Stored` part is left out. A part with nothing set is an error.
- `stream_options.include_usage` is sent to every endpoint except OpenRouter, which documents it as having no effect.
- `Usage.InputTokens` is `prompt_tokens` minus cached and cache write tokens, because the contract says it excludes them. A negative result is an error, not clamped.
- Cost is set only for OpenRouter, from `usage.cost`, in USD.
- The cache key member is chosen by `cache_key_field` (`prompt_cache_key` or `session_id`) and nothing is sent when it is empty, since an endpoint may reject an unknown member.
- `X-Title` is sent as the brief says. OpenRouter now documents `X-OpenRouter-Title` and still accepts `X-Title`.

## Error classes

| case | class | reason |
|---|---|---|
| context length (code, `error_type` or message) | fail | `context_full` |
| 401 | fallback | `auth` |
| 403 with `metadata.limit_source` (spent budget) | fallback | `rate_limit`, `ResetAt` from `metadata.resets_at` |
| 403 otherwise | fallback | `auth` |
| 402 with `Retry-After` | retry | `rate_limit` |
| 402 without | fallback | `rate_limit` |
| 429 | retry | `rate_limit` |
| 408, 5xx, 529 | retry | `overload` |
| `content_policy_violation`, `refusal` | fail | `request` |
| other 4xx | fail | `request` |
| error event in a 200 stream | by `error.code`, then `metadata.error_type`, else retry `overload` | |
| no response, or stream cut off | retry | `network` |
| the caller's context ended | fail | `network` |
| response that cannot be parsed, unknown finish reason | retry | `network` |

`Retry-After` is read as seconds or an HTTP date. `X-RateLimit-Reset` is read only for OpenRouter, as milliseconds since the epoch, because other providers use the name with other units. A header value that cannot be read is put in the error's message.

## Buffer pooling: not used

Measured with a scratch benchmark (40 turns of history, about 18 kB of body), `go test -run '^$' -bench . -benchmem -count=3 ./src/provider/chat/`, i9-14900:

| | ns/op | B/op | allocs/op |
|---|---|---|---|
| build and encode the whole body | 153450 | 173589 | 617 |
| encode only, `json.Marshal` | 73176 | 18773 | 3 |
| encode only, pooled `json.MarshalWrite` | 71166 | 325 | 2 |
| parse one stream event | 1220 | 304 | 4 |

Pooling the encoded body would save one 18 kB allocation of the 174 kB a request allocates, and no time. It is not done: the transport can still be reading the request body after `Do` returns a response, so `Put` at that point would break the rule that no reference survives it. The per-event path has no buffer of its own to pool, because the SSE decoder reuses its event.

## Tests and what they rest on

- Request building: against the documented shapes, as JSON.
- Classification: bodies quoted from OpenRouter's errors and limits documentation, and one OpenAI context length body. Each case names its source.
- A cut-off stream and a cancelled one: a real server that drops the connection.
- Config: a TOML file through `config.Load` to the request body.
- Replay: three recordings from Ollama 0.24.0 (`llama3.2:3b`) on the build machine, made with `ACPHARNESS_RECORD_OLLAMA=1 go test ./src/provider/chat -run TestRecord -count=1`. Four requests were sent.

The Ollama recordings show that the client reads a real server's Chat Completions stream: text deltas, a tool call, the usage chunk, `[DONE]`, and a 404 error body. They show nothing about OpenRouter. Ollama sent the tool call's arguments in one delta, so assembling arguments from several deltas has no recorded test.

## Not done, not tested

- No OpenRouter recording exists: `OPENROUTER_API_KEY` was not set. `TestRecord` writes `testdata/openrouter_{text,tool_call,reasoning,unknown_model}.json` when it is, and `TestReplay` skips them until then.
- Untested against a real response: OpenRouter routing preferences, the `reasoning` object, `reasoning_details` round trip, `usage.cost`, cached token counts, rate limit headers, mid-stream error events, tool call arguments split over deltas, `reasoning_content` from DeepSeek or z.ai.
- Prompt cache breakpoints (`cache_control`) for Anthropic models through OpenRouter are not sent.
- Model metadata from `/models` is not fetched here.
