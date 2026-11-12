# `openai.compact_threshold` removed

2026-11-12

## Decision

The key `openai.compact_threshold` is gone. It had been kept only so that validation could refuse a value other than zero and name its replacement, `compaction.openai_native_threshold_tokens` with `compaction.openai_native`. It was the only key in `--help` that did nothing.

Setting it is now the same error as any other unknown key:

- flag: `unknown flag --openai.compact_threshold`
- file: `openai.compact_threshold is not a config key`

## Kept

- `--log-level` and `--log-format` are second names of `telemetry.logs.stderr.level` and `.format`, not deprecated ones (`2026-11-11-kong-config.md`).
- A `config.toml` in a config directory is still reported as not read. That is a file name, not a key.
