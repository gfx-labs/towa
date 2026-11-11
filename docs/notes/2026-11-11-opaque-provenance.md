# Opaque transcript provenance

`Item.Provider` remains the canonical wire owner. `Item.Origin` records the exact
request model and configured credential account ID. A present origin requires
both fields. `Item.OpaqueCompatible(provider, origin)` requires nonempty opaque
content and an exact provider/model/account match. It does not infer provenance,
rewrite items or decide whether a plain fallback is safe.

Adapters stamp provenance from the real request when producing opaque items,
including native compaction results. A legacy nil origin is unknown. Optional
opaque metadata may be omitted only where the structural representation remains
safe. Required signatures and native context must fail explicitly or be replaced
by a portable summary generated from retained source. Compaction must preserve
origins inside its native window as well as on its enclosing item.

The account is a configured alias, not a credential fingerprint. Rotating tokens
for the same account does not change compatibility. Rebinding an alias to a
different account is not detectable with this schema. Account aliases must not be
reused to assert compatibility with another identity. Exact model names also do
not detect an upstream model alias being retargeted.

The focused contract test covers matching, mismatched and unknown identities,
invalid partial origins, and the absence of inferred legacy provenance. It does
not prove remote provider acceptance. Adapter and compaction enforcement are
separate integration work.
