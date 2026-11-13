# towa

A Go coding agent that speaks ACP. Go 1.27.3, module `github.com/gfx-labs/towa`. The design and the reasons behind it are in `docs/design.md`.

- Check: `go build ./... && go vet ./... && go test -race ./... && golangci-lint run ./...` (golangci-lint v2.15.0, config `.golangci.yml`). `mise install` provides both tools.
- Linux is the platform. macOS is a headless build target: `GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build ./...` must pass, and code that only one of them can run goes in a `_linux.go` or `_darwin.go` file. `CGO_ENABLED=0 go build ./...` must pass.
- Packages live in `src/`, in eleven groups: `config`, `transcript`, `support`, `auth`, `provider`, `sidecar`, `store`, `prompt`, `tools`, `agent`, `cli`. A group imports only the groups before it in that list. Put a new package in the group it belongs to, not at the top of `src/`. No `internal/`. A package moves to `pkg/` when an outside caller needs it. `pkg/acp` is there because ACP clients import it.
- `flake.nix` and `nix/package.nix` build the binary with Nix. `vendorHash` in `nix/package.nix` is the hash of the modules in `go.sum`: after a change to `go.mod` or `go.sum`, set it to `lib.fakeHash`, run `nix build .#towa`, and copy the hash the error prints. CI does not build the flake, so run `nix build .#towa && nix flake check` by hand after such a change or a change to either file.
- `cmd/towa/main.go` only calls `src/cli`. The binary is `towa`. Commands, flags and wiring go in `src/cli`.
- `pkg/acp/*_gen.go` are generated from `pkg/acp/schema` by `tools/acpgen`. Change the generator or the schema files, then run `go generate ./pkg/acp`. Never edit a generated file. `tools/acpgen` is its own module: check it with `go vet ./... && go test ./...` in that directory.
- Keep the dependency list short. Record why each dependency was added in `docs/notes/`.
- Limits come from configuration, a protocol or a measurement, never an unexplained constant. Report an invalid state as an error. Do not truncate, clamp or fall back silently.
- Tests use testify. Provider tests replay recorded real responses, and tool tests run the tool against real files, a real shell and the real `rg`. No mocked providers, and no hand-written stand-in for a recording. Live checks run by hand behind an env flag. A recording that needs a local model is made from `openai/gpt-oss-20b` on an OpenAI-compatible server that takes no key (LM Studio), through an account of kind `none`. Recordings made earlier from `llama3.2:3b` stay until their request changes.
- A test checks behaviour a caller can see. Do not write one that fails when prompt, tool description or message wording changes, that compares a constant or default with a copy of it, that restates what Go or a library does, or that only proves an unexported helper to its author. Do not keep a recording that holds towa's own system prompt or tool list: every wording change would need it recorded again. Do not add an exported function or counter that only a test calls.
- Design notes go in a new file `docs/notes/<date>-<slug>.md` (short, dated). `docs/design.md` holds the current design. Update it when a decision changes.
- Commits go straight to master until the first milestone.
- Nothing is released and nobody depends on it. Change a format outright: no migration, no compatibility path, and no note or test about the old format or data stored under it.
- Plain names, short comments explaining why, no marketing language.

## JSON

Use `encoding/json/v2` and `encoding/json/jsontext`. Lint rejects `encoding/json` outside `pkg/acp`, which has not moved yet. Measured on Go 1.27.3 (`docs/notes/2026-11-11-contracts-and-conventions.md`):

- A nil slice encodes as `[]` and a nil map as `{}`. Use `omitzero` to leave a field out. `omitempty` leaves out only a value that encodes as empty JSON, so a zero number stays.
- Member names match by exact case. Unknown members are accepted unless `json.RejectUnknownMembers(true)` is passed. A duplicate member name and invalid UTF-8 are errors.
- `time.Duration` has no default encoding and fails to marshal. Store a duration as a number with its unit in the field name.
- The text of an error changes between runs on purpose. Match errors with `errors.As`, never by text.
- Raw JSON is `jsontext.Value`. Write to a stream with `json.MarshalWrite` and read successive values with `json.UnmarshalDecode`, so that a message is not held twice.

## Buffers

Use `github.com/valyala/bytebufferpool` for a buffer that lives for one message: an encoded wire message, an SSE event, a tool's output while it is assembled. `Get`, then `defer Put`, and keep no reference to the bytes after `Put`. Do not pool a buffer that lives as long as a session. A change that pools a hot path comes with a before and after benchmark (`-benchmem`) in its note.

## HTTP

Every HTTP client comes from `src/support/httpclient`, which takes the proxy from the environment and trusts the system certificate pool. towa runs in places where a proxy is the only way out and holds the credentials. Lint rejects `http.Transport`, `http.DefaultTransport`, `http.DefaultClient` and `http.Get` and its siblings elsewhere. Lint sees only this repo's code: a dependency that makes its own client is covered by the test that runs a turn through a proxy.
