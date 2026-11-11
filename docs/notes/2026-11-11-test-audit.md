# Test audit, 2026-11-11

Every test function was read against the production code it covers and against the test at the strongest boundary for the same contract. A test was removed only when a named remaining test fails on the same regression, or when the code it covered has no production caller.

## Removed

Tests that repeated a contract another test owns:

- `anthropic` opaque-of-another-provider test. `src/provider/boundaries_test.go` covers it for all three APIs.
- `jev` no-route test, and the config test's copy of the defaults. The config-file test covers the disabled case.
- `httpclient` GET cases. `src/cli/proxy_test.go` sends real provider requests with the same client through a CONNECT proxy, with and without the CA. The WebSocket and `NO_PROXY` cases stay.
- `mcp` missing-command and shared-server-restart tests, covered by `tools_test.go` and `manager_test.go`.
- `session` prompt-in-directory test, one `TestRejectedRequests` row and the `legacy` mapper case.
- `pkg/acp` extension cancellation test. Cancellation does not pass through extension lookup, and the two connection cancel tests cover it.
- `conformance` `TestPrompt` and `TestClose`, and the capability, mode and option assertions in `TestInitialize` and `TestNewSession`. `testdata/wire.golden` pins those bytes. The check that every update decodes in the SDK moved into `TestWire`.
- Smaller cuts in `chat`, `listen`, `cli`, `claude`, `patch`, `webfetch`, `hosts`, `auth` and `transcript`.

Tests of configuration validation that lived in `src/hooks` and `src/agentctx` and called only `config.Load`. Most cases restated a validator struct tag with a bare error check. The two hand-written hook rules (tools on an event without a tool, empty program) are now rows of `src/config/load_test.go:TestInvalidInputIsAnError`, which checks the key. `TestOptionalSectionsPreserveLoaderPolicy` loads every hooks and context key from a file.

## Production code removed with its only tests

None of these had a production caller.

- `todo.Update`. The CLI builds the plan update itself, and the ACP encoder writes the `sessionUpdate` discriminator.
- `fsops.Roots.Contains`.
- `subagent.Options.Loop`. The session supplies `Parent.Loop` on every call.
- `listen.Serve` and `listen.ErrNoListener`. The CLI calls `Open` then `Listeners.Serve`, and only when a socket is configured.
- `mcp.Manager.Servers`, `jev.Client.Endpoint`, `jev.Client.Model`, `agentctx.IsInstructionSection`.
- `src/store/storebench` and `dir.Stats.WrittenBytes`. The package was a measurement harness that only its own test imported. Its results are in `docs/design.md` and `2026-11-11-store.md`. The commands in that note for `TestRunByHand` and `TestListByHand` now need the package from git history (last present at `3d4a15f`). The one backend contract its test held, one fsync per append, is asserted in `dir_test.go:TestLoadStartsAtTheLatestCompaction`.

Earlier notes that name these (`hosts-and-todo`, `tools`, `subagents`, `listen`, `store`) describe the code as it was on their date.

## Bugs found

- **Children index could serve a stale list.** `.index/children.json` was trusted when the root directory's modification time equalled the recorded one. A session created, deleted or copied in within the same timestamp tick as the index build leaves that time unchanged, so the next parent query returned the old children. On tmpfs `TestListPagesAndChildrenSurviveChangesAndDirectoryMoves` failed 30 of 30 runs, on btrfs about 1 in 15. The index is now trusted only when its file was written in a later tick than the root's last change. After the fix: 200 of 200 on tmpfs, 100 of 100 on btrfs.
- **`TestSessionMovesBetweenConnectionsOfOneProcess` was flaky.** It sent `session/status` and `session/resume` in one write and expected `saved`. Requests run concurrently, so resume could attach the session first and `open` was then correct. The test now awaits status before sending resume.
- **The `start` helper in `src/session` raced the commands announcement.** It sent a `/mode` prompt to drain the announcement, which usually but not always arrived before that prompt's answer. It now reads the announcement directly.

## Not removed

- **Recordings that do not exist.** 23 replay subtests skip because their recordings were never committed: every Anthropic API and OAuth scenario, every Codex scenario and all of OpenRouter. Only the Ollama recordings run. Three `chat` tests cover OpenRouter error classes and reasoning details with bodies typed from documentation. That breaks the recording rule, but they are the only coverage of that code until recordings exist.
- **Claude and Codex adapter tests assert hand-written output fragments**, not output recorded from the pinned upstream CLI. They are the only adapter coverage.
- **OAuth tests use a hand-written authorization server** (`src/mcp/mcptest`, and one inline in `src/cli/login_test.go`). The assertions are on this client's behaviour: refresh count, stored tokens, URL binding. Rotation under concurrency cannot be recorded.
- **`pkg/acp` benchmarks**, the baseline for the next pooling change on the connection path.
- **`tools/acpgen` tests**, inherited from upstream.

## Open

- The conflict rule for tool calls is implemented in both `src/loop/schedule.go` and `src/tools/batch/batch.go`, and tested in both. One shared implementation would remove about 100 test lines.
- `loop.Stub`, `session.Options.Turn`, `session.NewAgent` and the `Options.Mapper` fallback are reached only from tests. Boolean config options and `Supports.Prompt` are never registered or set by production code.
- `shell.Input.MaxOutputBytes`, `search.GrepInput.Literal`, `search.Filter.Hidden` and `search.Filter.NoIgnore` are set only by tests.
