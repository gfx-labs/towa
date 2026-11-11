# Runtime integration decisions

2026-11-11.

- Session resources belong to a session runtime opened on new, load, resume or fork. The process owns provider clients, the store, MCP pool and subagent admission. A runtime owns its loop, frozen MCP tools and file-read fingerprints. It closes after its turns have stopped, before the process pool closes.
- MCP discovery runs once for each session runtime. A model switch rebuilds dialect tools and checks collisions against the frozen MCP definitions, without rediscovery.
- Extras use section switches: `tools.batch.enabled`, `context.skills_enabled`, `webfetch.enabled` and `conversation_search.enabled`. Jev is disabled by an empty route. All enabled defaults remain subject to successful construction, never placeholder tools.
- Semantic compaction requires an embeddings endpoint. The single native-compaction threshold is `compaction.openai_native_threshold_tokens`. Nonzero `openai.compact_threshold` is rejected with a migration message instead of silently ignoring it.
- `subagents.models` names configured models. A missing name is a configuration error, not a fallback to the parent or default.

## Image dependency

Approved `golang.org/x/image v0.48.0` for WebP decoding and resampling, with standard-library encoders. PNG, JPEG and GIF decoding alone is already in the standard library, but neither WebP nor a resampling scaler is. The isolated fixture and allocation measurements are in `2026-11-11-claude-state.md`. ApproxBiLinear had less temporary allocation than CatmullRom in that check. Those figures are not a harness-wide memory measurement or proof of sharp-compatible output.

`go get golang.org/x/image@v0.48.0` added that module and selected `golang.org/x/text v0.44.0` and `golang.org/x/sync v0.25.0`. No other root requirement changed. The tool-only modules selected in the isolated full-graph probe did not become root requirements. Image normalization remains owned by the tools lane. Its runtime and format limits must be verified before claiming image parity.

## Verification boundary

Commit `29369ae` passed a clean-checkout build, vet, root race tests, generator tests, external SDK conformance, lint, format, generation cleanliness, Darwin cross-build/vet and static Linux build. CI run `43041287833` passed. This verifies assembled packages and recorded turn replay, not real-provider OAuth behavior or the not-yet-wired CLI runtime. An earlier clean check caught an outdated context-config expectation and lost MCP login-required classification after connection death. Their fixes were included before publication.
