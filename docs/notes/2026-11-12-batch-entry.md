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
- Why the same model checks more under towa's prompt than under pi's is not tested. towa's instructions say to finish the whole task and check the affected behavior. pi's say to be concise.
- Four of the six pi runs used an adapter that moved the rules and `AGENTS.md` out of the system prompt. The two runs after replacing it gave 338k and 440k.
