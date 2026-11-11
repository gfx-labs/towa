# Subagents

## Built

`src/subagent` runs a child through `Runner.Run(context.Context, loop.Turn)`.
`*loop.Loop` implements it. One `Spawner` serves the process. Input is a
prompt, optional named model and description. The session supplies its runner
per call in `Parent.Loop`, so process-wide admission does not retain a
session's MCP connections or tool set. `Options.Loop` remains an optional
default for callers sharing one loop. Result is the child's last
message, session id, stop reason and usage.

`extras.Subagent(spawner, runner)` adapts it to `tools.Tool`. `loop.Current`
supplies the actual parent turn for each invocation, including calls in
batches. The explicit runner receives the child turn and must open and close
a fresh child runtime. Reusing the parent's loop would retain its file-read
tracker, MCP manager and session-bound tool closures. No earlier turn or model
selection is captured. The tool
is `Executes`, since its child can execute commands. Its JSON output carries
the final message, child session id, usage and whether it was charged.
Failed calls carry a typed kind and `harness.Stop` when available, never a
successful partial message. The wrapper rejects a calling session that
differs from the actual turn.

The child inherits the parent's working directory, additional directories,
context settings, client, permission function and per-turn limits. It has a
new UUID and `Turn.ParentID` names its parent. The loop stores that relation.
The same loop resolver supplies providers and tools. Conversation history is
not copied. Each child gets its own transcript and turn state.

The parent's cancellation context cancels the child. The loop retains what
it stored. A limit or `loop.StopError` becomes a typed `subagent.Error`
carrying `harness.Stop`, never a successful message. Cancellation and an empty
final message are also typed errors.

`loop.Spend` charges returned child usage to the parent, including cached
and thought tokens. `Charged` reports whether the supplied context belonged
to a dispatched parent tool call. The child transcript owns its usage
entries, rather than duplicating them in the parent's transcript.

## Decisions

At jcode `origin/master` `8e11eb23e`, the direct subagent tool was removed:
`crates/jcode-app-core/src/tool/tests.rs:342-353` checks that it is absent.
The remaining in-process child runner is
`crates/jcode-app-core/src/server/swarm.rs:1943-2027`.

- `subagents.max_depth = 1` follows removal of `subagent` and `task` from
  the inline child's tools at `swarm.rs:1990-1993`. Zero disables children.
  Depth is carried through the running child's context. Excess depth is
  refused before a child runs.
- `subagents.max_concurrent = 32` follows
  `crates/jcode-config-types/src/lib.rs:609-611`. Zero disables that guard,
  as described at `:584-587`. Here the count is process-wide running
  subagents, not live sessions in one swarm. Excess calls are refused,
  not queued. There is no additional hard cap.
- `subagents.models = []` follows disabled model selection and the empty
  allowlist at `crates/jcode-base/src/config/default_file.rs:612-613`.
  An unnamed model inherits the parent's choice, as at `swarm.rs:1968`.
  Explicit selection must match a configured allowed name. The loop's
  model resolver checks that the name exists.

The adapter must pass the actual parent's `loop.Turn` and the tool's context
and `Env.Progress`. Child permissions use the same `Permit` function.
ACP tool call content has no nested update tree. The progress view adds the
child's text preceding tool calls, tool titles and failure lines under the
parent call. It does not forward thoughts, inputs, outputs, diffs, plans,
usage or router updates. The final message comes from `loop.Result.Final`,
not accumulated update chunks. No extra numeric progress limit is invented.
The view sends additions only when a child tool starts or fails, not for
every token. The dispatcher accumulates those additions for ACP replacement
semantics.

## Checked

`go build ./...`, `go vet ./src/subagent/...`, Linux `CGO_ENABLED=0`
build, darwin/arm64 `CGO_ENABLED=0` build and capability lint passed.
`go test -race -count=5 ./src/subagent/...` ran without skips against the
real loop fixture in `98425d2`: the Chat Completions client, router and
directory store replay the recorded Ollama turn. The tests check the final
answer, stored parent id, descendant conversation search, permission and
progress forwarding, refused model selection, depth and concurrency, and
a configured limit returned as a typed error. Both per-call and default
runner selection run through that loop. No fake loop or provider is used.
The wrapper regression invokes the tool from a real dispatched call's
permission context. Parent and child use distinct real loops and recorded
clients. It checks the current model selection, inherited permission handling,
and parent usage against the sum of both stored transcripts. The recording
calls `read_file`, not `subagent`, so this does not establish model-generated
selection of the subagent tool.

The recordings say nothing about hosted providers' authentication, rate
limits or reasoning items.

## Not done

Central CLI wiring is owned by the coordinator. No independently cancellable
child ACP session or subagent association notifications. This is a synchronous child
turn, not swarm. Depth is a running-context limit, not a persisted policy
for independently resumed children.

The wrapper regression also denies child permission with a transport error
after its first model request. It verifies positive stored child usage,
failed tool output, and that the parent total includes that usage. The loop
returns its usage snapshot alongside the error and the capability charges it
before returning the typed error. Hosted auth and rate-limit paths remain
unrecorded. Full CLI child runtime resource cleanup is a central-wiring check,
not established by these direct real-loop tests.
