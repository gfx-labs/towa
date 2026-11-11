# Claude and generic core tools

Date: 2026-11-11

## Captures and deployment

`claude.Tools(env, cfg, Deps)` deploys Edit, Write, Glob, Grep, TodoWrite, Read and Bash, in that order. `Deps.Reads` is a required session-owned `*claude.ReadTracker`. The runtime retains that pointer across turns and model switches. `Deps.ShowPlan` has the same callback signature as Codex. Nil omits TodoWrite. Neither constructor allocates a private replacement tracker.

The canonical captures are `prompts/claude-code/2.1.280/tools/{Edit,Write,Glob,Grep,TodoWrite}.v1.json`, committed by capture worker in `90012a2`. These are static extractions from the pinned bundle and its zod conversion, not a recorded model request. The selected v1 descriptions are the captured lean variants. For Edit and Write the read-before-edit text remains present. Choosing these variants is an explicit harness deployment choice, not a claim that a default 2.1.280 session selects them. The manifest retains the conditions for the other variants.

`go generate ./src/tools/claude` verifies each selected canonical file against its manifest SHA256 and copies its bytes into the embedded `specs` directory. Names, descriptions and input schemas are read from these files at construction, not retyped. The assembler adds its optional gating member to its own schema copy and compiles it with `repair.Compile`.

`generic.Tools` uses the same constructors, runners, session tracker and input schemas. Only its top-level descriptions are replaced by the harness-authored neutral strings in `src/tools/generic/generic.go`. These descriptions are not represented as upstream text. Input property descriptions remain part of the exact captured schema.

## Implemented boundaries

- Write creates a new file and remembers it in this session. Overwriting an existing file uses the same read/fingerprint authorization as Edit. Successful Write and Edit refresh the session fingerprint.
- Edit refuses a stale or absent session read. An empty old_string can create a missing or whitespace-only file, as allowed by the captured bundle's validation. Nonempty old_string uses shared exact replacement. The capability's UTF-8-only restriction remains.
- Glob runs real ripgrep through the shared capability and orders returned files by descending modification time. Equal times retain path ordering.
- Grep maps the captured filters, output modes, contexts, line-number choice, case option and multiline choice. The captured head_limit description supplies the default 250. Explicit zero retains the configured byte ceiling without a result-count ceiling. The -o content option is explicitly refused because the capability does not yet return individual submatches.
- TodoWrite uses `src/tools/todo` for full-list validation and delivers ACP entries to the session's plan callback. Multiple in-progress entries are not newly prohibited: upstream's one-item wording is a prompt instruction, not an observed validator.

Read and Bash are copied unchanged from `profiles/print-sdk.default.claude-opus-5-5/tools/`, committed in `2da23a3`, with their distinct manifest checksums verified by the generator. These are exact projections of a recorded 2.1.280 executable request in print mode, default settings, no login, and its default model claude-opus-5-5. No other mode or model was recorded. The request received HTTP 400, so it contains no executed tool results. The original five static captures and their hashes remain unchanged. This seven-tool set is a harness-selected combination, not a claim that upstream sends this entire inventory in that profile.

Read delegates to `claude.ReadFile`, including image normalization and full-file session fingerprinting, documented in `2026-11-11-claude-state.md`. It renders text as line number, tab and content. PDF pages and notebook reads are explicitly refused. Bash delegates to the process-group shell capability with 1500 ms cancel and timeout grace periods. Background commands and explicit sandbox-selection arguments are refused. Commands run without an operating-system sandbox. Timeout stops the group rather than upstream's possible auto-background behavior. Nonpositive or out-of-config-range timeouts are refused instead of silently clamped. Shell configuration supplies omitted timeout and program choices.

Agent, task catalogs, notebooks, interactive questions, remote hosts and other captured tools are not implemented by this core set.

## Result text status

No tool-output recordings for 2.1.280 have been delivered. Every adapter result text below is therefore unverified against an executed upstream binary:

| tool | inspected source and remaining differences |
|---|---|
| Edit | `chunk-1xqpf2j8.js:3451-3453`, validation and mapToolResultToToolResultBlockParam. Success, unread/stale, same-text, creation-refusal and not-found literals are based on inspected source. No locale/escape repair, whitespace normalization, UTF-16, host, notebook routing or contextual suffix parity is claimed. Other capability failures use their own error text. |
| Write | `chunk-1xqpf2j8.js`, Write mapToolResultToToolResultBlockParam, identified by `File created successfully at:`. Basic create/update literals are based on inspected source. Upstream contextual suffixes are not reproduced. Other capability failures use their own error text. |
| Glob | `chunk-1xqpf2j8.js`, mapToolResultToToolResultBlockParam identified by `No files found`. Returned path lines follow that shape. Byte-ceiling and unreadable-result markers are harness-authored and explicitly say that results are incomplete. Upstream count limits and truncation wording are not claimed equivalent. |
| Grep | `chunk-1xqpf2j8.js`, mapToolResultToToolResultBlockParam identified by `No matches found` and `Found`. Basic content/path/count output is implemented. Pagination, summary/pluralization, context separators and truncation text have not been replayed against upstream. Explicit unsupported-option and incomplete-result markers are harness-authored. |
| Read | `chunk-1xqpf2j8.js:697`, text/image result map, and `chunk-esax8p5m.js:15-19`, line-number formatter. Empty-file warning and tab separators follow inspected literals. Continuation marker is harness-authored because the capability does not return a total line count. PDF, notebook, memory/context suffixes, unchanged-file optimization and token-budget equivalence are not implemented. Offset past end is a capability failure rather than an upstream warning. Images use the separately documented normalization differences. |
| Bash | `chunk-1xqpf2j8.js:3525-3527`, result map, `:679-683`, prefix cut, and `chunk-s5969tjt.js:11`, default 30000-character limit. Output keeps a prefix measured in UTF-16 units without splitting a Unicode scalar. Configured byte truncation can occur first and has its own explicit marker. Combined stdout/stderr order, timeout, pipe-drain and sandbox/background refusal text are harness behavior. No image-output decoding, auto-background, separate stderr layout or upstream output parity is claimed. |
| TodoWrite | `chunk-00pn10ex.js`, mapToolResultToToolResultBlockParam, identified by `Todos have been modified successfully`. The success literal comes from that function. Validation failures come from the shared capability and are not claimed text-equivalent. |

## Validation

Tests run the constructed tools through their compiled captured schemas against real files, images, ripgrep and shell processes. The file workflow creates through generic, edits through Claude with the same tracker, rejects an independent session's overwrite and an external change, then succeeds after a model-facing Read of one line. It checks native image-part routing against a real upstream PNG fixture and explicit pages refusal. Search checks actual glob and grep effects, and plan delivery checks actual adapter-to-callback entries. Bash checks actual working-directory file effects, fractional millisecond input, exit failure, timeout termination and refusal before execution for unsupported modes and invalid timeout values. The existing session workflow and one focused Bash integration test are retained. No scaffolding tests or synthetic upstream result fixtures were added. These are capability integration tests, not stand-ins for upstream output recordings. Output replay remains pending those recordings.
