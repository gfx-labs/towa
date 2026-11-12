# A provider entry says what its models are

2026-11-12

## Why

A model the built-in table does not have, on a provider with no model list, needed `model_context_window` and `model_max_output_tokens`. Those two keys are global: they give the figures of every such model, and they outrank a provider's model list.

- A user config that set them for one gateway model gave the same figures to a Claude model selected by another config file. A config for Claude had to set both back to 0.
- Two gateway models with different limits could not both be configured.

## What changed

- A `providers` entry takes `models`, a list of `id`, `context_window` and `max_output_tokens`. The id is the model as a chain entry writes it.
- A figure given there is the one used for that model on that provider. It outranks the built-in table, because a gateway may serve a model with other limits than its maker does. On any other provider the model keeps what it had.
- A figure left out, or 0, is not set. It comes from where it would without the entry: the table, the two global keys, the provider's model list, then the model's name.
- An id listed twice under one provider and a negative figure are refused at load.
- The error for a model nothing is known about names the provider whose `models` would describe it, when the provider is a configured one, and then the two global keys.
- The two global keys stay. They are still the short way to run one unknown model from flags.

```yaml
providers:
  - id: openrouter-morph
    kind: chat
    base_url: https://openrouter.ai/api/v1
    openrouter:
      enabled: true
      routing:
        order: ["morph/fp8"]
        allow_fallbacks: false
    models:
      - id: deepseek/deepseek-v4.1-flash-1017
        context_window: 1048576
        max_output_tokens: 393216
```

## The output limit is not the largest the endpoint allows

towa sends `max_output_tokens` to a chat endpoint as `max_tokens` with every request. OpenRouter adds it to the prompt and refuses the request when the sum passes the endpoint's context length.

Measured on `deepseek/deepseek-v4.1-flash-1017` pinned to `morph/fp8`, whose endpoint row gives a context length of 1,048,576 and a largest completion of 943,718, with one prompt of about 157,509 tokens by OpenRouter's count:

| `max_tokens` | Result |
|---|---|
| 943718 | 400: "you requested about 1101227 tokens (157509 of text input, 943718 in the output)" |
| 393216 | answered by Morph, 140,038 prompt tokens billed |

393,216 is the model's `top_provider.max_completion_tokens` in OpenRouter's model list. With it, a prompt can reach 655,360 tokens. Reactive compaction starts at 400,000 by default (`compaction.reactive_threshold_tokens`), which is below that. No session was run to that size.

Not done: towa does not lower `max_tokens` to what the window has left for a request. A model configured with the endpoint's largest completion fails on a long prompt, as the first row shows.

## One task on the model

`oku-trade/trade` PR 4668 through towa `e3f0db6` and pi 1.3.0, both on `deepseek/deepseek-v4.1-flash-1017` pinned to `morph/fp8` at medium effort, one run each (`compare-coding-agents`, output `eval19-4668-morph`). towa's limits came from the provider entry, with both global keys at 0.

| Agent | Requests | Tokens | First to last request | Wall s | Cost USD | Change | Merged PR's tests |
|---|---|---|---|---|---|---|---|
| towa | 16 | 417,407 | 18,466 to 30,037 | 151 | 0.0033 | 2 files, +31/-1 | 6 of 6 pass |
| pi | 9 | 214,433 | 18,863 to 26,236 | 46 | 0.0024 | 1 file, +1/-1 | 6 of 6 pass |

- Both made the same fix, trimming the search input where it is passed on. towa also wrote a test file: requests 11 to 15 of its 16 list the directory, read a neighbouring test, write the file, run it and format it. Requests 9 and 10 format, lint and diff the source edit.
- OpenRouter's generation records name Morph for all 9 of pi's requests. towa keeps no generation id, so its upstream was not confirmed that way. A request body captured from towa in a separate check, with the same routing settings, carries `provider: {order: ["morph/fp8"], allow_fallbacks: false}`.
- One run each says nothing about which agent takes fewer requests on this model.
