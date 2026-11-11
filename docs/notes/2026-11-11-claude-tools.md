# Claude and generic core tools

Date: 2026-11-11

## Captures and deployment

`claude.Tools(env, cfg, Deps)` deploys Edit, Write, Glob, Grep and TodoWrite. `Deps.Reads` is a required session-owned `*claude.ReadTracker`. The runtime retains that pointer across turns and model switches. `Deps.ShowPlan` has the same callback signature as Codex. Nil omits TodoWrite. Neither constructor allocates a private replacement tracker.

The canonical captures are `prompts/claude-code/2.1.280/tools/{Edit,Write,Glob,Grep,TodoWrite}.v1.json`, committed by capture worker in `90012a2`. These are static extractions from the pinned bundle and its zod conversion, not a recorded model request. The selected v1 descriptions are the captured lean variants. For Edit and Write the read-before-edit text remains present. Choosing these variants is an explicit harness deployment choice, not a claim that a default 2.1.280 session selects them. The manifest retains the conditions for the other variants.

`go generate ./src/tools/claude` verifies each selected canonical file against its manifest SHA256 and copies its bytes into the embedded `specs` directory. Names, descriptions and input schemas are read from these files at construction, not retyped. The assembler adds its optional gating member to its own schema copy and compiles it with `repair.Compile`.

`generic.Tools` uses the same constructors, runners, session tracker and input schemas. Only its top-level descriptions are replaced by the harness-authored neutral strings in `src/tools/generic/generic.go`. These descriptions are not represented as upstream text. Input property descriptions remain part of the exact captured schema.

## Implemented boundaries

- Write creates a new file and remembers it in this session. Overwriting an existing file uses the same read/fingerprint authorization as Edit. Successful Write and Edit refresh the session fingerprint.
- Edit refuses a stale or absent session read. An empty old_string can create a missing or whitespace-only file, as allowed by the captured bundle's validation. Nonempty old_string uses shared exact replacement. The capability's UTF-8-only restriction remains.
- Glob runs real ripgrep through the shared capability and orders returned files by descending modification time. Equal times retain path ordering.
- Grep maps the captured filters, output modes, contexts, line-number choice, case option and multiline choice. The captured head_limit description supplies the default 250. Explicit zero retains the configured byte ceiling without a result-count ceiling. The -o content option is explicitly refused because the capability does not yet return individual submatches.
- TodoWrite uses `src/tools/todo` for full-list validation and delivers ACP entries to the session's plan callback. Multiple in-progress entries are not newly prohibited: upstream's one-item wording is a prompt instruction, not an observed validator.

The image/read integration is `claude.ReadFile`, documented in `2026-11-11-claude-state.md`. No model-facing Read or Bash tool is exposed yet. Their captures are absent, and the constructors do not invent those schemas. Agent, task catalogs, notebooks, interactive questions, remote hosts and other captured tools are also not implemented by this core set.

## Result text status

No tool-output recordings for 2.1.280 have been delivered. Every adapter result text below is therefore unverified against an executed upstream binary:

| tool | inspected source and remaining differences |
|---|---|
| Edit | `chunk-1xqpf2j8.js:3451-3453`, validation and mapToolResultToToolResultBlockParam. Success, unread/stale, same-text, creation-refusal and not-found literals are based on inspected source. No locale/escape repair, whitespace normalization, UTF-16, host, notebook routing or contextual suffix parity is claimed. Other capability failures use their own error text. |
| Write | `chunk-1xqpf2j8.js`, Write mapToolResultToToolResultBlockParam, identified by `File created successfully at:`. Basic create/update literals are based on inspected source. Upstream contextual suffixes are not reproduced. Other capability failures use their own error text. |
| Glob | `chunk-1xqpf2j8.js`, mapToolResultToToolResultBlockParam identified by `No files found`. Returned path lines follow that shape. Byte-ceiling and unreadable-result markers are harness-authored and explicitly say that results are incomplete. Upstream count limits and truncation wording are not claimed equivalent. |
| Grep | `chunk-1xqpf2j8.js`, mapToolResultToToolResultBlockParam identified by `No matches found` and `Found`. Basic content/path/count output is implemented. Pagination, summary/pluralization, context separators and truncation text have not been replayed against upstream. Explicit unsupported-option and incomplete-result markers are harness-authored. |
| TodoWrite | `chunk-00pn10ex.js`, mapToolResultToToolResultBlockParam, identified by `Todos have been modified successfully`. The success literal comes from that function. Validation failures come from the shared capability and are not claimed text-equivalent. |

## Validation

Tests run the constructed tools through their compiled captured schemas against real files and ripgrep. The file workflow creates through generic, edits through Claude with the same tracker, rejects an independent session's overwrite and an external change, then succeeds after a real re-read. Search checks actual glob and grep effects, and plan delivery checks actual adapter-to-callback entries. These are capability integration tests, not synthetic stand-ins for upstream output recordings. Output replay remains pending those recordings.
