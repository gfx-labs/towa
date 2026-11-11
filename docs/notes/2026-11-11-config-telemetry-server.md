# Config, telemetry and the stdio server stub

2026-11-11

This finishes step 1 of the implementation order. `acpharness` (or `acpharness serve`) serves ACP on standard input and output with `initialize`, `session/new`, `session/prompt`, `session/cancel` and `session/close`. The turn is `loop.Stub`, which has no model: it says so and repeats the prompt's text.

## Config (`src/config`)

- Layers, later over earlier: defaults, `/etc/acpharness/config.*`, `$XDG_CONFIG_HOME/acpharness/config.*` (or `~/.config`), files in `ACPHARNESS_CONFIG` then `--config`, `ACPHARNESS_*` environment, flags, `--set key=value`.
- `Config` holds only the sections some code reads. Today that is `telemetry`. A section is added with the code that uses it.
- A key that `Config` does not have is an error from any layer, including a mistyped environment variable. So is a value of the wrong type. koanf's default decoder is weakly typed and ignores unknown keys, so `Load` passes its own decoder config: `ErrorUnused`, and only string-to-type hooks (environment and `--set` values are text).
- A listed file that is missing is an error. A system or user directory without a config file is not. Two config files in one directory is an error, since neither can be said to win.
- `acpharness config` prints each key, its value and the layer that set it.
- Flags reach config through koanf's `cliflagv3` provider, which yields only flags that were given. `config.FlagKeys` maps a flag name to its key.

## Telemetry (`src/telemetry`)

- `telemetry.otel = false` (the default): the tracer and meter are no-ops. No exporter, goroutine or timer exists.
- `telemetry.otel = true`: traces, metrics and logs go out over OTLP/HTTP. The endpoint, headers, batch sizes and intervals are the SDK's standard `OTEL_*` variables. We add no settings of our own on top, so there is one place to configure them.
- The stderr log and the exported log each have their own `enabled` and `level`. The stderr line carries `trace_id` and `span_id` when the record's context has a span.
- `session/prompt` starts a span whose parent is the `traceparent` in the request's `_meta`, the key ACP reserves for W3C trace context.

## Sessions (`src/session`)

- A session runs one turn at a time. A cancel or a prompt cancels every prompt of that session that arrived before it, by `acp.Arrival`. So a new prompt cancels the running turn and starts once it has stopped, and a cancel sent between two prompts applies to the first only.
- A cancelled turn answers with the `cancelled` stop reason, also when the turn returned an error, as the prompt-turn spec requires.
- Only what is implemented is advertised: `sessionCapabilities.close`. `session/new` with MCP servers, extra directories, a system prompt or config options is rejected with `-32602`, since accepting them would drop them silently. Each is lifted when its step lands.
- Requests before `initialize` get `-32600`. An unknown session gets `-32002` with `data.sessionId`.

## Shutdown

SIGINT or SIGTERM cancels `Serve`, which cancels running turns, writes their answers and returns. Checked by hand: with standard input held open, the process exits on SIGTERM.

## Measurements

Linux amd64, Go 1.27.3, `CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' ./cmd/acpharness`, commit after `ac8ed24`:

- Binary: 17,887,392 bytes.
- One idle process (`acpharness --log-level error`, stdin open, no session, OTel off), from `/proc/<pid>/status` and `smaps_rollup` after 1 s: RSS 17,276 kB, of which 4,588 kB is anonymous memory and 12,688 kB is the binary's own pages. The binary's pages are shared between processes that run the same file, so each further process costs about the anonymous part. 7 threads.

These are a first reading, not the footprint benchmark of step 11. The size of the OTel and koanf dependencies in those numbers has not been separated out yet.

## Dependencies

- `github.com/knadh/koanf/v2` with `providers/{confmap,env/v2,file,cliflagv3}` and `parsers/{toml/v2,yaml,json}`: layered config, chosen in the design. Each provider and parser is its own small module.
- `github.com/go-viper/mapstructure/v2`: koanf's decoder. Imported directly only to pass a strict decoder config.
- `github.com/urfave/cli/v3`: commands and flags, chosen in the design.
- `go.opentelemetry.io/otel` with `sdk`, `sdk/metric`, `sdk/log`, the three `otlp*http` exporters and `contrib/bridges/otelslog`: the versions jobshop pins (v1.47.0, v0.23.0, v0.21.0), so both export the same way.
