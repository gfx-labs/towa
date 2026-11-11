# Compaction

2026-11-11

`src/compact` and `src/config/compaction.go`. The controller boundary is agreed with the loop lane. This lane edits no loop, router, session or runtime assembly code.

## Boundary and ownership

```go
controller, err := compact.NewController(compact.ControllerOptions{
    Config: compactionConfig,
    Summary: summaryProvider,
    SummaryInput: compact.SummaryInput{
        Model: summaryModel,
        MaxOutputTokens: summaryMaxOutputTokens,
        Effort: summaryEffort,
        ConversationSearch: conversationSearchEnabled,
    },
    SummaryContextWindow: summaryContextWindow,
    Embedder: embeddingsClient,
    Native: nativeForProvider,
})
if err != nil {
    return err
}
defer controller.Close()
effects, err := controller.Before(ctx, compact.BeforeInput{
    Entries: fullStoredHistory,
    Request: selectedRequest,
    ProviderName: selectedContextName,
    ContextWindow: selectedContextWindow,
})
if err != nil {
    return err
}
// The existing session owner appends effects, then calls compact.Items and
// loop.Conversation before sending selectedRequest.
```

The names in this fragment are the caller's resolved dependencies, not additional configuration keys. `SummaryContextWindow == 0` uses the selected request model's window. A different summary model needs its own metadata. The summary provider can be a router, which supplies the credential. A direct provider needs a credential in `SummaryInput`.

The loop hook is `Options.NewCompactor func(context.Context, Turn, Resolved) (*compact.Controller, error)`. It is called once per turn and may return a session-owned controller. `Close` cancels the one pending call and leaves the controller reusable. There is no session map, second store, polling timer, tool dispatcher or alternate context lifecycle in this package. The existing session owner serializes `Before`, `Close`, and appends.

`Before` returns zero or more effects, ordered as summary usage and then the compaction. The caller appends them as one group. The controller never appends directly. `Source` captures the old prefix and its latest compaction id. `Result.ItemFor` refuses a superseded prefix and preserves every entry appended while a summary ran. In particular, a capture with nil `FirstKept` uses the first newly appended entry when applied, rather than discarding those new entries.

Read the full `Store.History`, not `Load`: a compaction's `FirstKept` may refer to an entry before that compaction's position. Only the context view is shortened. Nothing deletes or rewrites an entry. No path dispatches a stored tool call. The loop's existing `Conversation` repairs unanswered calls before normalizing a request.

The router's preparation callback runs for the actual selected chain entry and masks itself in the summary request context. This handles native portability on fallback without recursively preparing the summary's router. `BeforeInput.ProviderName` must be the wire client's opaque-context identity, exactly the value in `Item.Provider`, not necessarily the configured provider id. Codex OAuth and API-key Responses have different identities. The coordinator and provider/loop lanes own `provider.ContextNamer` wiring for this distinction.

## Decision and emergency wait

`Decide(Input) (Decision, error)` follows jcode's `CompactionManager` at origin/master `8e11eb23e`, read with `git show` (`crates/jcode-base/src/compaction.rs`: `ensure_context_fits` 981, `should_compact_with` 881, `anti_signals_block` 508, `should_compact_proactively` 557, `should_compact_semantic` 604).

- Context tokens are the last request's input plus cache reads and writes. The budget is the model window, capped by `max_context_tokens`.
- At `emergency_threshold`, it chooses emergency compaction, or a context-full stop when mode is off or too few messages remain to cut.
- Reactive uses the configured fraction or absolute threshold, whichever comes first.
- Proactive uses EWMA growth and lookahead, with floor, sample, stall and cooldown guards.
- Semantic requires the configured embedder. Topic shift and projection are gated as proactive is. Fewer than four observed turn vectors means topic-shift history is still warming, not permission to ignore a missing or failed embeddings endpoint.
- Native auto leaves compaction to the server. Native explicit starts a background native request at its threshold. Native off uses the ordinary mode.

`Controller.Before` polls completed work without waiting. At emergency it waits for pending work with a channel select bounded by both the caller's context and `compaction.pending_wait_ms`. The default is 15,000 from `crates/jcode-base/src/compaction.rs:44` (`HARD_THRESHOLD_PENDING_WAIT_MS`), same jcode commit. Zero means no wait. Duration overflow and invalid fractions are configuration errors.

If the wait expires, the task is cancelled and an explicit emergency omission entry is returned, retaining the configured emergency message count and tool-call/result pairs. This is not presented as a model-written summary. Provider errors, refusal, truncated output and cancellation are returned, not silently converted into a successful model summary. A tool-safe cut that replaces nothing returns context-full.

Limits: the decision uses reported usage. There is no tokenizer or estimate of a newly added user message, tool result or the first request of a session. An emergency cut keeps a configured count, not a token-counted suffix. A still-oversized kept suffix can be rejected by the provider. Unlike jcode, this package does not shorten stored tool results or repeatedly halve the configured count. These limits are not a claim that every next request is guaranteed to fit.

## Summary request and omission report

`SummaryRequest` builds the prompt. `BoundedSummaryRequest` retains the fixed instructions and previous summary whole and removes oldest serialized conversation text when it would exceed the summary context estimate. `SummaryReport` reports omitted UTF-8 text bytes, including each prompt format's tool-result caps, excluded thinking, and unreadable native items. The controller appends a labelled harness omission notice to the summary when necessary, so omissions are not hidden from the next model or stored history.

The estimate is jcode's four bytes per token (`crates/jcode-compaction-core/src/lib.rs:43`, origin/master `8e11eb23e`). It reserves at least 4,000 tokens as jcode does for summary requests (`crates/jcode-base/src/compaction.rs:1792`), or the requested output allowance when larger. This is not an exact tokenizer or exact wire-overhead measurement. Fixed instructions and previous summary that cannot fit produce an error. A provider context error leaves history unchanged.

The conversation is plain text in one user message, with no tools, as both upstreams do. Text logs of old tool calls cannot dispatch a tool. Stored blob references are never sent. Thinking signatures and native ciphertext do not enter the summary prompt.

| prompt | source | commit |
|---|---|---|
| checkpoint system, first prompt, merge prompt | earendil-works/pi `packages/coding-agent/src/core/compaction/utils.ts` (`SUMMARIZATION_SYSTEM_PROMPT`) and `compaction.ts` (`SUMMARIZATION_PROMPT`, `UPDATE_SUMMARIZATION_PROMPT`, `UPDATE_SUMMARIZATION_INSTRUCTIONS`) | `4839e41d6463d759a92ee55f4ca7c8ae57e52c9c` |
| summary wrapper | Pi `packages/coding-agent/src/core/messages.ts` (`COMPACTION_SUMMARY_PREFIX`, `COMPACTION_SUMMARY_SUFFIX`) | same |
| short prompt | jcode `crates/jcode-compaction-core/src/lib.rs:85` (`SUMMARY_PROMPT`) | `8e11eb23ea33c7b09d43aa9a3b52fa3f0728f39e` |
| short system | jcode `crates/jcode-base/src/compaction.rs:1799` | same |

The constants in `prompt_checkpoint.go` and `prompt_short.go` are copied exactly and carry source comments. The optional conversation-search sentence is held separately and added only when `ConversationSearch` is true. Checkpoint results use Pi's 2,000 UTF-16-unit cap. Short results use jcode's 500-byte cap. Their original layouts remain unchanged before the whole-conversation budget cut. Pi's optional focus instructions and read/modified file lists are not built.

`TestPromptsMatchTheirSources` compares prompt bytes with those exact commits when `ACPHARNESS_PI_REPO` and `ACPHARNESS_JCODE_REPO` point to the source checkouts. Without them it skips rather than substituting hand-written source fixtures.

## Native compaction and portability

`Native` is `Compact(context.Context, provider.Request) (NativeResult, error)`. Runtime assembly wraps the OpenAI client's package-specific return type:

```go
native := compact.NativeFunc(func(ctx context.Context, req provider.Request) (compact.NativeResult, error) {
    result, err := openaiClient.Compact(ctx, req)
    if err != nil {
        return compact.NativeResult{}, err
    }
    return compact.NativeResult{Items: result.Items, Usage: result.Usage}, nil
})
```

`ControllerOptions.Native` maps the actual wire identity to this adapter, returning nil for endpoints without the feature. When native config is enabled a missing resolver is an error. Explicit native requests must receive the loop's normalized `BeforeInput.Request.Items`, including its existing interruption and lost-tool-result handling. The whole returned native window is retained, not just the encrypted item. Empty windows, missing compaction items, invalid item variants, absent opaque content and mismatched provider tags are errors.

`AutoItem` converts a finished native `compaction` item into a compaction effect at that same position in the streamed output. Earlier input/output is represented by it, and later entries remain active. This follows OpenAI's compaction guide for stateless input-array chaining: discard the prefix before the most recent compaction item, not the whole response. This harness does not use `previous_response_id` chaining.

`Items` returns a native window only to its owning provider. For another it returns `UnreadableError`. `EnsurePortable` reconstructs the readable replaced prefix from full history, reuses the latest earlier text summary when applicable, and asks the configured summary model for a real new summary. The ciphertext is not sent. The portable result is appended, not written over the native entry. A new native stream item invalidates older background work. A provider change cancels pending explicit native work for the old provider.

Native auto, explicit and cross-provider fallback have no real upstream recording here. These paths are implemented and source-reviewed, not account-validated. No invented native response was recorded.

## Semantic embeddings

`*embed.Client` satisfies `Embedder`. The controller embeds each newly observed completed request's plain user/assistant text with `EmbedTurn`, retains at most `embedding_history_window` vectors, and clears topic history after a compaction. It never reads gated `Stored` parts. `SemanticKeep` embeds the current goal and candidate older messages and keeps from the earliest sufficiently relevant message. Embeddings errors, wrong dimensions, non-finite values and zero norms are returned. There is no silent switch to proactive when embeddings fail.

The coordinator's whole-config validation rejects semantic mode without `embeddings.base_url`. `NewController` also rejects semantic mode without an embedder. There is no in-process model or second embedding transport.

## Recordings and checks

Provenance: Ollama 0.24.0 on loopback, loaded `llama3.2:3b`, no model pull, settings change, account or external request. The source conversation is the loop lane's real `ReadTurn` recording (`98425d2`, normal-client fixture `49b5c7d`), replayed through its Chat client, router, actual read tool and directory store.

This follow-on made four actual local requests: summary and embeddings, then one approved replacement of each. The first two could not persist because the recorder's parent directory was absent. The replacement run creates the directory and explicitly loads both saved exchanges. Including the earlier three Anthropic-emulation requests, the total is seven, approved by the coordinator. There are no further local requests or retries in these tests unless recording is explicitly enabled.

- `testdata/ollama_summary.json`: real streamed short-prompt summary of that recorded read-tool turn, requested max output 1,024. The summary was accepted only with a complete end-turn stop and nonempty text. Tests replay it through the real Messages client and a context-owned background task and through `Controller.Before`, append to a real store, reopen the store, preserve every original entry, retain an entry appended during summarization, refuse a stale result, and confirm no old tool is dispatched again. Gating the real HTTP transport tests zero wait and caller cancellation/deadline without fabricating provider output.
- `testdata/ollama_semantic.json`: real `/v1/embeddings` request through `src/embed` from `SemanticKeep`. The loaded model returned two vectors of 3,072 dimensions with HTTP 200. The replay checks the measured request path and relevance-keep bounds, not embedding quality or a real topic-shift workload.
- Fixture headers, bodies and responses were checked for home paths, machine/user names and the placeholder key. The key is redacted. The only network location is the permitted loopback endpoint.

Checks:

```sh
nice -n 10 go build ./src/compact/... ./src/config/...
nice -n 10 go vet ./src/compact/...
nice -n 10 go test -race ./src/compact/... -count=1
nice -n 10 mise exec -- golangci-lint run ./src/compact/... ./src/config/...
CGO_ENABLED=0 nice -n 10 go build ./src/compact/...
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 nice -n 10 go build ./src/compact/...
ACPHARNESS_PI_REPO=/home/a/.jcode/scratch/pi-src \
ACPHARNESS_JCODE_REPO=/home/a/repo/github.com/1jehuang/jcode \
    nice -n 10 go test ./src/compact -run TestPromptsMatchTheirSources -count=1
```

## Still untested or owned elsewhere

Native windows preserve each item's origin and copy the native compaction's origin onto the enclosing appended item. Replay requires exact canonical provider, model and account through `Item.OpaqueCompatible`. `Items` and `EnsurePortable` accept an optional target origin. An absent target or legacy nil origin cannot replay opaque context. The controller receives the selected request origin in `BeforeInput.Origin`, regenerates a portable summary from retained source on mismatch, and cancels pending native work when that origin changes. It never infers or backfills a legacy origin.

- Summary quality for continuing real coding work, repeated checkpoint merges, omitted input and long-history native portability. A short local summary is not a quality benchmark.
- OpenAI native automatic/explicit responses and cross-provider fallback on real encrypted history. A real account is needed.
- Anthropic thinking/cache/OAuth behavior. Ollama proves none of it.
- Exact token fitting for arbitrary model tokenizers, multimodal content or huge kept suffixes.
- Real semantic topic-shift quality and the controller's single-turn embedding request as distinct from the recorded relevance-keep request.
- Loop/session/runtime integration, compaction ACP updates and `/compact` are owned by their lanes. This lane supplies the agreed callback and no edits to their paths.
