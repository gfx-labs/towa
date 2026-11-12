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
- `models.Metadata.NoAdaptiveThinking` marks `claude-haiku-4-5`, the one model the probe found. A request to it carries no thinking parameter. A model with no row is read from the row its name points to (`docs/notes/2026-11-12-model-guess.md`): `claude-haiku-4-5-20251001` got the HTTP 400 until it was. A model no row speaks for is sent the configured parameter. No such model was sent to the API.
- Manual thinking with a token budget (`anthropic.ThinkingManual`) is not wired. Nothing here needs it: the one model without adaptive thinking is left without a parameter.

## Checked

With the binary built from `830580f`, same question, one run each:

| Setting | Model | Thought text | Result |
|---|---|---|---|
| defaults | `claude-opus-5-5` | 341 chars in 39 chunks | 19 |
| `--claude.thinking=off` | `claude-opus-5-5` | 0 | 19 |
| `--claude.thinking_display=` | `claude-opus-5-5` | 0 | 19 |
| defaults | `claude-haiku-4-5` | 0 | answered, no error |
| defaults, the question followed by a `read` and a `bash` call | `claude-opus-5-5` | 533 chars in 51 chunks | 19, both tool calls completed, 2 requests |

- In the last row the thinking is stored with its text and signature and sent back in the second request. The API accepted it.
- The probe build that sends the parameter to every model still gets the HTTP 400 from `claude-haiku-4-5`, so the table's mark is what lets the default work there.
- `--claude.thinking=loud` is refused when the config loads: `claude.thinking: fails oneof validation (adaptive off)`.

## On a real task

`oku-trade/trade` PR 4672 on `830580f`, towa only, Opus 5.5 at effort medium, alternating the default with `--claude.thinking=off`:

| Thinking | Requests | Tokens | Last request | Thought text | Committed | Merged PR's 2 tests |
|---|---|---|---|---|---|---|
| adaptive, summarized | 8 | 376k | 54.2k | 2,200 chars | yes | pass |
| off | 6 | 251k | 47.9k | 0 | no | 1 fails |
| adaptive, summarized | 6 | 274k | 53.8k | 1,513 chars | no | pass |
| off | 6 | 257k | 48.8k | 0 | no | pass |

- A client is shown thought text on a real task.
- The request counts overlap (8 and 6 against 6 and 6), and the same agent has varied by two on this task with nothing changed. Two runs a side do not show that thinking adds requests, and do not rule out one.
- The last request was about 5k tokens larger in both runs with thinking. Earlier runs with no parameter ended between 48k and 55k, so this is not established either.

## Limits

- Two runs a side on one task is all there is on what the summary costs.
- The summary is the API's, not the model's full reasoning. No other display value was tried.
- Every measurement used a subscription login. An API key was not tried.
