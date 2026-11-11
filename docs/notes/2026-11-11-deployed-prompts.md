# Deployed Codex prompts

`src/prompts.Lookup(dialect, model)` returns instruction text from the canonical
capture at `prompts/codex/ce0323f2/`, commit
`ce0323f2f2d3b25e82806cd555237da3db4e1897`. It accepts only the exact catalog
slug. Provider prefixes, unknown models and unavailable variants are errors.
Claude and generic dialects are not deployed by this change.

## Selection evidence

The captured `source/codex-rs/models-manager/models.json` maps each model's
`slug` to its own `model_messages.instructions_template`. The capture manifest
records each extracted template's JSON pointer and SHA256. The deployment
generator verifies the catalog and template hashes, the pointer and the exact
string equality before emitting Go string literals. Generated comments retain
the source pointer and hashes. Lookup needs no checkout or filesystem access.
`Tools` means extra system instruction text. It is empty because every captured
catalog entry has `model_messages.tools: null`. Tool request schemas remain in
the dialect packages.

The captured `source/codex-rs/core/src/tasks/mod.rs:85-98` selects a developer
interruption for effective multi-agent version v2, and a contextual user
interruption otherwise. Its lines 103-120 render those roles. The catalog's
`multi_agent_version` is v2 for eight models, v1 for `gpt-5.6-luna` and
`codex-auto-review`, and null for `gpt-5.5`. Pinned upstream
`codex-rs/core/src/config/mod.rs:1641-1667` resolves explicit feature overrides,
then model metadata, then the feature default (v1 or disabled). Both possible
defaults for the null catalog value select contextual user text. This is a
catalog lookup without feature overrides, with interruption messages enabled.
`InterruptionRole` preserves the capture's `user` or `developer` role. Callers
must not silently render developer text as a user message.

`System` is the captured template unchanged. This API does not assemble runtime
permission, collaboration, environment or persistent-mode messages. It does not
claim equivalence to the upstream's fully assembled request. Persistent variants
need an explicit API before deployment, rather than aliasing the normal model.

## Regeneration and checks

Run `go generate ./src/prompts`. Run
`go run tools/capture/prompts.go -check` from the repository root to check the
same hash-verified output without writing. The Go helper does not change the
canonical root capture or the Python capture scripts.

The retained test compares every deployed model against the real canonical
catalog and interruption captures, including message roles. Invalid lookup
names in that test are composed inputs. No provider responses or upstream CLI
outputs were synthesized. No scaffolding tests were added.
