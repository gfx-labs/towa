# Context files are read when the session opens

Date: 2026-11-12

## What changed

`agentctx.Load` ran at the start of every turn: it read `~/AGENTS.md`, the rule directories, each `AGENTS.md` from the repository root to the working directory, the skill listing and the configured sections. It now runs once, in `runtimeFactory.open`, and the result is kept in memory on the session's runtime. Every turn builds its prompt from that copy.

A context file or a skill changed while a session is open is therefore not in that session's prompt until the session is opened again. The prompt stays the same from turn to turn, so the provider's cached prefix survives a turn that edits a rule file. jcode does this with `AGENTS.md` for that reason (`agents_md_snapshot` in `jcode-app-core/src/agent.rs`, read at `037163c7b`). opencode v1.18.25 is the other way: `instruction.system()` is called for every model step.

The copy is not stored with the session. Whatever opens a runtime reads the files again:

- `session/new`
- `session/load` and `session/resume`, after a restart of the process or of a session that is live, which is drained and opened again
- `session/fork`

So a session continues with changed rules after a load or a resume. This is read from the code (`openRuntime` in `src/session` is called from those three places) and was not run across a restart.

What decides the load cannot change within an open session: the working directory, and the `context_files` and `skills_dirs` overrides, are set when it opens.

The system prompt is still assembled every turn by `agentctx.Build`, because its tools section depends on the model entry the turn selects.

`TestContextFilesAreReadWhenTheSessionOpens` covers it: a rule file rewritten after a session opened is absent from that session's prompt and present in the next one's.

## Measurements

Thirty-two turns in four sessions against `openai/gpt-oss-20b` loaded with a 65,536-token context, with the user's own rules and skills (about 12,000 tokens of prompt), allocation profile from `telemetry.pprof.address`. One run of each build. The model chooses its tool calls, so the runs differ: the second produced 30% more output.

| Allocated over the run | Before | After |
|---|---|---|
| `agentctx.Load` | 14.0 MB | 3.5 MB |
| All of `prepare` | 25.2 MB | 11.1 MB |
| Whole process | 79.7 MB | 59.3 MB |

CPU did not move outside the difference between the runs (810 ms and 980 ms), and memory at the end was the same (38.4 MB PSS).
