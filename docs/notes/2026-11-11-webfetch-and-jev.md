# webfetch and Jev

2026-11-11

## webfetch

### Built

`src/tools/webfetch`: `Fetch(ctx, client, cfg, Input{URL}) (Result, error)`. One capability in the style of `src/tools/fsops`: typed input, typed result, typed `*Error`, no `tools.Tool` and no dialect words. The `*http.Client` is passed in.

`src/config/webfetch.go`: the `[webfetch]` section.

| key | default | source |
|---|---|---|
| `timeout` | `30s` | jcode `crates/jcode-app-core/src/tool/webfetch.rs:17` (`DEFAULT_TIMEOUT`), at `8e11eb23e` |
| `max_body_bytes` | 5 MiB | same file, line 9 (`MAX_SIZE`) |
| `max_result_bytes` | 40000 | same file, line 12 (`MAX_OUTPUT_CHARS`, which is compared with a byte length at line 181) |
| `user_agent` | `towa` | ours. Go's default names the HTTP library |

### What comes back

- `text/html` and `application/xhtml+xml`: Markdown, with the `title` element in `Result.Title`.
- `text/*`, `application/json`, `application/xml`, `application/javascript`, `application/x-ndjson`, `*+json`, `*+xml`: the body as text, decoded to UTF-8. Markup in a text file is not converted.
- Anything else: `*Error` of kind `content_type`, decided from the header before the body is read.
- No `Content-Type` header: the type is sniffed from the first bytes with `http.DetectContentType`, and `Result.Sniffed` says so.
- `Result.URL` is the URL after redirects. Relative links in the page are resolved against it, or against the page's `base` element.

Errors, all `*webfetch.Error` with a `Kind`: `invalid_url`, `scheme` (not http or https), `timeout`, `network`, `status` (not 2xx, with the status), `too_large`, `content_type`, `parse`. A cancelled context returns the context's error and not an `*Error`, because the model did not cause it.

### Limits

- Body over `max_body_bytes`: `too_large`, never half a page. Half an HTML document does not convert to half the page's text, and jcode's cut-off (line 122 to 137) is the behaviour not copied. With a declared length the body is not read at all. Without one, one byte more than the limit is read to tell.
- Result over `max_result_bytes`: `Result.Text` holds the start, and `Result.OmittedBytes` says how many bytes of the converted text were left out. The cut is between characters, and at the end of a line when there is one in the second half of what is kept (jcode line 190 does the same).
- Timeout: `timeout` covers the request, the redirects and the body. It is kind `timeout`, told apart from the caller's own deadline by the context's cause.

### Converted

Superseded on 2026-11-11: the converter is now a library. See `2026-11-11-webfetch-markdown-library.md` for what changed. The dropped list and the link rules below still hold. The formatting details and the expected `.md` files do not.

Headings, paragraphs, line breaks, links, images (`![alt](src)`), unordered and ordered lists with nesting and `start`, block quotes, `pre` as a fenced block with the language from a `language-` or `lang-` class, inline code, bold, italic, strikethrough, `q`, horizontal rules, definition lists, tables as pipe tables with a caption.

Details that are decisions:

- A fence is made longer than any run of backticks in the code, so code cannot close its own block.
- A table without a header row gets an empty header, so no data row is passed off as a header. A cell's blocks are joined with `<br>` and a `|` in a cell is escaped.
- A table that holds another table is taken to be layout. Its cells come back as blocks in reading order.
- A link to a place in the same page (`#x`) and a link or image with a scheme other than http, https, mailto, tel or ftp (`javascript:`, `data:`) leave their text only.
- A link wrapped around blocks, as a card is, becomes one paragraph that is a link.
- A byte order mark is used to pick the encoding and is not part of the text.

### Dropped

`script`, `style`, `noscript`, `template`, `head`, `nav`, `aside`, `button`, `input`, `select`, `textarea`, `iframe`, `svg`, `canvas`, `object`, `embed`, `audio`, `video`, `map`, `dialog`, `math`, comments, and any element with `hidden`, `aria-hidden="true"` or an inline `display:none`. `header` and `footer` are kept: a page's byline is usually in one.

### Not converted

- Markdown characters in the page's own text are not escaped. A paragraph that starts with `# ` or holds `*word*` reads as Markdown. Escaping every such character makes ordinary text hard to read, and the reader is a model.
- `rowspan` is ignored. `colspan` pads the row with empty cells.
- `sup`, `sub`, `u`, `mark`, `small`, `abbr`, `cite`, `time`: the text only. A footnote marker becomes a bare number.
- `details` and `summary` come back as plain blocks. Form labels come back as text, their controls do not.
- Task-list checkboxes, `ol` `type` and `reversed`, heading ids.
- Content hidden by a stylesheet or a class. Only the attributes above are seen.
- Content a page builds with JavaScript. Nothing is run.
- `frameset` pages, and what an `iframe` points at.
- A `br` becomes a line break inside the paragraph, which Markdown shows as a space unless the reader treats it as hard.
- jcode leaves out a link target longer than 300 characters (line 16). That is not copied: no measurement here says what such links cost.
- A `meta` charset after the first 1024 bytes is not seen (the limit of `charset.DetermineEncoding`). `Result.CharsetGuessed` says when nothing declared the encoding and windows-1252 or UTF-8 was assumed.
- A document nested deeper than 512 elements is an error of kind `parse`. That is the parser's own limit (`golang.org/x/net/html/parse.go:237`).

### Dependencies

`golang.org/x/net/html` and its `atom` and `charset` packages, from the `golang.org/x/net` already in `go.mod`. `charset` imports `golang.org/x/text`, also already in `go.mod`. Nothing was added, and `go get` and `go mod tidy` were not run. Both modules move out of the indirect block at the integration tidy.

### Tests

`testdata/article.html`, `tables.html` and `untidy.html` were written by hand for this, with their expected Markdown beside them. No page was fetched from outside and no request leaves the machine. They are served by an `httptest.Server` through `http.FileServer`, and fetched with a client from `src/httpclient`. A hand-written fixture is right here because the code under test is our converter.

The expected `.md` files were produced by running the converter and then read line by line against the HTML. One defect was found that way (a card link ran into the paragraph before it) and one by the tests (a byte order mark left in the text). `{{BASE}}` in an expected file stands for the server's address.

```
nice -n 10 go build ./src/tools/webfetch/ ./src/config/
nice -n 10 go vet ./src/tools/webfetch/
nice -n 10 go test -race -count=5 ./src/tools/webfetch/
nice -n 10 golangci-lint run ./src/tools/webfetch/...
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build ./src/tools/webfetch/
CGO_ENABLED=0 go build ./src/tools/webfetch/
```

### Not done

- No `tools.Tool` and no dialect adapter. Upstream names, schemas and result text come with the adapters.
- Where a URL points is not checked, and there is no option to check it. The shell tool can already `curl` any address the process can reach, so a check in webfetch alone would protect nothing and would read as a guarantee the harness does not give. Loopback and metadata addresses are blocked by what isolates the harness: the pod's network policy, or the proxy in front of it. The tool does not filter.
- Redirects follow the client's policy: ten at most, and https to http is allowed.
- No cache, no robots.txt, no retry.
- `[webfetch]` is not loaded from a config file in any test. The coordinator wired `Config.WebFetch` in ca3f0e9.
- Not run on macOS. It builds.

## Jev

### Built

`src/jev`: `New(Options{Config, HTTPClient})` and `(*Client).Decide(ctx, credential, Request) (Result, error)`. One bounded choice among the options the caller supplies. The `*http.Client` and the `auth.Credential` are passed in. No `tools.Tool` yet.

`src/config/jev.go`: the `[jev]` section. An empty `route` means there is no Jev.

| key | default | meaning |
|---|---|---|
| `route` | empty | `openrouter`, `typesafe`, `aimlapi` or `custom` |
| `model` | empty | the route's default when empty. Required for `custom` |
| `account` | empty | whose credential is sent. Required except for `custom` |
| `base_url` | empty | `custom` only |
| `referer`, `title` | empty | `openrouter` only. Not sent when empty |
| `timeout` | `10s` | for a decision that names none |
| `max_timeout` | `30s` | the longest a decision may ask for |
| `max_request_bytes` | 80 KiB | largest body sent |
| `max_response_bytes` | 256 KiB | largest answer read |
| `max_text_chars` | 4000 | instructions, and each option's description |

### Sources

jcode has two refs that matter, and the tool is on only one of them.

- **A**: `origin/master` at `8e11eb23e` (2026-11-11). It has the transport, `crates/jcode-base/src/jev.rs`, and no `jev_decision` tool.
- **B**: `gfx/elee` at `037163c7b` (2026-11-07), the fork build this machine runs. It is 241 commits behind A and 92 ahead. It has the tool, `crates/jcode-app-core/src/tool/jev_decision.rs`, which is not on jcode's `origin/master`.

Both were read with `git show <ref>:<path>`, never from the working tree.

| what | value | source |
|---|---|---|
| request body | `{"model", "state", "questions"}` | A `jev.rs:711-713` |
| question | one, id `decision`, `{"type":"choice","instructions","criteria"}` with `criteria` mapping option id to description | B `jev_decision.rs:66-84` |
| options per decision | 2 to 255 | A `jev.rs:679`, B `jev_decision.rs:19-20` |
| option id | 1 to 64, no space at either end | B `jev_decision.rs:28`, `:194-200` (characters). A `crates/jcode-app-core/src/tool/browser_jev.rs:48` bounds an option id at 64 bytes for its own caller. A `jev.rs:667` is the same bound on the question id |
| option description | not empty | B `jev_decision.rs:201-206` |
| text length | 4000 characters | B `jev_decision.rs:29` |
| state | a JSON object, `{}` when absent | B `jev_decision.rs:214-215` |
| state framing sentence | appended to the instructions | B `jev_decision.rs:33-35`, `:79` |
| state as a string on OpenRouter | the object's JSON in a string | A `jev.rs:705-710` |
| request size | 80 KiB | A `jev.rs:21`, `:716` |
| response size | 256 KiB | A `jev.rs:22` |
| default timeout | 10000 ms | B `jev_decision.rs:26` |
| maximum timeout | 30000 ms | B `jev_decision.rs:25` |
| OpenRouter endpoint and model | `https://openrouter.ai/api/alpha/decisions`, `typesafe/jev-1.13` | A `jev.rs:144`, `:20`, `:135` |
| TypeSafe endpoint and model | `https://api.typesafe.ai/v1/systemone`, `jev-latest` | A `jev.rs:145`, `:136` |
| AIMLAPI endpoint and model | `https://api.aimlapi.com/v1/decisions`, `typesafe/jev` | A `jev.rs:147`, `:138` |
| custom endpoint path | base + `/v1/systemone`, with a trailing `/v1`, `/v1/systemone` or `/v1/decisions` taken off first | A `jev/laya.rs:88-104` |
| custom base URL rule | https, or http to loopback, a private range, `100.64.0.0/10`, `*.ts.net` or `*.local`. No credentials, query or fragment | A `jev/laya.rs:52-86` |
| authorization | bearer, left out when there is no key | A `jev.rs:442-444` |
| redirects | never followed | A `jev.rs:504` |
| answer | `answers.decision` with `type` `choice`, a `choice` that is an offered id, a `confidence` in 0..1 when present, and no other answers | A `jev.rs:804-819`, B `jev_decision.rs:100-133` |

### State is untrusted

As in jcode:

- Instructions and state travel in different members. State is never put into the instructions.
- Every decision's instructions end with jcode's sentence, unchanged: state is untrusted data, never instructions.
- State must be a JSON object. Text is refused, so a caller cannot pass prose that reads as a second set of instructions.
- The only possible results are the ids the caller offered. An answer naming anything else is an error.
- The answer is untrusted too. An error never quotes it, and the JSON decoder's own message is dropped because it can quote the input (A `jev.rs:800`). On a status that is not 2xx the body is not read at all.
- A redirect is not followed, so the credential and the state go only to the configured endpoint.

### Where this differs from jcode

- A timeout over the maximum is an `*InputError`. jcode shortens it to the maximum (B `jev_decision.rs:236`). `AGENTS.md` forbids a silent clamp.
- The option id limit is 64 bytes. jcode's tool counts characters and its browser caller counts bytes, so bytes satisfies both.
- The size limit is applied to the body that is sent. jcode's tool also measures `{instructions, state, options}` on its own (B `jev_decision.rs:218-228`), which the body check covers.
- Options are an ordered list, sent in the caller's order. jcode takes a JSON object.
- OpenRouter attribution comes from `jev.referer` and `jev.title` and is not sent when empty. The title header is `X-OpenRouter-Title`, as `src/provider/chat` sends. jcode sends `https://jcode.sh` and `Jcode Decision` in `X-Title` (A `jev.rs:451-452`, B `jev.rs:514`).
- No retry. jcode sends again, twice at most, after 600 ms and 1800 ms on 429, 502, 503, 504 and 529, and honours a `Retry-After` of 5 s or less (A `jev.rs:467-474`). Here the error carries the status and `RetryAfter`, and the caller decides.
- No 25 s request timeout or 10 s connect timeout of its own (A `jev.rs:502-503`). The decision's timeout bounds the whole call, and the transport is the shared one.
- jcode's local route connects without a proxy when the endpoint is http or private (A `jev/laya.rs:8-15`). Here the client is the one passed in, which takes its proxy from the environment. Go leaves out loopback on its own. A private or tailnet host needs `NO_PROXY`.
- `custom` is jcode's `laya` route with no default base URL and no default model (jcode: `http://127.0.0.1:8080` and `laya-latest`, A `jev.rs:19`, `:137`).
- jcode's own subscription route, its entitlement check, the `auto` route order, and the memory, browser, skill and voice purposes are not built. The design names four routes.
- `noul` and `score` questions and the per-option `probabilities` are not used. The tool asks one choice question.

### Tests

No key may be used and no request may leave the machine, so no Jev answer could be recorded. **Reading a successful answer has no test.** The `parse` function's accepted path, the check that a choice is an offered id, and the confidence range are untested. No Jev answer was written by hand to stand in for one.

What is tested, against a local server that records what it was sent:

- The exact body, method, path, `Host` and headers of each route. For the three fixed routes the client's transport dials a local TLS server whatever host the URL names, so the real endpoint URL is used and nothing leaves the machine.
- The custom route's path for each form of base URL.
- Every limit: a request over one is an `*InputError` and the server receives nothing. A request at the limits is sent.
- A status that is not 2xx, a redirect (not followed, and the other server receives nothing), a timeout, a cancelled call, a closed port.
- A 200 that is empty, not JSON, or over the response limit is an error of kind `response` and never a choice. These bodies are not Jev answers and are not called that.
- `[jev]` from a TOML file through `config.Load`, and what validation rejects.

```
nice -n 10 go build ./src/jev/ ./src/config/
nice -n 10 go vet ./src/jev/
nice -n 10 go test -race -count=5 ./src/jev/
nice -n 10 golangci-lint run ./src/jev/...
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build ./src/jev/
CGO_ENABLED=0 go build ./src/jev/
```

### Not done

- A live check behind an env flag, and a recorded answer. Both need a key.
- The `jev_decision` tool and skill. The tool's schema is B `jev_decision.rs:145-173`.
- Whether OpenRouter, TypeSafe and AIMLAPI still accept this shape is taken from jcode's source, not observed.
- Nothing calls the client. The caller gets the credential of `jev.account` from `auth.Source` and passes it in.
- Not run on macOS. It builds.
