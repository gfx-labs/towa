# `src/` is eleven groups with an import rule

2026-11-12

## Why

`src/` had 23 entries at its top, with nothing saying which could import which. `tools/extras` imported the loop and the subagent runner, so `tools` was not a layer, and the clients, the prompt building and the support code sat beside the packages that use them.

jobshop groups its packages into families under `pkg/`, and its `archcheck` lists which family may import which and fails CI on a new edge.

## Decision

Packages keep their names. Only their paths change.

| group | packages | moved from |
|---|---|---|
| `config` | `config` | |
| `transcript` | `transcript` | |
| `support` | `filelock`, `httpclient`, `telemetry` | top of `src/` |
| `auth` | `auth` | |
| `provider` | `anthropic`, `chat`, `openai`, `hostedsearch`, `router`, `providertest`, `models` | `models` from the top |
| `sidecar` | `embed`, `jev` | top of `src/` |
| `store` | `store`, `dir` | |
| `prompt` | `agentctx`, `compact` | top of `src/` |
| `tools` | the contract, one package per tool, `toolset`, `mcp` | `toolset` and `mcp` from the top |
| `agent` | `loop`, `looptest`, `hooks`, `subagent`, `session`, `extras` | the top, and `extras` from `tools/` |
| `cli` | `cli`, `listen`, `hosts` | `listen` and `hosts` from the top |

- A group imports only groups above it in the table.
- `src/layers_test.go` holds which group may import which. It reads `go list` and fails on an import the table does not allow, in code or in a test, and on a package that is in no group. It is a test and not a tool of its own, so `go test ./...` and CI run it with nothing added.
- Names that were turned down: `context` (the standard library has it), `integrations` and `app` (too general).

Two things the rule turned up:

- `compact`'s exchange tests ran the loop through `looptest`, which is above `prompt`. They moved to `src/agent/loop/looptest/compact_exchange_test.go` and read the two recordings where they stayed, in `src/prompt/compact/testdata`, because a `compact` test replays one of them too.
- `sidecar/embed`'s test replays with `provider/providertest`, so `sidecar` may import `provider`. No code of `sidecar` does.

## Check

`go list ./src/...` before and after: 45 packages both times, and every package name is unchanged. The full check set passes.
