# acpharness

A Go coding agent that speaks ACP. Go 1.27.3, module `github.com/gfx-labs/acpharness`. The design and the reasons behind it are in `docs/design.md`.

- Check: `go build ./... && go vet ./... && go test -race ./... && golangci-lint run ./...` (golangci-lint v2.15.0, config `.golangci.yml`). `mise install` provides both tools.
- Linux only for now. `CGO_ENABLED=0 go build ./...` must pass.
- Packages live in `src/`. No `internal/`. A package moves to `pkg/` when an outside caller needs it.
- `src/acp/*_gen.go` are generated from `src/acp/schema` by `tools/acpgen`. Change the generator or the schema files, then run `go generate ./src/acp`. Never edit a generated file. `tools/acpgen` is its own module: check it with `go vet ./... && go test ./...` in that directory.
- Keep the dependency list short. Record why each dependency was added in `docs/notes/`.
- Limits come from configuration, a protocol or a measurement, never an unexplained constant. Report an invalid state as an error. Do not truncate, clamp or fall back silently.
- Tests use testify. Provider tests replay recorded real responses, and tool tests compare against outputs recorded from the pinned upstream CLI. No mocked providers. Live checks run by hand behind an env flag.
- Design notes go in a new file `docs/notes/<date>-<slug>.md` (short, dated). `docs/design.md` holds the current design. Update it when a decision changes.
- Commits go straight to master until the first milestone.
- Plain names, short comments explaining why, no marketing language.
