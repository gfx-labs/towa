# towa

towa ([insp.](https://boogiepop.fandom.com/wiki/Towa_Organization)) is an acp-based coding harness, primarily inspired by https://github.com/1jehuang/jcode, https://github.com/vercel-labs/fx, and https://github.com/earendil-works/pi

it is designed to be run in autonomous harness swarms and workflows in a single-turn, or orchestrated multi-turn settings.

there is no ui. the only way to speak with it is the acp protocol, over stdio or socket.

try it with out with https://github.com/gfx-labs/acpc or acpx

there are a large amount of configuration keys, everything configurable through ENV, cli, or file.

these are meant to be set per run by a caller, another llm/program/scheduler/whatever.

for instance, ask your llm to setup a provider with you, and then run a turn using acpc or acpx or another light weight acp cli client.

## install

with nix:

```sh
nix profile install github:gfx-labs/towa
nix run github:gfx-labs/towa -- --help   # without installing
```

or in a flake: `inputs.towa.url = "github:gfx-labs/towa";`, then `towa.packages.${system}.default`.

the package is one static binary for x86_64-linux, aarch64-linux and aarch64-darwin. it brings no tools with it: the model runs `bash`, `rg` and `find` from the `PATH` towa is started with.

with go 1.27.3: `go install github.com/gfx-labs/towa/cmd/towa@latest`.

or download a static binary from the [latest release](https://github.com/gfx-labs/towa/releases/latest), built for linux amd64, linux arm64 and darwin arm64, with `checksums.txt`:

```sh
curl -fsSLO https://github.com/gfx-labs/towa/releases/latest/download/checksums.txt
file=$(grep linux_amd64 checksums.txt | awk '{print $2}')
curl -fsSLO "https://github.com/gfx-labs/towa/releases/latest/download/$file"
sha256sum -c --ignore-missing checksums.txt && tar -xzf "$file"
```

releases are tagged `vX.Y.Z` by release-please. pin one with `/vX.Y.Z` on the flake url (`github:gfx-labs/towa/vX.Y.Z`) or `@vX.Y.Z` with go install.

## key features

- like jcode, built to run many sessions per host. one process can serve many connections over stdio, a unix socket or tcp with mtls. an idle session wakes no goroutine.
- claude and codex subscription support, along with chat completions and responses api.
- models are one ordered list of `provider:account/model#effort` entries, comma separated as a flag. on a rate limit, exhausted quota, auth failure or overload the next entry in the list takes the request, and the client is told.
- a proxy can hold the credentials. the harness sends a placeholder and never sees the token, every http client honours `HTTPS_PROXY`, and `towa hosts` prints each host the config will contact, for an egress allowlist.
- an session log is append-only, with reactive compression which compaction adds a summary and deletes nothing, so `conversation_search` can still read what was compacted away, and a parent can read its subagents' sessions.
- a tool result over the token budget comes back as a summary with its size. the model asks again to get all of it.
- mcp servers sit behind one `mcp` tool with list, search and call, so their schemas stay out of the prompt. a server the client adds for the session gets its tools by name.
- every config key is a flag in `--help`, and can be set in a yaml file or the environment. an invalid value is an error, never a silent fallback.
- a large amount of levers for selecting custom injected context for the run from different paths.
- hooks, per-turn limits and opentelemetry traces, metrics and logs, are all config


## funny numbers

as a result of doing very little, towa is rather lightweight.
- one process with one empty session measured about <50 MB, with new empty sessions costing <64kb. know that this is purely synthetic.
- idle sessions not in turn use almost no measurable cycles
