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

## Checks

Scoped provider build, vet, race tests and lint pass. Static Linux and
Darwin/arm64 builds pass. The retained request-boundary test uses no key or
live model. Existing provider recordings are unchanged. No scaffolding tests
were added. OAuth, custom-tool response parsing and WebSocket operation still
have no real recording in this repository.
