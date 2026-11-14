# Reasoning effort as an ACP config option

2026-11-13

## Why

The only way to choose an effort was the `#effort` suffix on a `models` entry. An ACP client sees the model option, whose values are whole entries, so to change effort it had to pick an entry text with another suffix, and only the efforts written in the config were reachable. The reference adapters expose effort as its own select option:

- codex-acp `src/ModelConfigOption.ts`: id `reasoning_effort`, category `thought_level`, choices from the model's `supported_reasoning_levels`.
- claude-agent-acp `src/session-effort.ts`: id `effort`, category `thought_level`, choices from the model's supported effort levels, plus `default`.
- acpx treats `thought_level` as an alias for codex-acp's `reasoning_effort`, so clients look the option up by category.

towa uses codex-acp's id and the `thought_level` category.

## What changed

- `provider.ModelInfo.Efforts`: Anthropic's `/v1/models` reports `capabilities.effort` as `{"supported":bool,"<level>":{"supported":bool},...}`; the members are read in order, so a new level needs no towa change. The Codex backend's `/models` reports `supported_reasoning_levels[].effort`. Both are kept in `models.json` as `efforts`.
- The model list is now read for a configured model the built-in table has too (the four OpenAI rows), because the table holds no efforts.
- A configured provider names a model's efforts in `models[].reasoning_efforts`, which outranks the list.
- `session.ConfigOption.Depends` gives an option whose choices follow the session's other values. After every change and at each step of session start the registry aligns such options: the value stays if still a choice, moves to the default if not, and the option is dropped when there are no choices.
- `reasoning_effort` starts at the entry's `#effort`, or `default`, which sends nothing. An entry effort the list does not have stays offered, since the config chose it.
- `loop.Turn.Effort` replaces the selected entry's effort; fallback entries keep their own. A subagent on another model does not inherit the parent's effort.

## Checked

- Anthropic recording `oauth_models.json` (2026-11-12): opus 5.5 lists low through max, opus 4.5 low through high, haiku 4.5 none.
- Live, Sonnet 5.5 `#medium`: the option starts at `medium` with five choices, `low` is taken and the turn ends normally, an unknown value is Invalid params.
- A capture server for a chat provider with `reasoning_efforts: [low, high]`: choices `default, low, high`, and after choosing `high` the request carried `"reasoning_effort":"high"`.
