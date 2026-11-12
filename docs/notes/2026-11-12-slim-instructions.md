# Shorter base instructions, and conversation search off by default

2026-11-12

## Why

Three `oku-trade/trade` tasks were run through towa `a799c49`, jcode and pi 1.3.0 on Claude Opus 5.5 at medium effort, one run each, the three tasks at the same time (`compare-coding-agents`, outputs kept outside the repository):

| PR | towa | jcode | pi | Merged PR's tests |
|---|---|---|---|---|
| 4668 | 5 requests, 165k | 16, 507k | 5, 149k | all pass, the same one-line change |
| 4646 | 20, 1.07M | 25, 1.13M | 10, 359k | all fail 5 of 8 |
| 4634 | 47, 2.43M | 57, 2.62M | 19, 728k | towa fails, jcode and pi pass |

- On 4646 the five failures are `coachScrollDelta is not a function`, a helper only the merged code has. They say nothing about the three fixes.
- On 4634 towa and pi found the same cause in 12 and 11 requests. towa then changed the caller and left the `Switch` component as it was. pi changed `Switch`, as the merged PR did. towa made 34 more requests after its first edit, pi 7. About 22 of towa's went on a scratch test that rendered the whole order form.
- pi's base instructions, as its model receives them, are four lines: who it is, be concise and helpful, use the tools, show file paths. towa's had three paragraphs, including "finish the whole task, not only its first part" and "Make the smallest complete change and check the affected behavior".

Whether those sentences caused the difference on 4634 is not established. One run each, and on an easier task the instructions were worth one request at most (`2026-11-12-batch-entry.md`).

## What changed

- `agentctx.Instructions` is three sentences after the identity line: a question stops a session nobody is watching, ask only for a decision that is the user's and otherwise say which option was taken, and do not claim a result that was not observed.
- Removed: work until the task is done, fix a failure and do not stop at reporting it, do what the user means, finish the whole task, inspect before changing, make the smallest complete change and check the affected behavior, report what changed and what was checked.
- In the runs above the removed advice is not gone from the prompt. The task repository's `AGENTS.md` gives the workflow (format, lint, targeted tests, commit), and one of the rule files that towa, jcode and pi were all given says to make the smallest complete change and to validate the changed behavior. pi read those same words and still did less, so the base instructions repeating them is a candidate for the difference and no more than that.
- `conversation_search.enabled` is off by default. The tool's definition went with every request and it has something to find only after a compaction. The summary prompts already mention the tool only when it is enabled.
- The read guidance was not added, because it is there: "Look at a file with read, not with cat, head or sed in bash." has been the `read` tool's line since `09c3c84`, and the model quotes it back from the prompt on the build before this change and on this one.

## Measured

Same question to both builds through `acphost exec`, the user's rules and skills loaded, no project `AGENTS.md`, Opus 5.5:

| Build | First request | Tools offered |
|---|---|---|
| `a4d2f94` | 23,723 tokens | 11, with `conversation_search` |
| this change | 23,206 tokens | 10 |

517 tokens less on every request, for the two changes together.

How often a file was read through `bash` (`cat`, `sed -n`, `head`, `tail`) in the runs above:

| | 4634 | 4646 |
|---|---|---|
| towa | 5 of 34 `bash` calls, beside 6 `read` calls | 2 of 11, beside 5 |
| pi | 7 of 19, no `read` call | 4 of 8, no `read` call |

pi has the same kind of line ("Use read to examine files instead of cat or sed.") and never called `read`. A line in the prompt does not settle which tool is used.

## The factory write recording

`src/loop/testdata/factory_write_{1,2}.json` hold the prompt, so they were recorded again on `openai/gpt-oss-20b` with the command in `2026-11-12-prompt.md`. The first recording passed and its replay passed three times.

## Not measured

- Whether the shorter instructions change the request count or the result on a task.
- What the model does without the removed sentences where no project rule covers them: whether it stops early, or reports a failure it would have fixed.
