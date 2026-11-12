# Claude thinking is asked for, with a summary a client can show

2026-11-12

## Why

An ACP client was never shown a thought from a Claude model. `anthropic.Config.Thinking` existed and nothing set it (`docs/notes/2026-11-11-provider-anthropic.md`, "Nothing wires `Config.Thinking` yet"), so no request carried a thinking parameter.

The model thinks anyway. The stored session of a real turn on Opus 5.5 (`oku-trade/trade` PR 4672, effort medium) holds four thinking items. Each has a signature of 1,064 to 2,296 characters and no text. The loop sends an `agent_thought_chunk` only for text, so the client got none.

## Measured

Subscription login, `claude-opus-5-5` at effort medium, one question that needs working out (a snail on a wall, answer 19), through `acphost exec`. No context files or skills.

| Request | Thought text sent to the client | Output tokens | Answer |
|---|---|---|---|
| no `thinking` parameter | 0 chars | 257 | 19 |
| `{"type":"adaptive"}` | 0 chars | 240 | 19 |
| `{"type":"adaptive","display":"summarized"}` | 461 chars in 45 chunks | 225 | 19 |

One run each. The output token counts are within what one run varies by, so this shows no cost for the summary and does not rule one out.

Every Claude model in the built-in table was then sent the third form with a shorter question:

| Model | Result |
|---|---|
| `claude-opus-5-5`, `claude-sonnet-5-5`, `claude-haiku-5-5`, `claude-fable-5-2`, `claude-opus-4-8`, `claude-opus-4-7`, `claude-opus-4-6`, `claude-sonnet-4-6` | answered |
| `claude-haiku-4-5` | HTTP 400, "adaptive thinking is not supported on this model" |

## Decisions

- `claude.thinking` (default `adaptive`) and `claude.thinking_display` (default `summarized`). `off` sends no thinking parameter, as before. An empty display sends none, and the thinking then arrives without text.
- The display value is passed through as written. The API names the values and refuses one it does not know.
- `models.Metadata.NoAdaptiveThinking` marks `claude-haiku-4-5`, the one model the probe found. A request to it carries no thinking parameter. A model the table does not have is sent the configured one, and the API's 400 names the problem.
- Manual thinking with a token budget (`anthropic.ThinkingManual`) is not wired. Nothing here needs it: the one model without adaptive thinking is left without a parameter.

## Checked

With the built binary, same question:

| Setting | Model | Thought text | Result |
|---|---|---|---|
| defaults | `claude-opus-5-5` | 284 chars | 19 |
| `--claude.thinking=off` | `claude-opus-5-5` | 0 | 19 |
| `--claude.thinking_display=` | `claude-opus-5-5` | 0 | 19 |
| `--claude.thinking=off` | `claude-haiku-4-5` | 0 | answered |

`--claude.thinking=loud` is refused when the config loads: `claude.thinking: fails oneof validation (adaptive off)`.

## Limits

- Whether a summary changes what the model does on a real task, or what a turn costs, is not measured.
- The summary is the API's, not the model's full reasoning. No other display value was tried.
- Every measurement used a subscription login. An API key was not tried.
