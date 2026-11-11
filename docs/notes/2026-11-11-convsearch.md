# Conversation search

2026-11-11

`src/tools/convsearch` reads a session's stored history. It is a capability in the style of `src/tools/fsops`: typed input, typed result, a typed `*Error`, no `tools.Tool` and no dialect words. An adapter turns jcode's `conversation_search` input (`query`, `turns`, `stats`) into calls to it.

## What was built

| function | does |
|---|---|
| `Search(ctx, store, access, limits, SearchInput)` | finds the entries that hold a query |
| `Turns(ctx, store, access, limits, TurnsInput)` | returns a range of one session's entries |
| `Stats(ctx, store, access, StatsInput)` | totals for each session and their sum |

All three read through `store.Store`: `List` for the sessions and `History` for the entries, so entries from before a compaction are read. `Search` also uses `OpenBlob`.

jcode's tool was read from `origin/master:crates/jcode-app-core/src/tool/conversation_search.rs` with `git show`, not from the working tree.

## Scope

- `Access` is passed by the caller: the calling session, and `Widest`, the widest scope configuration allows (`self`, `descendants` or `store`).
- `Target.Scope` is what a call asks for. Empty means `descendants`, or `self` when that is all `Widest` allows. Asking for a wider scope than `Widest` is an `*Error` of `KindScopeNotAllowed` that carries both.
- `descendants` is the session and every session below it by `ParentID`, to any depth. A child does not read its parent.
- `Target.Session` reads one session, which must be inside the scope. Otherwise the error is `KindOutOfScope`.
- The calling session comes first in every result, then the others from the oldest to the newest.
- Config key wanted: `conversation_search.scope`, one of `self`, `descendants`, `store`, default `descendants`. It becomes `Access.Widest`.

One `List` call with an empty filter is made per call, for every scope, and the tree is walked in memory. The store has no call that returns one session's `Meta`, and a `List` per session would read every session's metadata once for each of them. On a shared store with many sessions a `descendants` call therefore reads every `meta.json`. See "Requests".

## Matching

- Case is ignored by lowercasing both sides with `strings.ToLower`. This is what jcode does (`to_lowercase` on both). One difference: Rust lowercases a word-final Greek sigma to `ς`, and Go does not, so a query written with `ς` does not match text written with `Σ` here.
- A query of several words matches only where the words stand together in that order, spaces included. jcode does the same: its query is one substring. There is no AND or OR of words.
- A match is found inside one piece of text, never across two. The pieces of an entry are: each text part of a user message or tool result, a resource's URI and its text, a link's name and URI, the model's text, its thinking, a tool call's name, and a tool call's input.
- A tool call's input is searched as the JSON the model wrote, so a quote inside an argument is searched as `\"`. A free-text tool's input is searched as its text.
- jcode searches message text and tool results only. Thinking and tool calls are searched here because the task asked for them.
- A compaction's summary is not searched: it restates entries that are. `Turns` returns it.
- An entry is at most one hit, with the snippet around its first match.

## Bounds

The package has no limit of its own. The caller passes `Limits`:

| field | bounds | wanted config key |
|---|---|---|
| `Hits` | hits of a search, entries of a range | `conversation_search.max_hits` |
| `HitBytes` | bytes of text per hit or entry | `conversation_search.max_hit_bytes` |
| `BlobBytes` | the largest stored blob a search reads, zero for none | `conversation_search.max_blob_bytes` |

- `SearchResult.Omitted` and `TurnsResult.Omitted` count what `Hits` left out. A search still reads every entry in scope to count them.
- A snippet is centred on the match and never cuts a character. `CutBefore` and `CutAfter` say the text goes on. A `Turn` has `Cut` and the whole length in `Bytes`.
- jcode's figures, for whoever sets the defaults: 10 hits, 50 bytes of context on each side of the match, and in a range 1,000 bytes of a message and 200 of a tool result. This package has one `HitBytes` for all of them.

## Stored blobs

A `Part.Stored` is searched when its size is at most `Limits.BlobBytes`. The bound is cheap to state because the reference carries the size, and the read itself is limited to the same number in case the reference is wrong. A blob over the limit, or one that is not UTF-8 text, is not searched and is counted in `SearchResult.StoredSkipped`. `StoredSearched` counts the ones read. A hit inside a blob carries the blob's hash in `Hit.Stored`.

`Turns` does not read blobs. A `Turn` says how many stored parts its entry has.

## Stats

jcode's `stats` reports total turns, active messages in context, whether there is a summary, whether a compaction is running, an estimated token count and context usage. Those come from its compaction manager, which this package does not have. What is here, per session and summed:

| field | is |
|---|---|
| `Entries` | every entry, which is also the end of the positions a range can read |
| `Prompts`, `ToolCalls` | user messages, tool calls |
| `Compactions`, `InContext` | compaction entries, and entries from the latest one on (jcode's "has summary" and "active messages") |
| `Requests`, the token counts | model requests that recorded usage, and their sums |
| `First`, `Last` | the time range of the entries |

Not reported: whether a compaction is running, and context usage as a fraction. Both are state of the running session, not of the store. Token counts are sums of recorded usage and nothing is estimated, so a session with no usage entries reports zero.

## Positions

jcode's turn index counts messages. Here a position counts entries of the full history, from zero, and a usage record or a compaction is an entry. A `Hit` carries its position, and `Turns` with that position returns the same entry.

## Checked

`go build`, `go vet`, `go test -race`, `golangci-lint run` on `./src/tools/convsearch/...` and the `darwin/arm64` build pass. The tests write real entries through `Append` and real blobs through `PutBlob` into a `store/dir` store under `t.TempDir()`. They cover: every searched field, entries from before a compaction, the phrase rule, the hit and snippet bounds with multi-byte text, the three scopes and both scope errors, one subagent session by id, blobs under and over the limit and a binary one, a range with its bounds, and stats over a parent, a child and an empty session.

The snippet test was checked to fail when the offset mapping from the lowered text is removed.

## Not checked

- Only `CompressionNone` of the dir store is used in the tests. The package reads through the `store.Store` interface and does not see the format.
- No measurement on a long history. A search lowercases a copy of each piece of text, and reads the whole scope even after `Hits` is reached.
- The Greek sigma difference from jcode is read from the two languages' documented behaviour, not tested.

## Not done

- No `tools.Tool`, no adapter for jcode's input shape, no config section.
- No embeddings and no ranking: hits come in history order.

## Requests

1. Config keys `conversation_search.scope`, `max_hits`, `max_hit_bytes` and `max_blob_bytes`, as above.
2. A way to get one session's `Meta` from the store, or a `List` that is cheap by `ParentID`, if a shared store grows large. Today every call lists the whole store.
