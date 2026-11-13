# Leftovers from the first days removed

2026-11-13

## Code

- `config.toml` is no longer looked for. The loader named it only to say it is not read, which is a path for an old format.
- `session.Options.Turn` and `session.Options.Mapper` are gone. Only tests used them. A host is given `Open`, and replay uses the mapper of the session's runtime, or an empty one when the runtime has none.
- `session.NewAgent(Options)` is gone. Tests call `NewHost` and then `Host.NewAgent`, as `serve` does.
- The loop no longer sets a replay context size from `Resolved.ContextSize`. `prepareRequest` sets it for each request, from the selected entry or from that same field.
- `fsops.Read` takes the image function as its last argument, which may be nil. `ReadWithImage` was the only form the binary called.
- `config.Load` moved to `src/config/configtest`. The binary calls `config.Parse`.

## CI and the flake

- CI no longer installs ripgrep and the dev shell no longer has it. No Go code or test runs `rg` since the search tools were removed.

## Docs

Ten notes described things that no longer exist and were deleted: the upstream capture scripts, the Claude and Codex tool dialects and their prompts, the generic prompt lookup, the lane split of the first build, the stub loop, the sort order of the removed search tools, and the removed `openai.compact_threshold` key.

`docs/design.md` lost its implementation order and the line about swarm workers, and now says how model figures are found and that the flake exists.

## Left as it is

- Recordings made from `llama3.2:3b` stay until their request changes, as `AGENTS.md` says.
- Other notes keep paths from before `src/` was regrouped. They are dated and say what was decided then.
