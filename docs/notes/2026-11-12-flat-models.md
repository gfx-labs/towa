# Models as one flat list

2026-11-12

## Decision

`models` is a list of text, in order. It replaces the map of named models and `default_model`.

```yaml
models:
  - claude:work/claude-opus-5-5#medium
  - claude:personal/claude-opus-5-5#medium
  - codex:chatgpt/gpt-6.1-sol
```

```
towa --models claude:work/claude-opus-5-5#medium,codex:chatgpt/gpt-6.1-sol
```

- An entry is `provider:account/model`, optionally followed by `#effort`. The effort is the text after the last `#`. A provider's model name may hold `/`, `:` and `@`, so none of those could mark it.
- An effort is sent as written with a request to that entry and is not checked. An entry without one sends none, and the provider's default applies.
- A session selects an entry by its whole text, effort included, and starts with the first. The ACP model option lists the entries in list order.
- The selected entry's chain is itself and every entry after it. A request that an entry cannot take (rate limit, quota, auth, overload) goes to the next. A session that selects the second entry never goes back to the first.
- `model_context_window` and `model_max_output_tokens` are top-level keys with the figures of any model the built-in table does not have. A model the table has keeps the table's figures. They are not named `context_window` and `max_output_tokens` because `limits.max_output_tokens` is a different thing, the per-turn limit.
- `subagents.models` lists entries of `models`, each written as it is there.
- An entry listed twice is an error: a session could not tell the two apart.

## Why

The user asked for models that can be given on the command line in order, with effort in the model's text. A map of structs is one of the six keys kong has no flags for (`--models=YAML`). A list of text is an ordinary flag.

## What changed in behavior

- A fallback can now move to a different model, not only another account of the same one, because the chain is the rest of the list. Order the list with that in mind.
- A model the table does not know anywhere in the list makes every entry before it unusable until the two figures are set, since each of those can fall back to it. `session.AddModels` reports it at start.
- The two figures were per named model and could replace the table's. They are now global and only fill in for models the table lacks.
- Effort was one word for a whole chain. It is now per entry, carried on `router.Entry` and set on the request when the router moves, so a chain that mixes providers can give each its own word.
- Config files no longer merge models across layers. The last file that sets `models` wins, as for any other key, and a flag replaces the files. The loader's table merge, which only the models map used, is removed.

## Checked

Live on the `a-ant` account, 2026-11-12, models given by flag:

- `claude:a-ant/claude-opus-5-5#medium`: the turn answered.
- `claude:a-ant/claude-opus-5-5,claude:a-ant/claude-sonnet-5-5#low`: the session started on the first, listed both in order, and the turn answered.
- `claude:a-ant/claude-opus-5-5#banana`: the provider's 400 came back naming the words it takes.

`TestAFallbackSendsTheNextEntrysEffort` serves a real HTTP endpoint that refuses every request and reads the model and effort of each one it gets.

## Not done

- No entry can be marked "selectable but never a fallback target" or the reverse.
