# Compaction: the decision, the summary request and the appended entry

2026-11-11

`src/compact` and `src/config/compaction.go`. Nothing calls the package yet, and no request was sent to any provider. The quality of a summary written with either prompt is untested.

## What was built

| file | what |
|---|---|
| `src/config/compaction.go` | `Compaction`, `compactionDefaults`, `validate` |
| `decide.go` | `Decide(Input) (Decision, error)`, `ContextTokens`, `Budget` |
| `cut.go` | `Cut`: where a compaction divides the active entries. `Messages` |
| `summary.go` | `SummaryRequest`: the `provider.Request` asking for the summary. `SummaryText`: the summary from the response, or why it cannot be used |
| `prompt_checkpoint.go`, `prompt_short.go` | the copied prompts with their sources |
| `compact.go` | `Entry`, `NativeEntry`, `EmergencySummary`, `Read`, `Items`, the `Native` interface |
| `semantic.go` | the `Embedder` interface, `TopicShift`, `SemanticKeep` |

## How the loop would use it

1. After a model request: `Decide` with the mode, the model's context window, that request's usage, the count from `Messages(view.Active)` and, for proactive and semantic, the per-turn history the caller keeps.
2. On `Background` or `Emergency`: `Read(entries)` for the active entries and the earlier summary, then `Cut(view.Active, decision.Keep)` (or the count from `SemanticKeep`).
3. `Background`: send `SummaryRequest(...)` to the summary model, pass the response's items and stop reason to `SummaryText`, append `Entry(...)`. With `Decision.Native`: call the `Native`, append `NativeEntry(...)`.
4. `Emergency`: append `Entry(...)` with `EmergencySummary(...)` and `ModeEmergency`. No model is asked.
5. Before every request: `Items(entries, providerName)` in place of the plain list of items, and then the loop's `Conversation` as today.

## Append only

The package has no function that takes entries and returns fewer of them for storing. `Read` and `Items` return views. `Entry` and `NativeEntry` return one new entry. `TestItemsAfterCompactions` checks that after two compactions every earlier entry is still in the slice and unchanged.

A compaction's `FirstKept` is the id of the first entry still sent. The nil id means nothing before the compaction is kept. An id the transcript does not hold before the compaction is an error, not an empty session.

## The decision

Follows jcode's `CompactionManager` at origin/master 8e11eb23e (`crates/jcode-base/src/compaction.rs`: `ensure_context_fits` 981, `should_compact_with` 881, `anti_signals_block` 508, `should_compact_proactively` 557, `should_compact_semantic` 604).

- The context size is the last request's `InputTokens + CacheReadTokens + CacheWriteTokens`. The budget is the model's window, or `max_context_tokens` when that is smaller.
- At `emergency_threshold` of the budget the answer is `Emergency`, or `Stop` when the mode is `off` or there are no more messages than the emergency keep. This is checked first, also while a compaction is running.
- `reactive`: `Background` at `reactive_threshold` of the budget or at `reactive_threshold_tokens`, whichever comes first.
- `proactive`: `Background` when the context plus `lookahead_turns` times the EWMA of its growth reaches `proactive_threshold` of the budget. Held back below the floor, with fewer than `min_samples` sizes, when the last `stall_window` sizes show no growth, and for `min_turns_between` turns after a compaction.
- `semantic`: the same holds, then a topic shift, then the proactive projection.
- Native: when the provider can compact itself, `auto` returns `None` (the server compacts in the stream) and `explicit` returns `Background` with `Native` set at `openai_native_threshold_tokens`. `off` falls through to the mode.

`TestDecide` is a table of 22 cases over these rules.

Differences from jcode:

- jcode estimates the context from message text when it has no usage. `Decide` takes only the reported usage, so nothing is decided before the first request of a session. A loaded session has its last `Usage` entry.
- jcode waits a bounded time for a running compaction before it cuts at the emergency threshold. `Decide` answers `Emergency` at once. The wait is the caller's to add.
- jcode's emergency cut also shortens long tool results and halves the kept count until the context fits. Here the emergency keep is one configured count. Shortening a result would rewrite an entry.
- jcode counts messages and calls them turns. The config keys here say messages. A message is a run of items of one role.

## The two prompts

| prompt | source | commit |
|---|---|---|
| `checkpoint` system, first prompt, merge prompt | `packages/coding-agent/src/core/compaction/utils.ts` (`SUMMARIZATION_SYSTEM_PROMPT`) and `compaction.ts` (`SUMMARIZATION_PROMPT`, `UPDATE_SUMMARIZATION_PROMPT` with `UPDATE_SUMMARIZATION_INSTRUCTIONS`) in earendil-works/pi | 4839e41d6463d759a92ee55f4ca7c8ae57e52c9c |
| the wrapper a summary is sent in | `packages/coding-agent/src/core/messages.ts` (`COMPACTION_SUMMARY_PREFIX`, `COMPACTION_SUMMARY_SUFFIX`), same commit | |
| `short` prompt | `crates/jcode-compaction-core/src/lib.rs:85` (`SUMMARY_PROMPT`) in 1jehuang/jcode, read with `git show origin/master:` | 8e11eb23ea33c7b09d43aa9a3b52fa3f0728f39e |
| `short` system prompt | `crates/jcode-base/src/compaction.rs:1799`, same commit | |

`TestPromptsMatchTheirSources` reads those files at those commits with `git show` and checks that each constant is in its file byte for byte. It needs the two checkouts, which a clone of this repo does not have, so it runs only when they are named and skips otherwise:

```
ACPHARNESS_PI_REPO=~/.jcode/scratch/pi-src ACPHARNESS_JCODE_REPO=~/repo/github.com/1jehuang/jcode \
    go test ./src/compact -run TestPromptsMatchTheirSources
```

It passed on 2026-11-11 against both.

The sentence about searching: jcode's prompt ends with "You can search the full conversation later if you need exact error messages or code snippets." It is held apart as `searchSentence` and added only when `SummaryInput.ConversationSearch` is set. With the flag set, the `short` prompt is jcode's whole. Pi's prompt has no such sentence, so with the flag set the same sentence follows the checkpoint prompt after a blank line. That one addition is ours and not Pi's.

The conversation is sent as text in one user message, as both upstreams do, so the model does not carry on with it. The text layout follows each upstream: Pi's `[User]:`, `[Assistant]:`, `[Assistant tool calls]:`, `[Tool result]:` inside `<conversation>` tags with tool results cut at 2000 UTF-16 units, and jcode's `**User:**`, `[Tool: name - input]`, `[Result: ...]` with tool results cut at 500 bytes. Thinking is left out by both. These layouts were written from reading the two sources and are not compared with them by a test.

Not ported: jcode cuts the whole prompt to the summary model's context window (`compaction.rs:1792`). `SummaryRequest` does not, so a replaced part larger than the summary model's window fails at the provider with a context-full error. Pi's `Additional focus:` custom instructions and its file lists (`<read-files>`, `<modified-files>`) are not built.

A later summary is given to the model as a user message between Pi's prefix and suffix, for both prompts. jcode uses a `## Previous Conversation Summary` heading instead. One wrapper was chosen so that `Items` does not depend on which prompt wrote the summary.

## Native compaction

`compact.Native` is `Compact(ctx, provider.Request) (NativeResult, error)`. `(*openai.Client).Compact` returns `*openai.Compacted`, a type of that package, so it does not satisfy the interface directly. The wiring wraps it:

```go
native := compact.NativeFunc(func(ctx context.Context, req provider.Request) (compact.NativeResult, error) {
	compacted, err := openaiClient.Compact(ctx, req)
	if err != nil {
		return compact.NativeResult{}, err
	}
	return compact.NativeResult{Items: compacted.Items, Usage: compacted.Usage}, nil
})
```

`NativeEntry` stores the provider's items as JSON in the entry's `Opaque` with `Provider` set, and no summary text, because the provider's summary is encrypted. `Items` gives them back to that provider only. For any other provider it returns `*UnreadableError`: the replaced entries are still in the store, so the caller can summarise them with a model. That fallback is not built.

## Semantic mode

Built: the `Embedder` interface (`Embed(ctx, []string) ([][]float32, error)`, which `*embed.Client` has), `TopicShift` over the turn embeddings, and `SemanticKeep`, which widens the kept part back to the earliest older message whose similarity to the current goal reaches `relevance_keep_threshold`.

Missing:

- Nothing produces `Input.Embeddings`. After each turn the caller has to embed the first `embed_max_chars` characters of the turn's assistant text and keep the latest `embedding_history_window` vectors. jcode caches these (256 entries). There is no cache here.
- `SemanticKeep` is not tested: a test needs an embeddings endpoint or a recording of one, and a hand-written vector table would be a stand-in. `TopicShift` is covered through `TestDecide` with literal vectors.
- With no embeddings endpoint configured, semantic mode decides exactly as proactive mode. Config does not refuse `mode = "semantic"` with an empty `embeddings.base_url`, because that needs the whole `Config`: see the requests.

## Config

Every default is jcode's, cited by file and line in `src/config/compaction.go`. None is unsourced, so none is required. Two keys have no jcode setting behind them and take jcode's constant as the default: `proactive_threshold` (`COMPACTION_THRESHOLD`, 0.80) and `history_window` (`TOKEN_HISTORY_WINDOW`, 20). `summary_prompt` defaults to `checkpoint` because `docs/design.md` says so.

The defaults were decoded through koanf into `Compaction` and passed `validate` in a throwaway test, which was deleted.

## Checks

```
nice -n 10 go build ./src/compact/... ./src/config/... && go vet ./src/compact/...
nice -n 10 go test -race ./src/compact/...
nice -n 10 golangci-lint run ./src/compact/...
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build ./src/compact/... ./src/config/...
```

All pass. `golangci-lint run ./src/config/...` reports `compactionDefaults` and `Compaction.validate` as unused until they are wired.

## Not done

- No summary was requested from a model. Whether either prompt gives a summary a session can continue from is untested.
- Nothing calls the package. The loop's `Conversation` still returns an error for a `Compaction` item, which is right as long as it is given `Items`' result, which holds none.
- Reporting a compaction to the client (`compaction_update`) and the `/compact` command are the session's.
- Compaction entries from a native `auto` compaction: the server's compaction item arrives in the stream as a `Native` item of kind `compaction`. Nothing here turns it into a `Compaction` entry, so the entries before it are still sent.
- The summary request is not cut to the summary model's window.
- No wait for a running compaction at the emergency threshold.
