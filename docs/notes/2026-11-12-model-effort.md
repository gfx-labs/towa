# Reasoning effort per model

2026-11-12

## Decision

A configured model takes `effort`, a word sent with every request of its chain:

```yaml
models:
  opus:
    chain: ["claude:work/claude-opus-5-5"]
    effort: medium
```

The word is not checked. Which levels a model takes differs by provider and by model and changes with each release, so a table here would be wrong sooner than it would help. A word the provider does not take comes back as the provider's error on the first request. Left out, nothing is sent and the provider's default applies (`medium` on Claude Opus 5.5 and Haiku 5.5, `high` on the other Claude models).

## Built

- `config.Model.Effort`, carried by `models.Model.Effort` into `loop.Resolved.Effort`. The loop and the compaction summary already read that field, and the three provider clients already send it: `output_config.effort` (Messages), `reasoning.effort` (Responses), `reasoning_effort` or OpenRouter's `reasoning.effort` (chat).
- The `--models` help names the members of a model.

## Limits

- One word for the whole chain. A chain that mixes providers needs a word every entry takes.
- No per-session switch. A second named model with another effort does that today through the `model` option.
- The `claude` client still sends no `thinking` parameter. Thinking is on by default from Claude 5, so effort steers it there, with the thinking text omitted. On Opus 4.6 to 4.8 and Sonnet 4.6 thinking stays off and effort changes only the output.
