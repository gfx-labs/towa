# Intent and comprehensive change in the instructions

Date: 2026-11-13.

## Change

Two lines from jcode's system prompt (`crates/jcode-base/src/prompt/system_prompt.md` at 037163c7b) go into `agentctx.Instructions`, after the line about the user's instructions:

> Accomplish the user's intent over the literal wording. Given a task, be comprehensive.

pi's base prompt has only tool and edit rules, so it gave no candidate.

`TestFactoryWriteProcess` and its two recordings are removed. They replayed exact request bodies, so every prompt change needed a re-recording against a live model (user, 2026-11-13).

## Why

Opus 5.5 at medium effort, one run per agent on trade PRs 4668, 4672, 4673 and 4679 (`eval23-*`, `eval24-*`):

| Agent | Requests | Tokens | Merged-PR tests |
|---|---|---|---|
| towa 8c1ae90 | 32 | 1.31M | 11 of 14 |
| pi | 30 | 1.18M | 12 of 14 |
| jcode 037163c7b | 45 | 2.04M | 12 of 14 |

jcode was the only agent to pass 4672's tooltip test. It applied the date formatter to the sibling `PriceWrapper` tooltip as well.

## Measured

Two runs each on 4672 and 4679 (`eval23-*` as run 1, `eval25-*`). The eval25 runs went in parallel, so wall time is not compared.

| PR 4672 + 4679 | Tests | Requests | Tokens |
|---|---|---|---|
| towa with the lines (64669289) | 7 of 10 | 40 | 1.60M |
| towa master 8c1ae90 | 5 of 10 | 42 | 1.62M |
| pi | 6 of 10 | 34 | 1.29M |

- 4672: master and pi passed 2 of 2 on their second run, so the first failure was run-to-run noise. Both branch runs passed.
- 4679: one branch run passed "leaves focus alone when the popover did not hold it" by refocusing the trigger only when focus was inside the popover. Both master runs failed it. Every run of every agent failed the footer CSS test.

The gain is one test in one run at no extra cost, which two runs cannot separate from chance. Recheck with more runs if accuracy regresses.
