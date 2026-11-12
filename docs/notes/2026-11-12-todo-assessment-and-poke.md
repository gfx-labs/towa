# Todo goal assessment and the incomplete-todo poke

2026-11-12

## Decision

The `todo` tool takes jcode's assessment fields, and a turn that ends with unfinished items is sent back to them. Both are on by default, as in jcode (`auto_poke = true`). jcode's quality gate (`todo_gate_digest`) and `autojudge` are off by default there and are not ported.

Read from jcode `037163c7b`: `crates/jcode-app-core/src/tool/todo.rs` (schema and field descriptions) and `crates/jcode-todo-policy/src/followup.rs` (the poke).

## Built

- `src/tools/core/specs/todo.json`: beside the list, an optional `plan` (`user_intention`, `understands_user_intent`) and `goals` (the feedback loop, its relevance, coverage and traceability, delivery state, difficulty, autonomy, iteration maturity, stopping evidence), and `confidence` and `completion_confidence` on an item. The enums and descriptions are jcode's. The tool's schema checks them at dispatch. Nothing scores them: they are the model's own record.
- The list stays flat and is sent whole on every call. jcode's phases, lanes, patches by name, blockers and `waiting_on` are not ported: they serve its swarm and background tasks.
- `todo.Open` reads the session's log for the last accepted list and returns its unfinished items. `src/loop` calls it when a turn ends with `end_turn` and the stop hooks allow it, and appends `todo.Poke` as a user message.
- `limits.max_todo_pokes` (default 3, jcode's `INCOMPLETE_TODO_MAX_ATTEMPTS`). The count is read from the log, so it holds across a restart. It starts again when the unfinished items change or the user writes. 0 never sends a turn back.
- The tool's guidance line now says to use it very frequently.

## Measured

- Through `acphost exec` on `openai/gpt-oss-20b` (LM Studio): asked to list two items, one pending, and then stop, the model sent the plan and one goal, replied `paused`, was sent back once, wrote the file, marked the item completed and the turn ended. One poke, 20,130 tokens in all.
- `src/loop/testdata/factory_write_{1,2}.json` hold the tool list, so they were recorded again on the same model with the command in `2026-11-12-prompt.md`.

## Limits

- No recorded loop test covers the poke. `todo.Open` is tested on built logs, and the turn was checked live as above.
- The assessment costs input tokens on every request: the tool's definition grew from 1.1 KB to 4.3 KB of JSON.
- A model that ignores the tool is never sent back.
