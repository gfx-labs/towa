# No glob or grep tool, and web search off by default

2026-11-12

## Why

pi's prompt was captured from a real request and set beside towa's (`85e52ca`), both built in the same `oku-trade/trade` checkout with the same rule files:

| | pi 1.3.0 | towa `85e52ca` |
|---|---|---|
| Tools | `read`, `bash`, `edit`, `write` | those four, `apply_patch`, `glob`, `grep`, `websearch`, `skill_manage`, `batch` |
| Tool definitions | 2,878 characters | 8,723 |
| System prompt | 70,019 characters | 65,776 |

- pi has no tool for finding or searching. Its rule is "Use bash for file operations like ls, rg, find", and towa has had the same line since `85e52ca`.
- towa's `grep` ran `rg` and `glob` listed files through it. With the rule above the model had two ways to do each. In the eight runs on `85e52ca` it made 54 `bash` calls, 8 `grep` calls (in four of the runs, on PRs 4668 and 4673) and no `glob` call.
- `websearch` was on by default and its definition, a tools line and a rule went with every request. None of the eval runs searched the web.

The capture pointed pi at a local server through a separate agent directory (`PI_CODING_AGENT_DIR`), so the `pi-anthropic-auth` adapter did not run. Its source replaces pi's opening with four lines ("You are an expert coding assistant. Be concise and helpful. Use the available tools to answer the user's request. Show file paths clearly when working with files."), drops `<docs>` and cuts one sentence from `<tools>`.

## What changed

- `glob` and `grep` are removed: the two core tools, their specs, `src/tools/search` and the `tools.search.ripgrep` key. Nothing was released with them, so there is no migration.
- `websearch.enabled` is off by default. `websearch.hosted` still defaults to on and still applies only when `websearch.enabled` is set, so a session without the key gets neither the local tool nor the provider's.
- `tools.max_output_bytes` now bounds a shell call only. Its help says so.
- The `bash` rule and summary are unchanged: "Use bash for file operations like ls, rg, find." and "Execute bash commands (ls, grep, find, etc.)".

The default tool set is `read`, `write`, `edit`, `apply_patch`, `bash`, `skill_manage` when skills are loaded, and `batch`.

## What is lost

- A search result was capped by `head_limit` and named paths from the working directory. `rg` through `bash` is bounded by the shell tool's output limit and the output gate.
- A missing `rg` was reported as its own error. Now the shell says the command is not found.
- ACP clients were shown "Find files" and "Search content" calls with a location. They now see a command.

## Measured

The request as sent, captured in the trade checkout:

| Build | System prompt | Tool definitions | Tools |
|---|---|---|---|
| `85e52ca` | 65,776 characters | 8,723 | 10 |
| this change | 65,495 characters | 5,749 | 7 |

## One run with the instructions cut to the identity line

Before this change, PR 4634 was run once on `85e52ca` with `--context.system_prompt_file` holding only "You are towa, a coding agent. Follow the user's instructions and the project rules provided in context." The question was whether the two sentences after it make towa commit, which pi never did in seven runs and towa did in three of eight.

| Run | Requests | Tokens | Committed | Merged PR's test | Fix |
|---|---|---|---|---|---|
| towa, full instructions | 27 | 1.21M | yes, then amended | pass | `Switch` and `Checkbox` forward the label, with tests |
| towa, identity line only | 9 | 320k | no | fail | the call site uses `aria-label`, and the `Switch` type drops `aria-labelledby` |
| pi | 19 | 728k | no | pass | `Switch` forwards the label |

It did not commit and said so. It also picked a different fix, which the merged PR's test does not accept. One run, with both sentences removed together, so it does not say which sentence matters or whether the choice of fix follows from the instructions. The instructions in the repository are unchanged.

## The factory write recording

`src/loop/testdata/factory_write_{1,2}.json` hold the tool definitions, so they were recorded again on `openai/gpt-oss-20b` with the command in `2026-11-12-prompt.md`. The first recording passed and its replay passed three times.

## Not measured

- Request counts and results on a task with the two tools gone.
- Whether the model searches ignored directories such as `node_modules` more often now. `rg` skips them by default and `find` and `grep -r` do not.
