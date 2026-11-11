# Provider provenance and API boundaries

Date: 2026-11-11

## Context names

`provider.ContextNamer` is an optional interface with
`ContextName(auth.Credential) (string, error)`. The loop asks the actual router
entry's provider with the credential of the request. A configured account or
provider ID is not a substitute for transcript provenance.

The Responses client shares its identity selection with endpoint selection:
API-key requests use the client's configured provenance, OAuth requests use
`openai-codex`. Messages uses `Config.Provider`. Chat uses its endpoint's
configured `Provider.ID`. Unknown credential schemes return a typed
`provider.Error` with class `Fail` and reason `request`.

Existing real recorded exchange tests now compare `ContextName` with the
`Item.Provider` values actually emitted by each parser. The current recordings
exercise API-key paths, not Codex/OAuth stream identity.

## Tool-call provenance

A JSON string in `ToolCall.Input` does not establish that a call was a Responses
custom tool. It can also hold malformed function arguments from another API.
Responses now rejects a string input unless this endpoint's opaque metadata
explicitly records `function_call` or `custom_tool_call`. Own call types and the
corresponding output item type remain unchanged. Foreign opaque metadata is
never read to infer the type.

## Message roles

The captured Codex v2 interruption has `role=developer`. `transcript.User.Role`
is supplied by the transcript/history lane in `c3c1956`. Empty role remains
`user`. Chat and Responses preserve an explicit developer role. Messages has no
corresponding message role and reports a typed invalid request before HTTP,
rather than silently sending a lower-priority user message.

The request-boundary test uses real clients and a local HTTP endpoint. The
endpoint records outbound JSON and sends only HTTP 503, not a fabricated
provider response. It checks role preservation/rejection, unknown scheme
rejection without HTTP, and foreign-string rejection with own call-type
preservation. This test does not establish successful live provider behavior.

## Native tool definitions

`provider.Tool.NativeAPI` is a typed API tag. Its values are `responses`,
`messages` and `chat_completions`. A native definition requires the receiving
client's tag. Untagged or foreign definitions fail before HTTP, as does a tag
without a definition. There is no tool-name inference or JSON rewriting.

The boundary test reads the actual captured Codex `apply_patch` definition in
`src/tools/codex/specs/apply_patch.json`. It proves that Responses sends that
definition unchanged with its tag, and that Messages/Chat reject the Responses
tag before HTTP. It also exercises every other foreign tag and an absent tag.
It does not claim that an incorrectly tagged definition is semantically valid
for its named API. The producer owns accurate tagging.

## Exact request origin

`d87e607` adds `transcript.Origin{Model, Account}` and
`Item.OpaqueCompatible`. Adapters now use the actual wire request's model and
credential account in this check, in addition to canonical `Item.Provider`.
Legacy nil origins stay unknown. There is no inferred or backfilled identity.

All actual stream outputs carry this origin, including auto-compaction native
items. Explicit Responses `Compact` results stamp each retained item too.
Responses stamps before copying a finished item into the WebSocket continuation
cache, whose local rendering uses the same target origin. Actual `Stream` and
`Compact` calls reject an empty credential account before HTTP, since a partial
origin cannot be stored as a valid transcript item.

Replay policy:

- Portable text, user content and structural tool results keep their plain
  fields, without incompatible opaque metadata.
- Incompatible or legacy thinking is omitted, not sent unsigned or converted to
  a user message.
- Responses and Messages native items require exact provider/model/account and
  nonempty opaque content. Incompatible native items fail before HTTP.
- Chat has no native response-item format and rejects every native item.
- Responses and Chat reject opaque-bearing tool calls on any origin mismatch.
  Their matching calls keep their recorded call type/extensions. Messages
  produces no opaque tool-call format and rejects any such extension, including
  a matching one, rather than silently losing a required signature.
- Ordinary tool calls with no opaque metadata retain the existing structural
  encoding. A string input still needs explicit compatible Responses call-type
  metadata, as described above.

All adapter failures above are `*provider.Error`, class `Fail`, reason
`request`, status zero when no HTTP request was sent. No new reason value or
error-text matching contract was introduced. Native-compaction recovery remains
owned by `compact.ItemsWithOrigin` and its typed unreadable-window error, not a
provider retry or silent deletion.

The existing real recordings verify origin stamping and that the next request
can be built with emitted items. The request-boundary test checks legacy nil,
changed model, changed account and changed provider. It deliberately uses an
untrusted opaque JSON string, not a fabricated provider response, to prove that
incompatible metadata is not decoded or transmitted. Required native/tool-call
items fail without HTTP, while plain text survives and incompatible thinking
is absent. No successful native/Compact or WebSocket exchange has been recorded,
so those stamping paths are inspected and built, not claimed replay-verified.
The same retained boundary test fails against an overlay of the actual
pre-guard `eb54443` Responses request/Compact/WebSocket builders: the legacy
same-provider native input reaches HTTP and returns retryable 503 instead of
failing as an invalid request. The shared checkout was not reverted.

## Checks

Scoped provider build, vet, race tests and lint pass. Static Linux and
Darwin/arm64 builds pass. The retained request-boundary test uses no key or
live model. Existing provider recordings are unchanged. No scaffolding tests
were added. OAuth, custom-tool response parsing and WebSocket operation still
have no real recording in this repository.
