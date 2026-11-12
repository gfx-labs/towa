# webfetch: HTML to Markdown by a library

2026-11-11

`src/tools/webfetch/markdown.go` was a converter written here, 592 lines. The user asked for a Go Markdown library in its place, to cut the harness's line count. It is now `github.com/JohannesKaufmann/html-to-markdown/v2` v2.5.2, and the file is 159 lines.

## Dependency

- `github.com/JohannesKaufmann/html-to-markdown/v2` v2.5.2, MIT. Used: `converter` and the `base`, `commonmark`, `table` and `strikethrough` plugins.
- It brings `github.com/JohannesKaufmann/dom` v0.3.1, MIT, whose only requirement is `golang.org/x/net`, already here.
- The library's `go.mod` also lists goldmark, cascadia, termenv and others. They belong to its CLI and tests. `go list -deps ./cmd/towa` shows none of them linked.
- Binary, `CGO_ENABLED=0 go build -trimpath ./cmd/towa` on linux amd64: 37,559,103 bytes before, 37,867,871 after, 308,768 more.
- `go mod tidy` also moved eleven modules that the code imports directly out of the indirect block. No version changed.

## What stays ours

The library decides the Markdown. `toMarkdown` parses the page, changes the tree, and hands it over:

- The title is read from the `title` element.
- The dropped elements are removed. The list is unchanged from `2026-11-11-webfetch-and-jev.md`, as are `hidden`, `aria-hidden="true"` and inline `display:none`. The library drops only `head`, `script`, `style`, `link`, `meta`, `iframe`, `noscript`, `input`, `textarea` and comments, so without this `nav`, `aside`, `svg`, `select`, `template` and hidden text reach the model.
- Links and images are made absolute here, against the final URL or the page's `base` element. The library's `WithDomain` does not read a `base` element.
- A link to a place in the same page, and a link whose scheme is not http, https, mailto, tel or ftp, lose their target and keep their text. An image with such a source is replaced by its `alt` text. The library would write `javascript:` and `data:` URLs out.

## What changed in the output

Seen by running the three fixture pages through both converters:

- Markdown characters in a page's text are now escaped, and `<` is written `&lt;`. The old converter escaped nothing.
- A `br` is a hard break (two trailing spaces). A horizontal rule is `* * *`.
- A table's caption comes after the table, and a cell's paragraphs are joined by `<br />`.
- A nested list is separated from its item's text by an empty line.
- `dd` is a plain paragraph with no `: ` marker, and `q` has no quotation marks.
- A link whose text is its own URL is `[url](url)`, not `<url>`.
- A link wrapped around blocks keeps the blocks' formatting inside the link text.
- Our rule that a table holding another table is layout is gone. The fixture's layout table still came out as blocks with the inner table intact. Other nestings were not tried.

None of these was tuned back to the old output. Options set: no list end comment, links with an empty target or empty text are skipped, cells with line breaks stay in their table, and table columns are not padded, since padding spends the result limit on spaces.

## Tests

The byte-exact comparison against `article.md`, `tables.md` and `untidy.md` is removed with those files and `tables.html`. It would now test the library's formatting. What remains checks this package's decisions on `article.html` and `untidy.html`: the title, absolute links, the `base` element, same-page links, a `data:` image leaving its description, and the list of text that must not appear.

Not tested: nothing compares the library's Markdown for a real page against a reading of that page.
