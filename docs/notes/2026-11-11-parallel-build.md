# Parallel build: lanes and rules

2026-11-11

The remaining design steps are built by swarm workers in parallel. Each lane owns a set of paths. This file is the split and the rules. Read `AGENTS.md`, `docs/design.md` and `docs/notes/2026-11-11-contracts-and-conventions.md` first.

## Rules for every lane

- Edit only the paths your lane owns. Anything else is read-only for you: the contracts (`src/transcript`, `src/provider/provider.go`, `src/auth/credential.go`, `src/tools/tools.go`, `src/store/store.go`, `src/httpclient`, `pkg/acp/harness`), `src/cli/cli.go`, `src/config/config.go`, `go.mod`, `go.sum`, `AGENTS.md`, `docs/design.md`, `.github`, `.golangci.yml`. If a contract is wrong or missing something, message the coordinator with the exact change and the reason. Do not work around it.
- `go.mod` already pins every agreed module. If you need another module, message the coordinator. Do not run `go get` or `go mod tidy`.
- A lane that adds commands puts them in its own new file in `src/cli` (named in the table) as a function returning `[]*cli.Command`. The coordinator wires it into `cli.go`.
- Config: your section's file in `src/config` holds its type, defaults and validation. Every key needs a default, except lists.
- Other lanes are editing the same checkout. Build, vet, test and lint only your own packages: `go build ./src/<yours>/...`, `go vet`, `go test -race`, `golangci-lint run ./src/<yours>/...`. Run them with `nice -n 10`. `./...` will fail on someone else's half-written file, and that is not yours to fix.
- Also run `GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build ./src/<yours>/...`. Code that only Linux or only macOS can run goes in `_linux.go` and `_darwin.go` files.
- Commit your own paths, in one command, as pieces work: `git add <paths> && git commit -m "<message>" -- <paths>`. Never `git add -A`, never commit a path you do not own. Do not push: the coordinator pushes after checking the whole tree. If git reports an index lock, wait and retry.
- No credentials. Do not read any token store, key file or another tool's config (`~/.config/jcode`, `~/.jcode`, `~/.claude`, `~/.codex`, and the like). A recorder or live check runs only when the variable it names is already set in the environment. If it is not set, say in your report that the recording is missing. Never write a recording by hand, and never fake one.
- Tests follow `AGENTS.md`: real inputs, no mocked provider, no test that restates a constant. Throwaway probes go under `$JCODE_SCRATCH_DIR`, not in the repo.
- Write `docs/notes/2026-11-11-<lane>.md`: what was built, the decisions and why, measurements with their commands, and what is not done.
- Your final message lists: what works and how you checked it, what you did not check, what is missing, and every request you have for a contract or wiring change.

## Wave 1

| lane | owns | builds |
|---|---|---|
| `acp-layer` | `pkg/acp/**` except `harness`, `tools/acpgen/**`, `src/config/listen.go`. Works in the worktree `~/.jcode/worktrees/acpharness/acp-layer` on branch `acp-layer`, because every other lane imports `pkg/acp` | The generator and the connection on `encoding/json/v2`, with a before and after benchmark. Pooled per-message buffers, with a benchmark. A hook for `_`-prefixed extension requests and notifications on both sides. Unix socket and TCP listeners with optional TLS and client certificate verification |
| `provider-chat` | `src/provider/chat/**`, `src/provider/providertest/**`, `src/config/providers.go` | The Chat Completions streaming client, with the OpenRouter extras. Error classification. `providertest`: record a real exchange to a file with the secrets removed, and replay a file through the real client over HTTP |
| `provider-anthropic` | `src/provider/anthropic/**` | The Messages API streaming client for OAuth and API key credentials, including the request shape a proxied OAuth account needs |
| `provider-openai` | `src/provider/openai/**` | The Responses API streaming client, the Codex OAuth backend, the WebSocket transport, native compaction |
| `auth` | `src/auth/**` (adding to `credential.go` needs the coordinator), `src/config/accounts.go`, `src/cli/login.go` | The account store, refresh under `flock`, proxied accounts, the `login`, `logout` and `accounts` commands, a `Source` |
| `store` | `src/store/**` except `store.go`, `src/config/store.go` | The first backend: one directory per session, durable at the end of each turn, loadable by a new process. The benchmark harness |
| `tools` | `src/tools/**` except `tools.go`, `src/config/tools.go` | One implementation per capability: read, write, edit, shell, search. Argument repair, output gating, `batch`. No dialect adapters yet |
| `capture` | `prompts/**`, `tools/capture/**` | Tool names, schemas, descriptions and system prompts from Claude Code 2.1.280 and openai/codex `ce0323f2`, with the script that produced them and a diff script for new releases. The Claude Code interruption message |
| `mcp` | `src/mcp/**`, `src/config/mcp.go` | The MCP client over stdio, streamable HTTP and SSE, OAuth, the `mcp` meta-tool, direct tools |
| `agentctx` | `src/agentctx/**`, `src/config/context.go` | Context loading and the system prompt built from named sections |
| `loop` | `src/loop/**`, `src/provider/router/**`, `src/config/limits.go` | The agent loop: stream, tool dispatch, limits, cancellation, typed stops, retry and fallback. The mapping from transcript items to ACP updates |
| `session` | `src/session/**`, `src/config/session.go` | Client capabilities, modes, config options, additional directories, the system prompt field, commands, permission requests, the unstable methods that need no store |

## Boundaries between lanes

- `loop` owns `loop.Turn` and `loop.Result`. `session` fills a `Turn` and never edits `src/loop`. They agree on new fields by message.
- `loop` exports the mapping from a `transcript.Item` to `acp.SessionUpdate` values, because `session/load` replays history with the same mapping.
- The three provider lanes share `providertest`. `provider-chat` writes it first. The others start with request building and stream parsing and adopt it once it is committed.
- A provider client takes its `*http.Client` from the caller. Nothing but `src/httpclient` builds one.
- `auth` implements `auth.Source`. Providers only read `auth.Credential`.

## After wave 1

The coordinator integrates the lanes, wires `src/cli`, runs the whole check and pushes. Wave 2 then builds what needs wave 1: the dialect adapters, todo and plan, webfetch, conversation search, subagents, compaction, embeddings, Jev, hooks, model chains and the model config option, `session/load` and the other stored-session methods, the `hosts` command, the turn-through-a-proxy test, the conformance suite and the benchmarks.
