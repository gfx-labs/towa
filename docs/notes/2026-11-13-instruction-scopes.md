# Global and project instructions

Date: 2026-11-13.

## Change

The context files were one `<project_context>` section, each file in `<project_instructions path="...">`. `~/AGENTS.md` and `~/.agents/rules` were labelled as project instructions, the same as the repository's `AGENTS.md`. They are the user's rules for every project (user, 2026-11-13).

They are now two sections:

- `<global_instructions>`: `~/AGENTS.md`, `~/.agents/rules/*.md`, then every `context.agents` and `context.agents_text` entry. A configured entry is global, wherever its path points.
- `<project_instructions>`: `AGENTS.md` from the repository root down to the working directory, nearest last. Its opening line says it takes precedence over the global instructions where the two conflict.

Each file is in `<file path="...">` within its section. Project files now come after every configured entry, where before they came between the home defaults and the configured entries. A file reached as both, such as a configured path that is also the repository's `AGENTS.md`, stays at its first position, as a global file.

## Why

Codex and Claude Code each keep a user-wide file apart from the repository's (`~/.codex/AGENTS.md`, `~/.claude/CLAUDE.md`). OpenAI's Codex guide has later, more specific files override earlier ones. One label for both gave the model no way to tell which file applies to every project and which wins a conflict.

## Check

A request captured from `oku-trade/trade` with a local endpoint holds `<global_instructions>` with 12 files (`~/AGENTS.md` and the 11 rule files), then `<project_instructions>` with trade's `AGENTS.md`. Not measured on a task.
