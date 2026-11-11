# Config validation

Date: 2026-11-11

The user requested `github.com/go-playground/validator/v10`. The coordinator pinned v10.30.6. It replaces repeated field validation across `src/config`, not the configuration loader or its schema.

One package-private validator is configured at initialization with required-struct handling, koanf field names and a finite-float rule. Registrations do not change after initialization. Exported `Provider.Validate`, `Retry.Validate`, `Listen.Validate`, `Compaction.Validate` and `Subagents.Validate` keep their signatures and validate independently of loading a file.

Standard tags cover required fields, ranges, enums, slice elements, uniqueness, mutually exclusive context sources, account-kind constraints, optional embeddings, TLS field dependencies and comparisons between fields. Error paths use configuration names and indices. Field-validation errors report the rule without printing the value, which may contain credentials.

Explicit checks remain for URL policy, account filename safety, listener exposure and permissions, transport-specific MCP fields, duplicate named entries, model-chain parsing and references, Jev routing, semantic-mode dependencies, deprecated fields and millisecond-to-duration overflow. Provider URLs still accept HTTP/HTTPS with query or fragment, whereas embeddings URLs still reject query and fragment. Disabled sections retain their previous numeric validation and conditional behavior. Native compaction disabled still ignores its threshold.

Bounded float tags reject NaN and infinity. The finite rule also rejects them in unbounded retry multipliers and OpenRouter routing prices, throughput and latency. This closes an old hole where comparison-only checks accepted NaN or positive infinity. No new numeric limits or defaults were introduced.

## URL error disclosure followthrough

All four `net/url` callers in config (Provider, Embeddings, Jev and MCP) now report only the field path and a fixed policy reason. They neither interpolate the supplied URL nor wrap `url.Parse` errors, which include the original URL. Acceptance conditions, schemes, query/fragment policies and exported APIs are unchanged. The existing invalid-input loader table adds credential/query sentinel checks for each caller, including malformed escape parse failures. Inputs are authored TOML and `--set` values, with no provider requests. The same race, vet, lint and platform checks listed below were rerun for this followthrough.

## Checks

The config loader tests cover invalid ranges and relations, disabled-gate validation, NaN/infinity, duration overflow, mutually exclusive context sources and empty hook tool selectors. One additional TOML loader case covers zero subagent limits, disabled native compaction, optional embeddings, private HTTP Jev, provider URL policy and a file-only context section. These are authored configuration inputs, not provider recordings or simulated upstream responses. Existing listener and model tests remain. No scaffolding tests or live requests were used.

Validation commands run with `nice -n 10`:

- Race tests and vet for `src/config`, `src/subagent`, `src/compact`, `src/listen`, `src/cli`, `src/provider/chat` and `src/provider/router`, covering every direct exported validation caller.
- `golangci-lint run ./src/config/...`.
- `go build ./...`, `CGO_ENABLED=0 go build ./...`, and `GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build ./...`.
- `git diff --check -- src/config`.
