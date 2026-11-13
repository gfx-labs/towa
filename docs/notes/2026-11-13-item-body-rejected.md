# An item's body as one interface value: tried and reverted

Date: 2026-11-13

## What was tried

`transcript.Item` has ten pointer fields of which exactly one is set. Commit `9044401` replaced them with one field, `Body`, of a sealed interface type the ten body types implemented, with hand-written `MarshalJSONTo` and `UnmarshalJSONFrom` to keep the stored shape, and the `gochecksumtype` linter for exhaustive type switches. It was reverted the same day.

## Why it was reverted

It made the session log slower to write and read. `encoding/json` cannot encode or decode an interface field by itself, so the codec went through a second struct with the ten pointers: one copy of the body into it on the way out, one copy out of it on the way in, each a heap allocation, plus the boxing of the body into the interface.

A scratch benchmark of the entries of one turn (a user message, thinking, a tool call, a 1.7 KB tool result, text, usage: six entries), encoded as the store does and decoded with `Validate`, three interleaved runs of each build on the same machine:

| | Ten pointers (`f67bc15`) | One interface (`9044401`) |
|---|---|---|
| Encode, time | 8,950 to 9,245 ns | 10,310 to 10,590 ns |
| Encode, allocation | 2,307 B, 12 allocs | 2,610 B, 24 allocs |
| Decode, time | 12,531 to 12,589 ns | 13,528 to 14,205 ns |
| Decode, allocation | 5,506 B, 19 allocs | 6,195 B, 31 allocs |

That is two more allocations for every item in each direction, and about 10 to 15% more time. A live turn profile of the two builds was started and not finished.

What it gained: `Item` shrank from 128 to 64 bytes, an item with two bodies could not be written, and readers were type switches.

## If it is tried again

The cost is in the codec, not in the interface. A codec that writes the body's member name and then encodes the body in place, and that reads the member name before decoding into the right type, would not need the second struct. That is a hand-written encoder and decoder for `Item`, and it has to be measured against the ten pointers before anything else is converted.
