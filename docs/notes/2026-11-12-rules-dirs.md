# Rules directories

2026-11-12

## Decision

`context.rules_dirs` names directories whose `.md` files go into the system prompt after the `AGENTS.md` files. The default is `~/.agents/rules`, the directory jcode also reads by default. Subdirectories are read, in path order.

A rules directory that does not exist is skipped, as a skills directory is: the default is absent on most machines. `context.extra_dirs` stays strict, because a directory named there was asked for.

## Why

Found while comparing acpharness with jcode on one task (2026-11-12). jcode's prompt held the user's eleven rule files and acpharness's held none, so the two agents were working to different instructions. `context.extra_dirs: ["~/.agents/rules"]` already loaded them, but only for a user who knew to set it.

## Order

1. Global `AGENTS.md`.
2. `AGENTS.md` from the repository root down to the working directory.
3. Rules directories.
4. `context.extra_files`, then `context.extra_dirs`.

A file reached twice is read once, at its first position, so a rules directory that is also listed in `extra_dirs` adds nothing the second time.

## Not done

- jcode also reads `./.jcode/rules` and `~/.jcode/rules`. Those are jcode's own directories and are not defaults here. They can be added to the key.
- The rule files are sent whole in every session. There is no per-task selection.

## Superseded

2026-11-12: `rules_dirs` is replaced by `context.default_agents`, which reads `~/.agents/rules` as a default, and `context.agents`, which takes more rules directories (`2026-11-12-context-sources.md`).
