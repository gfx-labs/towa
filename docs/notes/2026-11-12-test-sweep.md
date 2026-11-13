# Test sweep: prompt, constant and scaffolding tests removed

2026-11-12

Tests that failed on wording changes, compared constants, or only proved
internals and library behaviour were removed. Statement coverage over
`./src/...` and `./pkg/...` went from 68.7% to 68.6%, measured with
`go test -count=1 -coverpkg=./src/...,./pkg/... -coverprofile=... ./...`
before and after. No function lost coverage except the removed code itself
and generated decoders in `pkg/acp/types_gen.go`.

## Removed

| what | why |
|---|---|
| `TestFactoryWriteProcess` and `src/agent/loop/testdata/factory_write_{1,2}.json` | The recordings held the whole system prompt and tool list, so the replay failed on any wording change. Seven of the last eight commits that touched them only recorded them again. The turn through client, router, loop and store is `TestARecordedTurnThroughTheClientRouterLoopAndStore`; the runtime factory with a real turn is `TestARecordedTurnThroughHTTPSProxy`; the binary over stdio is `TestServeAnswersAClientOnStdio` and `tools/conformance`. |
| `TestPromptsMatchTheirSources` | Compared prompt constants with two other repositories at pinned commits. It skipped unless two checkouts were named, so it never ran in CI. The source and commit of each prompt are in the comments of `prompt_checkpoint.go` and `prompt_short.go`. |
| `src/support/httpclient/httpclient_test.go` | Proved that Go's default transport honours `HTTPS_PROXY`, `NO_PROXY` and `SSL_CERT_FILE`. `Transport()` is a clone of `http.DefaultTransport` with nothing of towa's in that path. A real turn through a proxy, and its refusal without the CA, is `TestARecordedTurnThroughHTTPSProxy`. `trace_test.go` stays: it tests the span code towa wrote. |
| `TestConflicts` | Table over a nine-line predicate. `TestCallsRunTogetherUnlessTheyConflict` runs real calls through the dispatcher and covers it. |
| `TestToolCallsLeft` | A subtraction. The limit is exercised through the dispatcher and the batch tests. |
| Wording assertions in `TestPromptToolsSectionFollowsTheToolsOfTheEntry` | It asserted `"- name: "`, `"with name."` and `"no terminal"`. It now asserts only which tool names each entry's sections hold. |

## Production code removed with them

Only tests called these:

- `loop.Stub`, the turn from before any provider existed. Two tests used it as a turn that ends; they now use one written in the test.
- `compact.Entry` and `compact.NativeEntry`. The controller builds its entries itself.

## Looked at and kept

- `TestRequestProvenanceBoundaries`, `TestOpaqueCompatibilityRequiresKnownRequestOrigin`: they decide whether another provider's opaque data is sent. Removing the second lost coverage of `Item.Validate`, so it was restored.
- `src/agent/loop/prepare_test.go`: it reaches into the turn struct, but removing it took `searchTools` from 100% to 22%. Nothing else checks the per-entry search tool swap. Restored.
- The fsync counts in `src/store/dir` (`Stats`): a counter only tests read, but it is the only check that an append is made durable and that `store.fsync: false` syncs nothing.
- `TestSettle`, `TestConversation`, `TestDecide` (router and compaction), `TestBackoff...`: tables over decisions with many cases that the higher tests reach only in part.
- `TestRequestBodyForAPlainEndpoint` and `...ForOpenRouter`: the literal is the provider's wire format, not text of towa's.
- `TestSectionsComeInTheFixedOrder`: the text it compares is the test's own input, and the tag layout is what the model is sent.
- `TestInvalidInputIsAnError`, `TestLaterLayersOverrideEarlierOnes`: the config loader is towa's own.
- `pkg/acp` and `tools/acpgen` tests: an outside caller imports `pkg/acp`.

## Still exported for tests only

`config.Load`, `fsops.Read` and `session.NewAgent` have no caller outside tests and test helpers. Many tests are written on them, so they were left.
