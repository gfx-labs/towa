# Config on kong, with YAML files

Date: 2026-11-11

The user asked for kong as the command line, with the config as one nested struct that kong fills, our own loader for the files, and `goccy/go-yaml` so that YAML and JSON both load. The requirement behind it: every config key must be visible in `--help`. On master the only flags were `--config`, `--set`, `--log-level` and `--log-format`. Every other key was reached through `--set key=value`, a file or the environment, and `--help` listed none of them.

## What changed for a user

- Every key is a flag with the key's own name: `--limits.max_wall=30m`. `acpharness --help` is 462 lines, and each of the 153 keys that `acpharness config` prints was found in it as a flag.
- `--set` is gone. `--set limits.max_wall=30m` is now `--limits.max_wall=30m`.
- `--log-level` and `--log-format` stay as second names of `telemetry.logs.stderr.level` and `.format`.
- Config files are `config.yaml`, `config.yml` or `config.json`. TOML is no longer read. A `config.toml` left in a config directory is an error that names it, and so is a listed `.toml` file. It is not skipped, because a skipped file would silently drop every setting in it.
- Environment names did not change: `ACPHARNESS_LIMITS__MAX_WALL`.
- A list table (`accounts`, `providers`, `hooks`, `mcp.servers`, `context.sections`) is one key. In a file it is YAML. As a flag or an environment variable it is one line of YAML. A later layer that sets it replaces the earlier layer's list whole, as on master (checked with `hooks` in two files on both builds).
- `models` is a table of named entries, and each file adds to it by name, as on master: two files that each define one model give two models, and a later file can change one member of an earlier file's model. The first version of this change replaced `models` whole with the last file's. The comparison with master on two `--config` files found it, and `TestEachFileAddsToTheModels` fails on that version.
- `models` as a flag or an environment variable is the whole table as one line of YAML, and replaces what the files gave. On master `--set models.opus.context_window=5` and `ACPHARNESS_MODELS__OPUS__CONTEXT_WINDOW=5` set one member of one model. Both are now errors: the name is not a key. The user has not confirmed this.
- A file's text is read the way a flag's text is. So `yes` and `no` now set a boolean in a file, which master refused, and a quoted number still sets a number, as on master.
- `--manual`, `--device` and `--code` of `login` exclude each other in the parser.

The resolved defaults are the same. A dump of every field of the default `Config` from master `3c065d4` and from this change is 153 fields each and differs in one: `Subagents.Models` is nil where it was an empty slice. Its only reader ranges over it.

## How it is built

`config.Config` is the struct. A field's `name`, `default`, `help` and `validate` tags are the key, its default, its `--help` line and its constraints. The defaults maps are deleted, so a default is written once.

`config.Parse` builds the kong parser, and kong's `BeforeResolve` hook reads the files and the environment once, after the command line is parsed (so the `--config` files are known) and before any key has a value. A kong resolver then serves each key's value from the highest layer that set it, except `models`, which it builds from every file in order. kong applies flags over resolvers and resolvers over defaults, so the layering between flags, the rest and the defaults is kong's own.

The loader adds what kong does not have:

- An unknown name is an error in every layer. kong reports an unknown flag. The loader checks every name in every file and every `ACPHARNESS_*` variable against the keys, and names the file or the variable.
- A value in a file that is not text must have the key's type. YAML reads an unquoted `0600` as the number 384, so a number is not taken for a key that holds text, and a bare number is not taken for a duration. A key that holds a list must be given a list.
- A file nests its sections. A dotted name (`limits.max_wall: 30m`) is an error: it is the flag's spelling, and it would otherwise have been accepted as known and then never read.
- A file with a second YAML document, a missing listed file, two config files in one directory and an extension other than `.yaml`, `.yml` or `.json` are errors.
- A key without a snake_case `name` tag or without visible help is refused when the parser is built. That is what keeps every key in `--help`.

Tables decode through `encoding/json/v2` with unknown members rejected and durations as text.

`Loaded.Values` holds each key's value as JSON text and `Loaded.Origin` the layer it came from, which is what `acpharness config` prints.

## Dependencies

Added:

- `github.com/alecthomas/kong` v1.17.0. The command line, and the mapping from struct fields to flags, help and defaults. Requested by the user.
- `github.com/goccy/go-yaml` v1.19.2. Reads YAML and JSON with one decoder, reports a duplicate key with both positions, and keeps integers exact (below). Requested by the user.

Removed: `knadh/koanf/v2` with its seven provider and parser modules, `go-viper/mapstructure/v2`, `urfave/cli/v3`, and through them `knadh/koanf/maps`, `pelletier/go-toml/v2`, `fsnotify`, `mitchellh/copystructure` and `mitchellh/reflectwalk`. Fifteen modules leave `go.mod` and two enter.

`go.yaml.in/yaml/v3` is no longer imported by our code. Skill front matter (`src/agentctx/skills.go`) was its one user and now uses `goccy/go-yaml`, so the module has one YAML library. It stays in `go.mod` as an indirect requirement of testify. The 44 `SKILL.md` files in `~/.agents/skills` give the same name and description under both libraries. No other front matter was compared.

The binary is 212 KB smaller (`CGO_ENABLED=0 go build -trimpath`: 37,690,491 bytes on master `7c11040`, 37,478,092 with this change on top of it).

## Probes

Run against the real loader from scratch programs, not kept as tests. Every row is what the loader does now.

| Input | Result |
| --- | --- |
| `max_body_bytes: 9007199254740993` in YAML, in JSON, and `context_window` inside a table | kept exact, not rounded through a float |
| `max_body_bytes: 1.0`, `1e3`, `18446744073709551616` | error |
| `context_window: 1.5`, `1e3`, `'1000'` inside a table | error |
| `max_tool_calls: 1` then `max_tool_calls: 2`, in YAML and in JSON | error naming both positions |
| a section written twice | error |
| `x-base: &w 45m` at the top | error: not a config key |
| a list or a single value as the whole file | error |
| `Limits:` for `limits:` | error: names match by exact case |
| a `null` value, an empty section, an empty file, a file of comments | sets nothing |
| `global_dir: ""` in a file | replaces a default that is not empty |
| `ACPHARNESS_CONTEXT__EXTRA_FILES=` | an empty list, over the file's list |
| `ACPHARNESS_TELEMETRY__OTLE=1` | error naming the variable |
| `--config broken.yaml --help` | prints help, exit 0 |
| `--config broken.yaml config` | error with the file and position, exit 1 |

An `ACPHARNESS_*` variable that is not a key was already an error on master (`ACPHARNESS_RECORD_OLLAMA=1 acpharness config` fails there too), so the recording flags of the tests are no new restriction. `ACPHARNESS_CONFIG` is the one exempt name.

## Cost

kong is slower to start than koanf with urfave/cli, and the process holds more memory when idle. The numbers below are from this host (Go 1.27.3). Old is master `7c11040` and new is this change on top of it, before master gained the hosted web search (`3c065d4`). Every whole-process run had a `HOME` whose user config is one five-line `config.yaml`, the same for both builds. They were taken before the `models` merge was restored and were not repeated after it.

Loader alone, 400 loads in one process, defaults only:

| | median | allocations | bytes |
| --- | --- | --- | --- |
| old | 0.96 ms | 13,484 | 780 KB |
| new | 1.65 ms | 15,298 | 1,204 KB |

Whole process, `hyperfine -N --warmup 30 --runs 300`:

| command | old | new |
| --- | --- | --- |
| `acpharness config` | 8.1 ± 0.5 ms | 10.9 ± 0.8 ms |
| `acpharness --version` | 6.1 ± 0.7 ms | 10.3 ± 0.9 ms |

Idle footprint with the driver of `2026-11-11-runtime-footprint.md` (its `measure.py` with the two removed model keys taken out of its config, its `compare.py` unchanged), five interleaved pairs per session count:

| sessions | build | start to `initialize` reply, median (range) | RSS KiB, median (range) |
| --- | --- | --- | --- |
| 1 | old | 7.1 ms (6.7–8.0) | 28,628 (26,844–30,484) |
| 1 | new | 9.0 ms (8.7–9.4) | 29,168 (27,136–31,012) |
| 50 | old | 7.6 ms (6.8–7.9) | 29,344 (28,436–31,344) |
| 50 | new | 9.4 ms (9.1–9.9) | 31,156 (30,752–31,956) |

Start is about 1.8 ms later. The RSS ranges overlap, so this table does not show a memory difference. This host has transparent huge pages set to `always`, and `AnonHugePages` of an idle process read 6,144 or 8,192 kB from run to run, which is likely what spreads the numbers. With huge pages off for the measured process (`prctl(PR_SET_THP_DISABLE)` in a wrapper), `serve` idle with no session, ten interleaved runs:

| build | anonymous kB, median (range) | RSS kB, median |
| --- | --- | --- |
| old | 3,988 (3,800–4,156) | 21,752 |
| new | 5,552 (5,404–5,608) | 23,532 |

So about 1.5 MB more with no session open. This was not measured with sessions open and huge pages off.

Where the time goes, from a CPU profile of 2,000 runs of `acpharness --version` in one process. Of the 5.27 s sampled inside `cli.Run`:

| part | seconds |
| --- | --- |
| `kong.New` | 3.65 |
| of which reading struct tags (`parseTag`) | 1.76 |
| of which interpolating variables in them, a regular expression | 1.39 |
| `kong.Parse` | 1.30 |
| of which our hook that reads files and the environment | 0.27 |
| our validation | 0.09 |
| YAML decoding | 0.06 |

kong builds its model of all the flags on every start, and that is most of the cost. Our loader is a small part.

Where the memory goes is only partly known. A heap probe of `config.Parse` alone shows 241 KB more live heap while the kong context is held than after it is dropped. The new build runs one garbage collection during start-up where the old runs none (`GODEBUG=gctrace=1`).

Two changes were measured and not made. Both were built with `go build -overlay`, so the tree was not edited:

| variant | anonymous kB, median |
| --- | --- |
| new | 5,552 |
| `debug.FreeOSMemory()` before serving | 4,948 |
| the same, and `serve` called without kong's `Context.Run` | 4,384 |
| `serve` called without `Context.Run` only | 5,536 |

The two together take back 1.2 MB of the 1.5 MB. The other 0.4 MB is not explained. `FreeOSMemory` alone started 0.6 to 0.9 ms later in the footprint driver (9.6 and 9.9 ms against 9.0). They are left out because the second replaces kong's command dispatch with our own, and because the footprint driver at 50 sessions showed no gain from the first (31,216 KiB against 31,100, ranges overlapping, huge pages on). No way to reduce the start time was tried.

## Tests

`src/config/load_test.go` is rewritten for the new entry points. The fixtures of `hosts`, `jev`, `provider/chat`, `models`, `loop`, `cli` and `tools/conformance` moved from TOML to YAML and from `--set` to flags. In those files the changed lines are fixtures and the way the command is invoked, not what is asserted.

Read against the test-audit gate:

- The invalid-input table gained rows for a dotted name in a file and for text where a file needs a list. Both fail on the loader as it was before the two checks existed (run with `go test -overlay` against the earlier `load.go`).
- `TestEachFileAddsToTheModels` is new and fails the same way on the loader without the merge.
- The `login` check that `--manual` and `--device` exclude each other matched any error. With the exclusion removed (`go test -overlay`) the command still fails, with "claude has no device code login", so the check proved nothing. It now matches the parser's message.
- In the test files this change touched, no test asserts a default or a help text against a copy.

No test was added for the number forms above. They are properties of the YAML library and of kong's number parsing, checked once by the probes.
