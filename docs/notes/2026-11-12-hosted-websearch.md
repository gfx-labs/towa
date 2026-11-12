# Provider-hosted web search

2026-11-12

## Decision

Where the model's provider runs web search itself, acpharness uses it, as jcode does by default (`prefer_native`). Our own `websearch` tool stays for every other provider. This reverses part of the 2026-11-11 note, which used only the local tool.

## Built

- `src/provider/hostedsearch`:
  - `Tool(cfg, providerID, model)` returns the definition for a chain entry. `claude` gets `{"type":"web_search_20250305","name":"web_search","max_uses":N}` for the Messages API. `codex` gets `{"type":"web_search"}` for the Responses API, except a model with `codex` in its name. A configured `[[providers]]` endpoint gets none, because one that speaks the same API may not implement hosted tools. The version, the codex-model rule and the first-party rule are jcode's (`crates/jcode-provider-{anthropic,openai}-runtime/src/native_web_search.rs` at 037163c7b).
  - `Read(item)` reads a stored search item: Anthropic's `server_tool_use` and `web_search_tool_result`, OpenAI's `web_search_call`.
  - `Portable(items, provider, origin, declared)` replaces search items with text for a request that cannot carry them.
- `router.Preparation` carries the entry's hosted tool. `loop` builds each request's tools from the session's set and swaps `websearch` for it. The swap is per entry, so a fallback from `codex` to an OpenRouter entry gets the local tool back.
- A session whose tools do not include `websearch` (`websearch.enabled = false`) is not given hosted search either.
- The client sees a search as a tool call of kind `search`, live and on replay of a loaded session.
- Config: `websearch.hosted` (default true) and `websearch.hosted_max_uses` (default 5, Anthropic only, zero sends no cap). Both defaults are jcode's.

The provider clients needed no change: both already stored an item type they do not know as a native item and sent it back whole.

## Replay rule

A search item is sent back as it came only when the request goes to the same provider, model and account and declares the hosted tool. Otherwise it becomes an assistant text item: `[web_search for "query"] (summary of an earlier provider-side search; page contents are no longer available)` and the titles and URLs the item holds. Before this, a native item with another origin made the request invalid, so a model switch after a search would have ended the session.

## Measured, 2026-11-12

- `src/provider/openai/testdata/codex_web_search.json` is a real response from the ChatGPT backend (`gpt-6-luna`, the user's `codex` account, one request). It holds one `web_search_call` with `action.query` and no `sources`, then a message with a `url_citation`.
- Through the built binary, two turns in one session on the same account: the first searched and the client got a `tool_call` and a completed `tool_call_update`. The second request carried the stored `web_search_call` back and the backend accepted it.

## Not verified

- The `claude` provider. No Claude account or Anthropic key was available, so `api_web_search` and `oauth_web_search` in `src/provider/anthropic/replay_test.go` have no recording and are skipped. The definition and the reading of its two block types follow jcode's code, not a response seen here. Whether the Claude OAuth path accepts the tool is unknown.
- `api_web_search` for the OpenAI API-key endpoint has no recording either.
- A `pause_turn` during a long hosted search. The loop already resends on it.

## Limits

- No domain filters. jcode has allowed and blocked lists.
- A compaction summary leaves search items out, as it does every native item.
- Anthropic bills hosted searches on API keys (jcode's config comment gives about $10 per 1,000). `hosted_max_uses` caps one request, not a session.
