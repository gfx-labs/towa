# Prompt sections in tags, a tools list, and rules worded as pi's

2026-11-12

## Why

pi 1.3.0 took fewer requests than towa on the two harder `oku-trade/trade` tasks (`2026-11-12-slim-instructions.md`). Its base instructions were tried in towa and were worth one request at most (`2026-11-12-batch-entry.md`). What was left to compare was the layout of the prompt and the wording about tools. Read from pi's source and its ACP adapter, not from a captured request:

- pi has a `<tools>` section with one line per tool, a `<rules>` section, and a `<project_context>` section whose files are each in `<project_instructions path="...">`.
- towa sent untagged text: one paragraph about tools with guidance lines under it, then each context file under "Contents of <path>:". Nothing marked where one rule file ended and the next began, and no part of the prompt listed the tools.
- Four of towa's guidance lines told the model not to use `bash`: for reading, editing, finding files and searching. pi's lines say the opposite for the last two ("Use bash for file operations like ls, rg, find").

## What changed

- Every section after the instructions is sent inside a tag of its name: `<tools>`, `<rules>`, `<project_context>`, `<skills>`, `<working_directory>`, the name of a section added by `context.sections`, and `<client>` for a prompt the ACP client appended. The instructions open the prompt untagged, and so does a client prompt in override mode, which stands in their place.
- `<tools>` is new: one line per tool the request offers, `name: summary`. `tools.Tool` has `Summary` for it. Core tools take it from `summary` in `src/tools/core/specs/<name>.json`, where it is required. Extras set it where the tool is built. The summaries of `read`, `write`, `edit`, `bash`, `glob` and `grep` are pi's. An MCP tool has no summary and is not listed: its definition describes it.
- `<rules>` holds each offered tool's guidance lines, then `agentctx.GeneralRules`. The paragraph that used to open the tools section is now three of those rules, one sentence group each. pi's two general rules follow them: be concise, show file paths.
- Guidance lines reworded to pi's, with towa's parameter names:
  - `read`: "Use read to examine files instead of cat or sed."
  - `edit`: "Use edit for precise changes (old_string must match exactly)."
  - `write`: "Use write only for new files or complete rewrites."
  - `bash`: "Use bash for file operations like ls, rg, find." is added ahead of the no-terminal rule.
- `glob` and `grep` have no guidance line. Both lines only said not to use `bash`. The tools stay enabled, listed and described, and the model chooses.
- `<project_context>` starts "Instructions from the context files:" and puts each file in `<project_instructions path="...">`. A `"` in a path is escaped. The file's text is sent as written. Inline `context.agents_text` has the path `context.agents_text[i]`, as before.
- A `context.sections` name must match `^[a-z][a-z0-9_-]*$`, because it is now a tag. Another name is refused at config load. `rules` is a built-in name again, so a configured section cannot use it.
- `agentctx.Prompt` has `Tools []ToolLine` and `Rules []string` in place of `Tools string` and `ToolLines []string`. `SectionRules` is back as a section name. The slot of that name removed in `2026-11-12-prompt.md` was never filled. This one always is.

Not changed: the instructions, the tool definitions and their parameter names, the working directory text, and the skills list.

## The skills list stays one line per skill

pi lists skills as `<available_skills>` with `<skill><name>…</name><description>…</description><location>…</location></skill>` each. That was built and measured with the user's 45 skills: the skills section went from 17,049 to 21,042 characters, and the whole prompt to 50,794. It is sent with every request and adds nothing the one-line form lacks, so the list is as it was, inside `<skills>`.

## Measured

The prompt as sent, captured by pointing a build at a local server that writes the request body (12 context files, 45 skills, no project `AGENTS.md`):

| Build | System prompt | Tool definitions |
|---|---|---|
| `397bf56` | 45,615 characters | 8,723 |
| this change | 46,746 characters | 8,723 |

The tools paragraph and its lines were 1,014 characters. `<tools>` and `<rules>` together are 1,529. The rest of the growth is tags.

The same question to Opus 5.5 through `acphost exec` on both builds ("quote every sentence of your instructions that mentions cat or sed, then name your tools"):

| Build | First request | Sentences quoted from the harness |
|---|---|---|
| `397bf56` | 23,206 tokens | the old `read` and `edit` lines |
| this change | 23,423 tokens | "Use read to examine files instead of cat or sed." |

217 tokens more on every request. Both named the same ten tools.

## The factory write recording

`src/loop/testdata/factory_write_{1,2}.json` hold the prompt. The old recording failed on the first request, as it should. They were recorded again on `openai/gpt-oss-20b` with the command in `2026-11-12-prompt.md`. The first recording passed and its replay passed three times. `TestFactoryWriteProcess` reads the working directory out of the request with the same pattern as before, since that line's text is unchanged.

## Not measured

- Whether the layout or the wording changes the request count, the context per request or the result on a task.
- Whether the model now searches through `bash` more often, and what that costs: `glob` and `grep` skip ignored files and cap their output, and `rg` in `bash` is capped only by the shell tool's output limit.
