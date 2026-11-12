# Context sources: one ordered list of instruction files

2026-11-12

`[context]` had `agents_files`, `global_dir`, `extra_files` and `extra_dirs`, and rules had no default home. They are replaced by:

| key | default | meaning |
|---|---|---|
| `default_agents` | `true` | `~/AGENTS.md`, then `~/.agents/rules/**/*.md` in path order, then `AGENTS.md` from the repository root down to the working directory. Missing ones are left out |
| `agents` | none | files or directories (every `.md` under it, path order), read after the defaults in config order. Each must exist |
| `agents_text` | none | inline text read after `agents`. Repeatable flag with `sep:"none"`, so `--context.agents_text="$(cmd)"` keeps commas and newlines |
| `default_skills` | `true` | `~/.agents/skills`, searched first, may be absent |
| `skills_dirs` | none | searched after the default, in order. Each must exist |
| `system_prompt_file` | empty | its text replaces the built-in `prompt` section. An empty file is an error |

- Rules are instruction files, so one loader takes a file or a directory. A rules directory is an `agents` entry.
- Inline text appears in the prompt as `Contents of context.agents_text[i]` and counts against both size limits.
- The global file moved from `~/.agents/AGENTS.md` to `~/AGENTS.md`, which is where the user keeps it.
- A configured path that is missing is an error, a missing default is not: the user named the configured one on purpose.
- `_meta.towa.context_files` from `session/new` are appended to `agents`.
