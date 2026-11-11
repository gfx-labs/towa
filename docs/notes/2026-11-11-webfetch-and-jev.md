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
| `user_agent` | `acpharness` | ours. Go's default names the HTTP library |

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
- No check of where a URL points. A model can fetch `http://127.0.0.1:…` or a cloud metadata address. The design leaves isolation to whatever runs the harness (as `tools.files.confine` does), and a proxy is where such a rule belongs. This needs a decision before the tool is exposed.
- Redirects follow the client's policy: ten at most, and https to http is allowed.
- No cache, no robots.txt, no retry.
- `[webfetch]` is not loaded from a config file in any test, because `Config.WebFetch` is not in `config.go` yet.
- Not run on macOS. It builds.
