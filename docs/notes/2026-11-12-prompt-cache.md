# The system prompt is built once per session

Date: 2026-11-12

## What changed

`agentctx.Build` assembled the system prompt for every model entry of every turn, from inputs that do not change while a session is open: the context read when it opened, its working directory, the client's `systemPrompt` and the tools' guidance lines.

The session's runtime now keeps what it built (`promptCache` in `src/cli/runtime.go`). There are two places, because the prompt differs between model entries only in whether web search is the provider's own. A kept prompt is used when every input equals what it was built from, compared by value, and is built again otherwise. In practice that happens once, when the session's MCP tools arrive after the first prompt was built.

Nothing is keyed on time and nothing is evicted: the cache lives and dies with the session's runtime, so `session/load`, `session/resume` and `session/fork` start with an empty one, as they do with the context files.

`TestPromptIsBuiltAgainOnlyWhenItsInputsChange` covers the reuse and each rebuild case.

## Measurements

Sixty-four turns in eight sessions against `openai/gpt-oss-20b` with a 65,536-token context and the user's own rules and skills, allocation profile from `telemetry.pprof.address`. One run of each build (`a799c49` and this change). The model chooses its tool calls, so the runs differ: the second made 141 tool calls against 79 and sent 51% more input tokens, which is why its total is higher.

| Allocated over the run | Before | After |
|---|---|---|
| `agentctx.Build` | 20.29 MB | 2.56 MB |
| `runtimeFactory.prepare` | 23.79 MB | 9.06 MB |
| Whole run | 112.87 MB | 172.39 MB |

The 2.56 MB left is eight sessions building their prompt, about 0.3 MB each. PSS at the end was 42.3 MB before and 42.0 MB after. CPU is not separable from the difference in work between the runs.
