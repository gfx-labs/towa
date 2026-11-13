# towa

towa ([insp.](https://boogiepop.fandom.com/wiki/Towa_Organization)) is an acp-based coding harness, primarily inspired by https://github.com/1jehuang/jcode, https://github.com/vercel-labs/fx, and https://github.com/earendil-works/pi

it is designed to be run in autonomous harness swarms and workflows in a single-turn, or orchestrated multi-turn settings.

there is no ui. the only way to speak with it is the acp protocol, over stdio or socket.

try it with out with https://github.com/gfx-labs/acphost, a lightweight wrapper over acp that proxies and normalizes acp, along with providing journals, persistent sessions, and sandboxing.

there are a large amount of configuration keys, everything configurable through ENV, cli, or file.

these are meant to be set per run by a caller, another llm/program/scheduler/whatever.

for instance, ask your llm to setup a provider with you, and then run a turn using acphost or another light weight acp cli client.

## install

with nix, from this repository. it is private, so nix needs access to it: an ssh key that can read it, or a github token in `access-tokens` of `nix.conf`.

```sh
nix profile install git+ssh://git@github.com/gfx-labs/towa   # over ssh
nix profile install github:gfx-labs/towa                     # with a token
nix run git+ssh://git@github.com/gfx-labs/towa -- --help     # without installing
```

or in a flake: `inputs.towa.url = "git+ssh://git@github.com/gfx-labs/towa";`, then `towa.packages.${system}.default`.

the package is one static binary for x86_64-linux, aarch64-linux and aarch64-darwin. it brings no tools with it: the model runs `bash`, `rg` and `find` from the `PATH` towa is started with.

with go 1.27.3: `go install github.com/gfx-labs/towa/cmd/towa@latest`, with `GOPRIVATE=github.com/gfx-labs`.

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
