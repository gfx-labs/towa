# Web search

2026-11-11

## Decision

`websearch` is done by towa, not by the model's provider. The design had said to use each provider's built-in search tool. That needs three tool definitions, three kinds of stored provider-only items and recordings from three paid APIs, and a chain that falls back to another provider mid-turn would change what search is. One local tool has none of that.

## Built

- `src/tools/websearch`: `Search(ctx, client, cfg, Input{Query, Results}) ([]Result, error)`. It posts the form `q`, `kl` to `websearch.endpoint` and reads each `a.result__a` (title, address) and the `a.result__snippet` after it with `golang.org/x/net/html`, already a dependency. A result written as DuckDuckGo's own redirect (`/l/?uddg=`) is unwrapped. Errors are `*websearch.Error` with a `Kind`.
- `src/tools/extras/websearch.go`: the tool, with jcode's shape (`query`, `num_results`).
- `src/config/websearch.go`: the `[websearch]` section.
- `towa hosts` lists `websearch.endpoint`.

| key | default | source |
|---|---|---|
| `endpoint` | `https://html.duckduckgo.com/html/` | jcode `crates/jcode-app-core/src/tool/websearch.rs:219` at 037163c7b |
| `region` | `us-en` | same file, line 229 |
| `default_results`, `max_results` | 8, 20 | same file, line 81 |
| `timeout`, `max_body_bytes` | 30s, 5 MiB | the webfetch figures |
| `user_agent` | `towa` | ours |

## Measured against the live endpoint, 2026-11-11

- POST with `User-Agent: towa`: HTTP 200, ten results, 28763 bytes. jcode sends a Chrome user agent. It was not needed, so none is imitated.
- A query of nonsense words still returned ten results, so an empty result page was not observed. `Search` returns no results and no error for a page with none.
- Six POSTs in about two seconds with `User-Agent: curl/8`: every one HTTP 202 with a 14 KB page holding `anomaly-modal` and no results. This is `KindBlocked`. The status is a success, so the markup is what tells it apart.
- Through `src/httpclient`, one request each: POST with `User-Agent: towa` returned ten results, and so did POST with a Chrome user agent. POST with `User-Agent: curl/8.5.0` and a GET of `/html/?q=` with `User-Agent: towa` were both challenged. So POST is required, and a user agent naming curl is refused.
- The tool itself with the default configuration, a new client per search, eight searches four seconds apart: all eight returned results. One search before those, about five minutes after the burst above, was challenged while curl with the same headers got results a few seconds later. The cause was not found.

`testdata/results.html` and `testdata/challenge.html` are those two responses, unedited.

## Limits

- One backend. jcode falls back to Bing HTML, and has Kagi, SearXNG and a custom JSON endpoint. None is built: a worker pool that gets challenged is the case that would justify one, and it has not been measured.
- No retry on a challenge. The model is told to wait.
- How many searches an address may make before it is challenged was not measured, and neither was a pool of workers behind one address.
- The parser follows DuckDuckGo's class names. A markup change shows up as searches with no results, not as an error.
