# ACP union variants are encoded once

Date: 2026-11-12

## What changed

Every generated union type (`SessionUpdate`, `ContentBlock`, `ToolCallContent` and the rest, 100 call sites) encoded its object variant through `marshalUnionObject`: encode the struct to JSON, decode that into a `map[string]any`, encode the map. Unions nest, so the text block inside a session update paid for it twice over.

The generator (`tools/acpgen/emit/union_codec.go`) now emits `json.MarshalEncode(enc, &v, jsontext.EscapeForHTML(true))` for an object variant, as it already did for the scalar ones, and `marshalUnionObject` is gone.

Two things differ on the wire:

- Members are written in the struct's order, not sorted by the map. The generator sorts property names before it emits a struct, so the order is the same in practice, and ACP does not depend on it.
- An integer above 2^53 keeps its value. The map step read every number as a float64, so 9007199254740993 was written as 9007199254740992. `TestUnionVariantKeepsIntegersAboveFloatPrecision` covers that.

## Measurements

`go test -run xxx -bench Encode -benchmem -count=3 ./pkg/acp/`, same machine, before and after:

| Benchmark | Before | After |
|---|---|---|
| `EncodeSessionUpdate` | 7,320 to 7,432 ns, 2,568 B, 42 allocs | 1,352 to 1,404 ns, 560 B, 5 allocs |
| `EncodePromptRequest` | 67,906 to 71,637 ns, 56 to 61 KB, 62 to 63 allocs | 9,148 to 9,812 ns, 0.3 to 3.5 KB, 6 to 7 allocs |

Thirty-two live turns against `openai/gpt-oss-20b` (65,536-token context, the user's rules), CPU profile over the turns, one run of each build. The model chooses its replies, so the runs differ in work.

| | Before (`a799c49`) | After |
|---|---|---|
| Updates sent | 3,240 | 3,748 |
| CPU in `loop.(*turn).update` | 160 ms | 50 ms |
| CPU samples, whole run | 700 ms | 410 ms |
| Allocated under `session.(*Agent).send` | 7.02 MB | 2.00 MB |

The profile samples every 10 ms, so the smaller figures are a handful of samples each.

## The envelope and the params copies

After the change above, one session update still cost six allocations through the generated wrapper. Four came from the JSON-RPC envelope: the `outboundCall` struct, the params value boxed into its `any` member, and two copies `encoding/json/v2` makes of a non-pointer value it finds in an interface (`makeInterfaceArshaler` and `marshalValueAny` each call `newAddressableValue`).

`Connection.write` now writes the envelope by hand into the pooled buffer (`jsonrpc`, `id`, `method`, then the `params`, `result` or `error` member name) and hands only the value to `json.MarshalWrite`. The generated wrappers pass `&params`, so the encoder is given a pointer and copies nothing. The bytes on the wire are the same.

A session update sent through `AgentSideConnection.SessionUpdate`, measured with a scratch benchmark:

| | Before | After |
|---|---|---|
| Allocations | 6, 736 B | 3, 304 B |

`BenchmarkEncodeSessionUpdate`, which passes params by value to `SendNotification`, went from 5 allocations and 560 B to 3 and 304 B.

The three left are the params struct itself, which escapes to the heap because it is passed as `any` (176 B), and the generated union encoders' copies of the `SessionUpdate` and `ContentBlock` variants, made to set the discriminator (80 B and 48 B).
