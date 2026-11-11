# One tool set, one prompt

2026-11-11

## Decision

The three tool dialects (`claude`, `codex`, `generic`) and the pinned copies of Claude Code's and Codex's prompts are removed. Every model gets the tools in `src/tools/core` and the instructions in `src/agentctx/instructions.go`, both written here.

## Why

The dialects existed on the assumption that a subscription backend expects its own CLI's tools and prompt. jcode shows it does not. At `1jehuang/jcode` 037163c7b it sends its own instructions, its own function tools and `originator: jcode` to the ChatGPT Codex backend, sends its own tool names on the Claude OAuth path, and reads only `AGENTS.md`. The one piece the Claude OAuth path requires is the billing header block, which the `anthropic` provider already sends and which this change does not touch.

What the dialects cost:

- `prompts/` held the captured prompts and schemas, with `tools/capture` and `src/tools/core/specgen` to refresh them.
- Three adapter packages gave the same capabilities three names, schemas and output texts.
- A chain could not mix model families, because a fallback happens inside a turn and the tools could not change there. `models.<name>.dialect` and `family` existed to settle it.
- The interruption message had a role per dialect.

## What changed

- `src/tools/claude` became `src/tools/core`. `src/tools/codex`, `src/tools/generic`, `src/prompts`, `prompts/` and `tools/capture` are deleted.
- Tools: `read`, `write`, `edit`, `apply_patch`, `bash`, `glob`, `grep`, `todo`. Options the implementation never supported are gone from the schemas instead of being refused at run time: `read` `pages`, `bash` `run_in_background` and `dangerouslyDisableSandbox`, `grep` `-C` and `-o`.
- `apply_patch` is a function tool with one string argument, `patch_text`, in the format `src/tools/patch` already parsed for the Codex dialect. It was a Responses custom (freeform) tool before, which only that API has.
- Config keys `models.<name>.dialect` and `models.<name>.family` are removed. A config that sets them fails to load as an unknown key (`ErrorUnused` in `src/config/load.go`).
- A chain may mix providers.
- `CLAUDE.md` is no longer loaded. `AGENTS.md` is.
- The interruption message is one text, sent as a user message.

## Tests

The comparisons against outputs recorded from the upstream CLIs went with the adapters: there is no upstream to match. Tool tests run each tool against real files, a real shell and the real `rg`.

`src/loop/testdata/factory_write_{1,2}.json` were recorded again against local Ollama `llama3.2:3b` (two requests), because the request's tool list is part of the recording.

## Limits kept from the captures

Figures in `src/config/tools.go` that were read from Claude Code 2.1.280 (read size, line count, image size, shell timeouts, output length) keep their values and their source comments. They are defaults with a stated origin, not a claim of matching upstream behavior.
