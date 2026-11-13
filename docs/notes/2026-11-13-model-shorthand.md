# Model names without an account

2026-11-13

## Decision

- In `models`, an account of `*` stands for every account of the provider: `codex:*/gpt-6.1-sol#high`. `codex:gpt-6.1-sol#high` is short for it. A model name that holds `/` (a gateway's) needs the account written out, since the first `/` ends the account.
- The entry expands when a model is selected, to one chain entry per account of the provider in the order of the accounts (configured, then the auth store by id). A login made after the start is picked up by the next selection. A provider with no account is an error at selection (`models.NoAccountError`).
- A session option value, `_meta.towa.model`, `/model` and a subagent call's `model` may name a model as `model`, `model#effort`, `provider:model` or `provider:*/model`. `config.ResolveModel` maps it to an entry: matches that differ only in account select the first, whose chain falls back to the rest. Matches that differ in provider or effort are refused with the matches listed, since they send different requests. The option holds the resolved entry, so the client sees the configured text.
- In a short name, `x:y` is read as `provider:model` only when `x` is a provider some entry uses. Otherwise the colon is the model's own (`llama3.2:3b`).

## Why

The user wanted same-provider fallback across accounts without listing each account, and the model name alone to work in config and over ACP. Expanding at selection keeps `models` as the one place that says what may run: only entries that name the provider reach its accounts.

## Not done

Accounts are tried in list order. Ordering them by rate-limit headroom would need per-account state that outlives one turn's router. Revisit if a fixed order sends most turns to an account that is already limited.
