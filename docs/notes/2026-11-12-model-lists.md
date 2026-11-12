# A provider's model list gives the figures of a model the table lacks

2026-11-12

## Why

After `docs/notes/2026-11-12-model-guess.md` a new version of a known family works by its name. A model the name says nothing about (an older one such as `claude-opus-4-5`, or one whose figures differ from its family's) still needed the two config keys. jcode reads each provider's own model list for this and falls back to the name only when the list says nothing.

## What the lists hold

Read on 2026-11-12 with the stored logins.

- Anthropic, `GET https://api.anthropic.com/v1/models`, 15 models. Each has `max_input_tokens`, `max_tokens` and `capabilities.thinking.types.adaptive.supported`. Older models are listed with a release date (`claude-opus-4-5-20251101`).
- Codex backend, `GET https://chatgpt.com/backend-api/codex/models?client_version=1.0.0`, 11 models. Each has `context_window` and no output limit. Without `client_version` the answer is HTTP 400, and with `0.1.0` the list is empty.
- The OpenAI API key endpoint's list gives neither figure, so an API key account has no list (`provider.ErrNoModelList`).

The lists disagree with the table in two places, and the table wins both:

- Anthropic lists 1M for `claude-opus-4-6` and `claude-sonnet-4-6`. They have 1M only with the `context-1m` beta, which this client does not send. The table says 200K.
- The Codex list gives `gpt-6.1-sol` a `context_window` of 500,000 and a `max_context_window` of 872,000. The table says 1,050,000 from OpenAI's model page. Which is right for a ChatGPT login is not tested.

## Decision

- The figures of a model come from the first of: the built-in table, a config key that is set, the account's model list, the model's name.
- At start, for each configured `claude` or `codex` model the table does not have, towa reads the account's list. One that cannot be read is logged as a warning and the name is used.
- The lists are kept in `models.json` in the state directory (beside `auth/`), by `provider:account`, with the time each was read. A list younger than `model_list_max_age_seconds` (default 1800, jcode's value) is not read again. `model_list_timeout_seconds` (default 30, jcode's value) bounds one read.
- A model in the table causes no request, so a config of only known models starts as before.
- The list's adaptive thinking flag is kept too. `claude-opus-4-5` answers the default thinking parameter with HTTP 400, and the table only knew that for `claude-haiku-4-5`.
- A list names a model with its date, so a name without one matches when exactly one listed model has that family and version.

## Check

Build of this change, account `a-ant`, neither key set:

| Model | Before | After |
|---|---|---|
| `claude-opus-4-5` | refused at start | 200,000 and 64,000 from the list, no thinking parameter, the turn completes |
| `claude-opus-6` (does not exist) | guessed from `claude-opus-5-5` | the same: the list does not have it |
| `codex:gfx/gpt-5.6-sol` | refused at start | window 272,000 from the list, still refused for `model_max_output_tokens` alone |

The first start wrote `models.json` (mode 0600, 2.2 KB, two accounts). A second start within the age left its two read times unchanged.

`TestModelsReadsTheAccountsList` replays the recorded Anthropic list. `TestSelectTakesAFigureFromTheProvidersList` covers the order and the file. The Codex list has no recording: its response is 697 KB, of which all but 28 KB is two prompt fields per model.
