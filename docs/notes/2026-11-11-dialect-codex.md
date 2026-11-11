# Codex dialect and apply_patch

Date: 2026-11-11

Upstream: openai/codex `ce0323f2f2d3b25e82806cd555237da3db4e1897`. Tool definitions were captured by executing upstream's tool planners, in `prompts/codex/ce0323f2/` (capture commit `976e851`). The three embedded definitions are copies of that capture. The patch code and upstream fixtures carry upstream's Apache-2.0 license and notice.

## Interface

`codex.Tools(env tools.Env, cfg config.Tools, deps codex.Deps) ([]tools.Tool, error)` returns tools for the `codex` dialect. `env` supplies the directories for Describe. Run receives the call environment, including progress. `Deps.ShowPlan` sends checked plan entries through the session's update path. When it is nil, update_plan is omitted. No configuration keys or dependencies were added.

Supported tools:

- `exec_command`, the upstream run-to-completion variant. The capture is the interactive spec from `core/src/tools/handlers/shell_spec.rs:22-107`. The constructor applies the exact changes in `core/src/tools/handlers/unified_exec/exec_command.rs:463-491`: replace the description, remove `tty` and `yield_time_ms`, and add `timeout_ms`. Upstream selects this variant when unified exec is disabled (`core/src/tools/spec_plan.rs:1256-1263`). It runs commands through the shared shell capability.
- `apply_patch`, a native freeform tool with the captured Lark grammar. Source: `core/src/tools/handlers/apply_patch_spec.rs:9-27`, `core/assets/tools/apply_patch.lark`. Input is a JSON string holding patch text, as the Responses client stores custom tool calls. It has no JSON schema. Toolset must skip schema compilation and schema member injection for it.
- `update_plan`, from `core/src/tools/handlers/plan_spec.rs:7-57`. It uses `todo.Check` and ShowPlan. The success text is `Plan updated`, from `plan.rs:22`. Upstream's one-in-progress restriction is descriptive, not enforced by its handler, and this adapter does not add it.

## Patch behavior

`src/tools/patch` ports `apply-patch/src/{parser,streaming_parser,seek_sequence,file_update,text_file,lib}.rs`. It parses a whole patch, including the lenient EOF heredoc wrapper, then applies file hunks in order through fsops. Earlier changes remain when a later hunk fails. A move writes the destination then removes the source. A failed source removal retains the destination change in Result. There is no undo logic.

Matching tries exact lines, trailing-whitespace removal, surrounding-whitespace removal, then upstream's Unicode punctuation normalization. Context lines retain their original text and line endings. Insertions use the first existing line ending, or LF for an empty file. Updated final lines get a terminator, as upstream does.

The summary uses upstream's wording and A/M/D grouping (`apply-patch/src/lib.rs:825-840`). Moves list only the destination as modified, while both filesystem changes are retained for client diffs. Parse error wording is ported from the source. Filesystem failures retain the wrapped Go fsops errors. They do not pretend to have Rust's OS error wording.

The adapter deliberately uses the ordered library application rather than upstream's preliminary whole-patch filesystem verification (`apply-patch/src/invocation.rs:180-251`). That preflight would prevent the requested partial application in scenario 015. On failure the result explicitly lists the committed prefix under `Changes applied before the failure:`. That heading is acpharness wording, not an upstream quote. Upstream retains the delta but prints no success summary on failure.

## Output and limits

The shell timeout defaults to upstream's 10 seconds (`core/src/exec.rs:63`), with cancellation grace of 50 ms (`exec.rs:71`). The configured maximum timeout and output collection cap still apply. A timeout maps to exit code 124 (`unified_exec/process.rs:214-216`), and a signal exit maps to 128 plus its number (`utils/pty/src/process.rs:43-44`). Cancellation uses `Wall time: ... seconds\naborted by user`, from `core/src/tools/parallel.rs:378-382`.

The model's shell override and login flag use the shared capability's per-call Program and Login fields (added in `cfbf74b`). Login defaults to true, as upstream's `core/src/tools/handlers/unified_exec.rs:97-123` does when login shells are enabled. The configured program remains the default rather than rediscovering the user's shell. The call has Executes effect and follows the normal approval path.

Exec text follows `core/src/tools/context.rs:495-574`: wall time to four decimals, exit code, original approximate token count and Output. Chunk ID is omitted, which that formatter permits. Output uses the upstream four-byte token estimate, 10000-token default, UTF-8-safe head/tail truncation and truncation warnings (`utils/string/src/truncate.rs`, `utils/output-truncation/src/lib.rs`). All models in upstream's pinned `models-manager/models.json` use that 10000-token policy. Requests for fewer tokens are honored. Larger requests are capped by the pinned model policy, as upstream does. The shell capability's byte collector is distinct from model-text truncation. Omitted collection bytes remain explicitly reported.

Patch output uses the apply-patch emitter's format, not unified exec's: `Exit code`, wall time rounded to one decimal and Output (`core/src/tools/mod.rs:91-115`). ACP receives the committed file diffs. Deletions alone use the delete tool kind.

## Unsupported behavior

- Interactive exec and `write_stdin`: no resumable process or PTY capability exists. Only the upstream one-shot spec is exposed.
- `view_image`: upstream delegates resizing and high/original detail handling to centralized history preparation (`core/src/tools/handlers/view_image.rs:185-195`). That path is absent here. Chat also rejects binary tool results. The tool is omitted rather than sending different image semantics.
- `request_user_input`, `request_permissions`, code-mode `exec` and `wait`, clocks, multi-agent tools and attached environment selection are not implemented by this dialect. No invented replacement names or schemas are advertised.
- Sandbox escalation is refused with an explicit acpharness message. The host supplies isolation. No additional permission grant is implied by a model's argument.
- The shared todo checker refuses blank step text and trims whitespace. Upstream accepts it. This adapter reports the shared validation failure rather than silently inventing text. JSON errors use Go's serializer errors, not fabricated serde errors.
- Filesystem limits and confinement come from cfg.Files. fsops uses atomic file replacement and follows symbolic links, including a delete naming the symlink's target. Those semantics are inherited from the shared capability.

## Validation

Kept tests:

- All 25 scenario directories from upstream `apply-patch/tests/fixtures/scenarios`, copied without edits. The runner is ported from `tests/suite/scenarios.rs` and compares the full final filesystem state, including directories. It intentionally does not assert exit status, as upstream's runner does. Scenario 015 retains `created.txt` after a later failure. CRLF and mixed-line-ending fixture bytes remain unchanged.
- Six additional line-ending and repeated-context cases from upstream `tests/suite/tool.rs:108-183`, run on files in t.TempDir().
- The three heredoc forms and malformed quote case from `parser.rs:test_parse_patch_lenient`.
- Six token-truncation cases from `utils/string/src/truncate/tests.rs:89-124`.
- Composed integration inputs: real `printf` commands for output and nonzero exit, real `sleep` for timeout, `/bin/bash` with `shopt -q login_shell` for the default login flag and shell override, a partial patch based on upstream scenario 015, and a two-step plan passed through the real adapter callback. These are not upstream recordings.

No mocked provider, fabricated response recording or scaffolding test was added. No upstream CLI output was recorded, so this is source-and-fixture conformance, not the project's stronger recorded-CLI conformance requirement. No live model turn was run.

Lane checks passed: build, vet, race tests, golangci-lint, darwin arm64 no-CGO build and Linux no-CGO build for both owned packages. The upstream whitespace-padding fixtures intentionally contain trailing spaces, so unrestricted git diff --check reports those two input files. Go source passes whitespace checks.

A whole-repository build and vet passed. The first whole-repository race test run failed in the concurrent agentctx, subagent and extras lanes. The agentctx and subagent failures passed on a focused rerun after those lanes changed. A later whole-tree retry stopped at an unrelated compile error in `src/compact/work.go`, calling a missing UUID.Nil method. Final scoped checks above passed again. These results do not establish recorded upstream CLI or live-model conformance.
