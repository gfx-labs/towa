# A batch entry is the tool and its own arguments

2026-11-12

## Why

On a real task (`oku-trade/trade` PR 4672, Claude Opus 5.5 at medium effort, towa `9e66a35`) the model made two `batch` calls of five and two `read` entries. Every entry failed:

```
The tool call's input was not accepted: repair: schema_mismatch: validating root: unexpected additional properties ["intent"]
```

The batch schema required `intent` in each entry and the description told the model to send it. `batch` removes only `tool` from an entry and hands the rest to the inner tool. `read`, `write`, `edit`, `apply_patch`, `bash`, `glob` and `grep` set `additionalProperties: false`, so each refused the entry. The model then made the same seven reads as separate calls. That cost two requests of the turn's nine.

The existing test batched `skill_manage`, one of the extras whose schema does take `intent`, so it passed.

## Decision

- An entry requires only `tool`. The description says an entry holds the tool and that tool's own arguments, and nothing else.
- `intent` is not removed from an entry on the way through. A tool that takes it (the extras use it for the call's title) still gets it, and a tool that does not still refuses it with the error above.
- `TestBatchEntryIsTheToolAndItsOwnArguments` batches two real `read` calls through the loop's dispatcher.

## The same task after the fix

PR 4672 again, one run on each build, same model and effort:

| | `9e66a35` | `84339ad` |
|---|---|---|
| batch calls | 2, both failed | 1 of five reads, completed |
| failed tool calls | 2 | 0 |
| requests up to the edit | 6 | 4 |
| requests after the edit | 3 | 3 |
| requests and tokens | 9, 409k | 7, 322k |

Both runs made the same change to the two source files. pi 1.3.0 on this task took 4 requests up to the edit and 2 after, 238k tokens, in its one run.

## End to end

`docs/notes/2026-11-12-token-use.md` measured single requests. Whole turns, same machine and model, through `acphost exec`, one agent at a time. PR 4673 is the task that note started from.

| Agent | Runs | Requests | Tokens |
|---|---|---|---|
| towa before `b96f4df` | 1 | 11 | 668k |
| towa `53c4a2a` | 3 | 9 to 10 | 485k to 547k, mean 508k |
| pi 1.3.0 | 6 | 6 to 9 | 247k to 440k, mean 345k |
| jcode `037163c7b` | 2 | 19 to 20 | 880k to 1,078k |

- towa's tokens per request went from 61k to 54k. The fixed prompt is 31.4k against pi's 28.6k.
- The rest of the gap to pi is after the edit: towa took 4 to 6 requests there and pi 2 to 4. towa ran `lint:browser-agent` in every run and committed in half of them. Both are asked for by the task repository's `AGENTS.md`. pi did neither in any run.
- No tool call failed in the three towa runs after `b96f4df`, and the model still made partial reads with an offset. That is all the evidence there is on reading without line numbers.

## Limits

- One task, at most six runs per agent. Request counts moved by three between runs of the same agent.
- Four of the six pi runs used an adapter that moved the rules and `AGENTS.md` out of the system prompt. The two runs after replacing it gave 338k and 440k.

## towa with pi's base instructions

PR 4672, towa only, alternating its own instructions with pi's through `context.system_prompt_file`. The first run was on `de12ef6` and the other three on `619af59`. The one commit between them changes no prompt or tool text, and the first request is 31,367 tokens with pi's instructions on both builds. No run sent a thinking parameter. pi's are the four lines its model receives ("You are an expert coding assistant. Be concise and helpful. Use the available tools to answer the user's request. Show file paths clearly when working with files.") and its two general rules. Everything else was the same: tools, rules, `AGENTS.md`, skills.

| Instructions | Requests | Up to the edit | After it | Tokens | Committed | Merged PR's 2 tests |
|---|---|---|---|---|---|---|
| pi's | 5 | 3 | 2 | 207k | no | pass |
| towa's | 7 | 4 | 3 | 299k | yes | 1 fails |
| pi's | 6 | 3 | 3 | 250k | no | 1 fails |
| towa's | 8 | 5 | 3 | 380k | yes | pass |

pi itself took 6 requests (4 up to the edit, 2 after) and 238k in its one run on this task, did not commit, and failed the same one test.

- With pi's instructions towa took 5 and 6 requests, where pi took 6. With its own it took 7 and 8.
- Most of the difference is before the edit: one or two more requests reading code. After the edit both towa runs on its own instructions spent a request on the commit. One run on pi's instructions spent the same request running the tests a second time.
- The commit is step 5 of the workflow in the task repository's `AGENTS.md`. The task prompt does not ask for one. towa's instructions say to "finish the whole task, not only its first part", and pi's say nothing of the kind.
- The change itself was no better or worse in either pair: one run of each failed the same merged test, by leaving the Price chart's tooltip unformatted.
- The gap closed with no thinking parameter in any run, while pi sends adaptive thinking. So the thinking parameter is not needed to explain it. Whether sending it changes towa's count is not measured here.
- Two runs each, and the same agent has varied by two or three requests on one task. This points at the base instructions and does not establish them. The instructions were not changed.
