# Skills and tool schemas shared between sessions, and pprof

2026-11-12

## Decision

- `agentctx.SkillCache` keeps the name and description read from each `SKILL.md`, by path. The runtime factory holds one for the process. Every `session/new` still walks the skills directories and stats each `SKILL.md`. A file is read and parsed again when its modification time or size has changed, and a file that is gone is not listed. A file that fails is not kept, so its error is reported each time.
- `toolset.Cache` keeps each core tool's and extra's input schema as it is sent (with the gate's member) and as it is compiled, by tool name. An entry is used only when the tool's schema bytes and the gate setting match it. MCP tools are not kept, since their names and schemas differ by session.
- `src/tools/core` decodes each embedded spec file once per process.
- `telemetry.pprof.address` serves `net/http/pprof` under `/debug/pprof/`. It is empty by default. Config refuses an address that is not loopback: the pages have no authentication and show the program's memory. Port 0 takes a free port and the log names it.

## Why

One process with empty sessions grew by more per session than jcode did on the same test (PSS, no model request). A heap profile of 50 empty sessions (`runtime.MemProfileRate = 1`, a GC before each dump) showed that a session kept about 59 KB, and that about 49 KB of it was the same in every session:

| Kept by | Per session |
|---|---|
| `toolset.withAcceptMember`, the gated copy of each schema | 20 KB |
| `agentctx.ListSkills`, 45 skills | 20 KB |
| `core.Tools` and compiled schemas | 9 KB |
| the rest | 10 KB |

Each `session/new` also allocated about 7 MB that was then collected, of which 4.4 MB was reading 45 `SKILL.md` files (392 KB on disk) to parse their front matter. `prepare` runs twice per `session/new`.

## Measurements

Machine `dog`, Go 1.27.3, the user's real home (45 skills, 11 rule files), config with one model and the defaults.

Heap profile, 50 empty sessions, difference from no sessions:

| | Before | After |
|---|---|---|
| Live heap | 2.99 MB | 1.20 MB |
| Of which the caches' one-time cost | none | 0.30 MB |
| Allocated in `NewSession` | 343 MB | 61 MB |

PSS of the process from `/proc/<pid>/smaps_rollup`, two rounds of two runs, old and new binary interleaved. Old is `fd541d5`, new is `a41d534` plus this change:

| Sessions | Before | After |
|---|---|---|
| 1 | 29.0 to 31.8 MB | 30.1 to 32.8 MB |
| 50 | 36.7 to 37.9 MB | 31.6 to 33.8 MB |
| 200 | 50.9 to 52.0 MB | 36.0 to 38.7 MB |
| Each additional, 1 to 200 | about 107 KB | about 32 KB |

The run-to-run spread at one session (about 3 MB) is larger than the change there.

## Not done

- The context files (`AGENTS.md`, rules) are still read for each session and the system prompt is still built for each one. They make most of the 1.2 MB a `session/new` still allocates. The text differs by working directory, so sharing it needs a key on the file set.
- A skill edited without a change of modification time or size is not seen until the process restarts.
- Entries are never removed. The skill cache holds one per `SKILL.md` path seen and the schema cache one per tool name.
- Only `serve` starts the profiling pages.
