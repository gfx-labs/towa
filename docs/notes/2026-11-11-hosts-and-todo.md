# The hosts command and the todo capability

2026-11-11

## hosts

`src/hosts.List` takes the resolved `config.Config` and returns the hosts the process contacts, each with the config key that names it, and a list of what it does not cover. `src/cli/hosts.go` defines `hostsCommand(h Host)`. It prints one `host:port<TAB>key` line per host on standard output, sorted, and nothing else there. Each thing the list does not cover is one `hosts: not covered: ...` line on standard error.

What is listed:

| source | key | host |
|---|---|---|
| each `[[providers]]` entry | `providers[i].base_url` | the base URL's host |
| each account of kind `oauth` | `accounts[i].provider` | the authorize and token URLs of `auth.Flows()[provider]` |
| each remote MCP server | `mcp.servers[i].url` | the URL's host |
| OTLP, when `telemetry.otel` is on | `telemetry.otel:<VARIABLE>` | the endpoint in the variable |

Decisions:

- No URL is written in `src/hosts`. The flows are passed in as `Inputs.Flows` and the OTLP variables as values in `Inputs.OTLP`. The package does not read the environment.
- The port is the URL's, or 443 for `https` and `wss` and 80 for `http` and `ws`. A URL with another scheme and no port is an error. An IPv6 host is printed in brackets.
- A proxied account contacts its provider host like any other account, so it is treated the same for the provider. Its OAuth endpoints are not listed: the harness logs in and refreshes only an `oauth` account.
- An `oauth` account whose provider has no flow is an error, as it is in `auth.NewSource`.
- The authorize URL is listed although the user's browser opens it, not the harness, because the browser may run on the same host.
- OTLP follows the exporters: a signal's own variable (`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`, `..._METRICS_ENDPOINT`, `..._LOGS_ENDPOINT`) wins over `OTEL_EXPORTER_OTLP_ENDPOINT`. The logs signal is counted only when `telemetry.logs.otel.enabled` is on, as in `telemetry.Start`. When a signal has neither variable the exporter uses its own default endpoint. That default is the SDK's and is not written here, so it is reported as not covered.
- Two entries that give the same host and key are one line. The same host under two keys is two lines, so that the reader sees every key that names it.

Not covered, and said so on standard error on every run:

- The base URL of a built-in provider. An account whose `provider` is not a `[[providers]]` id (`claude`, `codex`) contacts a host that the provider's package owns. `src/provider/openai` has `DefaultBaseURL` and `DefaultCodexBaseURL` in the working tree but is not on master, and the Anthropic client is not either. One line per such account names it.
- A stdio MCP server. It is a local command, and what that command contacts cannot be known. One line per server names it and its command.
- The authorization server of a remote MCP server that uses OAuth. The server names it in its metadata when it is contacted, so it is not in the configuration.
- Embeddings and Jev, whose packages and config sections are not on master.
- webfetch targets, MCP servers a client names on `session/new`, and whatever a hook command or a shell command contacts.

For integration: add the built-in providers' base URLs from their packages (keyed by the account, since no config key holds them), and embeddings and Jev when their config sections exist. Each addition removes its line from `NotCovered`.

## todo

`src/tools/todo` has `Check(List) (Result, error)`. A `List` is the whole list of one call: items with content, status, an optional priority and an optional active form, and an optional explanation. `Check` trims white space and returns the items with the `acp.PlanEntry` list a client is shown. It holds no state.

The upstream shapes:

- Codex `update_plan`, read from openai/codex at `ce0323f2` (`codex-rs/protocol/src/plan_tool.rs`, `core/src/tools/handlers/plan.rs` and `plan_spec.rs`): `plan` is a list of `{step, status}` with status `pending`, `in_progress` or `completed`, and `explanation` is optional. Unknown members are refused by the deserializer.
- Claude Code `TodoWrite`: not confirmed. The `capture` lane has not committed its captures (`prompts/` is empty on master), so nothing was read. The fields assumed are `todos`, a list of `{content, status, activeForm}` with the same three statuses. Whether 2.1.280 still sends `priority` or `id`, and whether `activeForm` is required, must be checked against the capture when it lands.

Decisions:

- Typed errors (`*todo.Error` with `Kind`, `Index`, `Value`): `empty_content`, `unknown_status`, `unknown_priority`. A missing status is an unknown status with an empty value.
- More than one item in progress is not an error. Codex at `ce0323f2` does not enforce it: the handler parses the arguments and sends the event, and "At most one step can be in_progress at a time" is in the tool's description only. For Claude Code it could not be checked. `Result.InProgress` holds the count, so an adapter whose upstream does enforce it can refuse there.
- Codex also accepts a step with empty text. `Check` refuses it, because ACP shows the entry to a person and an empty line says nothing. The Codex adapter has to word that error itself, since upstream has none.
- An empty list is valid and clears the plan.
- ACP requires a priority on every entry and neither upstream shape is known to send one. An item without a priority is shown as `medium`. This is a stated mapping, and `Result.Items` keeps the priority empty.
- An item in progress with an active form is shown with that wording, which is what the field is for upstream. Other items are shown with their content.
- `todo.Update(entries, id, sendable)` picks the session update. With a plan id, and when `sendable` (which is `loop.Turn.Sendable`) says the client takes `plan_update`, it returns a `plan_update`. Otherwise it returns `plan`, which every client takes. With an empty id it always returns `plan`.

## Checks

```
go build ./src/hosts/... ./src/tools/todo/... ./src/cli/...
go vet ./src/hosts/... ./src/tools/todo/...
go test -race ./src/hosts/ ./src/tools/todo/ ./src/cli/
golangci-lint run ./src/hosts/... ./src/tools/todo/... ./src/cli/...
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build ./src/hosts/... ./src/tools/todo/... ./src/cli/...
```

All pass except one lint finding: `hostsCommand` is unused until `cli.go` registers it. The hosts tests load real TOML files through `config.Load`, and the command test runs the argument list `--config <file> --set telemetry.otel=true hosts`. The todo test that picks the update type uses the session's real `Gate`.

## Not done

- `hostsCommand(h)` is not registered in `cli.go`.
- No dialect adapter for todo, and no `tools.Tool`.
- The `TodoWrite` shape is unconfirmed, as above.
