# A new model works by its name

2026-11-12

## Why

A model the built-in table did not have was refused when it was selected, until `model_context_window` and `model_max_output_tokens` were set. Those two keys hold one pair of figures for every unlisted model. So a model released after the table was written (`claude-opus-6`) needed a config edit or a new row and a build before its name could be used.

What the other two agents do with a name they have not seen, read on the same day:

- jcode (`037163c7b`, `crates/jcode-provider-core/src/anthropic.rs`) parses the family and version from the name. A Claude model at version 5 or later is taken to have a 1M window. The output limit is a prefix list, and a name that matches no prefix gets 32K.
- pi 1.3.0 keeps a catalog bundled with the release and refreshed from pi.dev. A name not in it is sent with the warning `Model "claude-opus-6" not found for provider "anthropic". Using custom model id.`

## Decision

A model that has no row takes the figures its name points to (`src/models/guess.go`). The table stays the only place a figure is written down.

- A name is a vendor, a family and a version: `claude-opus-5-5` is claude, opus, 5.5, and `gpt-6.1-sol` is gpt, sol, 6.1. A release date at the end is dropped.
- The same model under another spelling or with a date takes its own row.
- A newer version of a family in the table takes the family's newest row.
- A family the table does not have takes the vendor's newest rows, when its version is not older than theirs and those rows agree.
- An older model, a model of a vendor with no row, a name without a version and a gateway's id (`anthropic/claude-opus-4.7`) are not guessed. They are refused as before unless the two keys are set.
- A key that is set outranks the guess, one figure at a time.
- `serve` logs each configured model whose figures are guessed, with the row they came from.

The guess reads the table and not a rule such as "version 5 or later is 1M", so no figure is written in the code and a new row moves the guess with it.

A guess can be wrong: a new model may have a larger window or a smaller output limit than its family's last one. A window guessed too large delays compaction until the provider refuses the request. An output limit guessed too large is refused by the provider on the first request. Both are corrected by a key or a row.

## Check

Build of this change, `towa serve --models claude:a-ant/<model>` with neither key set, one prompt through `acphost exec`:

| Model | Before | After |
|---|---|---|
| `claude-opus-6` (does not exist) | refused at startup: nothing is known about it | request sent, the provider answers `HTTP 404: model: claude-opus-6` |
| `claude-opus-4-5` (older than the table) | refused at startup | refused at startup |
| `claude-opus-5-5` (has a row) | completes | completes |

The log line at startup:

```
msg="model is not in the built-in table, its figures are guessed from its name" model=claude-opus-6 from=claude-opus-5-5 model_context_window=1000000 model_max_output_tokens=128000
```

`TestSelectGuessesANewModelFromItsName` covers the rules above.
