package webfetch

import (
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// dropped elements carry nothing a reader of the page's text wants: code,
// style, navigation, controls and embedded media.
var dropped = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Template: true,
	atom.Head: true, atom.Nav: true, atom.Aside: true,
	atom.Button: true, atom.Input: true, atom.Select: true, atom.Textarea: true,
	atom.Iframe: true, atom.Svg: true, atom.Canvas: true, atom.Object: true, atom.Embed: true,
	atom.Audio: true, atom.Video: true, atom.Map: true, atom.Dialog: true, atom.Math: true,
}

// blockTags start a new block.
var blockTags = map[atom.Atom]bool{
	atom.Html: true, atom.Body: true, atom.P: true, atom.Div: true, atom.Section: true, atom.Article: true,
	atom.Main: true, atom.Header: true, atom.Footer: true, atom.Figure: true, atom.Figcaption: true,
	atom.Details: true, atom.Summary: true, atom.Address: true, atom.Fieldset: true, atom.Form: true,
	atom.Center: true, atom.Hgroup: true,
	atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
	atom.Blockquote: true, atom.Pre: true, atom.Hr: true,
	atom.Ul: true, atom.Ol: true, atom.Menu: true, atom.Li: true,
	atom.Dl: true, atom.Dt: true, atom.Dd: true,
	atom.Table: true, atom.Caption: true, atom.Thead: true, atom.Tbody: true, atom.Tfoot: true,
	atom.Tr: true, atom.Td: true, atom.Th: true,
}

// linkSchemes are the schemes a link or an image is kept with. Any other,
// such as javascript: or data:, leaves only the text.
var linkSchemes = map[string]bool{"http": true, "https": true, "mailto": true, "tel": true, "ftp": true}

// converter turns a parsed document into Markdown. It recurses over the
// tree, which is safe because the parser refuses a document nested deeper
// than 512 elements.
type converter struct {
	base *url.URL
}

// toMarkdown converts an HTML document to Markdown and returns its title.
// Links and images are made absolute against base, or against the
// document's base element when it has one.
func toMarkdown(doc string, base *url.URL) (markdown, title string, err error) {
	root, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		return "", "", err
	}
	c := &converter{base: base}
	for n := range root.Descendants() {
		if n.Type != html.ElementNode {
			continue
		}
		switch n.DataAtom {
		case atom.Title:
			if title == "" && n.Namespace == "" {
				title = oneLine(textContent(n))
			}
		case atom.Base:
			if href := attr(n, "href"); href != "" && c.base == base {
				if u, err := base.Parse(href); err == nil {
					c.base = u
				}
			}
		}
	}
	markdown = strings.Join(c.blocks(root), "\n\n")
	if markdown != "" {
		markdown += "\n"
	}
	return markdown, title, nil
}

func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name && a.Namespace == "" {
			return a.Val
		}
	}
	return ""
}

func hasAttr(n *html.Node, name string) bool {
	for _, a := range n.Attr {
		if a.Key == name && a.Namespace == "" {
			return true
		}
	}
	return false
}

// skip says the element and everything in it is left out.
func skip(n *html.Node) bool {
	if dropped[n.DataAtom] || hasAttr(n, "hidden") || attr(n, "aria-hidden") == "true" {
		return true
	}
	style := strings.ReplaceAll(strings.ToLower(attr(n, "style")), " ", "")
	return strings.Contains(style, "display:none")
}

// isBlock says the element starts a block: it is a block element, or it is
// an inline element that was wrapped around one, as a span around a div.
// A link around a block stays a link, with the block's text as its text.
func isBlock(n *html.Node) bool {
	if blockTags[n.DataAtom] {
		return true
	}
	if n.DataAtom == atom.A {
		return false
	}
	return holdsBlock(n)
}

func holdsBlock(n *html.Node) bool {
	for d := range n.Descendants() {
		if d.Type == html.ElementNode && blockTags[d.DataAtom] {
			return true
		}
	}
	return false
}

// blocks returns the Markdown blocks of n's children. Text and inline
// elements between two block elements make one paragraph.
func (c *converter) blocks(n *html.Node) []string {
	var out []string
	var inline strings.Builder
	flush := func() {
		if p := paragraph(inline.String()); p != "" {
			out = append(out, p)
		}
		inline.Reset()
	}
	for child := range n.ChildNodes() {
		switch {
		case child.Type == html.TextNode:
			inline.WriteString(collapse(child.Data))
		case child.Type == html.DocumentNode || child.Type == html.DoctypeNode || child.Type == html.CommentNode:
			if child.Type == html.DocumentNode {
				out = append(out, c.blocks(child)...)
			}
		case child.Type != html.ElementNode || skip(child):
		case isBlock(child):
			flush()
			out = append(out, c.block(child)...)
		case child.DataAtom == atom.A && holdsBlock(child):
			// A link around blocks, as a card is, is a paragraph of its own.
			flush()
			inline.WriteString(c.inline(child))
			flush()
		default:
			inline.WriteString(c.inline(child))
		}
	}
	flush()
	return out
}

// block returns the Markdown blocks of one block element.
func (c *converter) block(n *html.Node) []string {
	switch n.DataAtom {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		text := oneLine(c.inlineChildren(n))
		if text == "" {
			return nil
		}
		// The level is the tag's digit.
		return []string{strings.Repeat("#", int(n.Data[1]-'0')) + " " + text}
	case atom.Blockquote:
		return one(prefixLines(strings.Join(c.blocks(n), "\n\n"), "> ", "> "))
	case atom.Pre:
		return one(codeBlock(n))
	case atom.Hr:
		return []string{"---"}
	case atom.Ul, atom.Ol, atom.Menu:
		return one(c.list(n))
	case atom.Dd:
		return one(prefixLines(strings.Join(c.blocks(n), "\n\n"), ": ", "  "))
	case atom.Table:
		return c.table(n)
	default:
		return c.blocks(n)
	}
}

// one returns a block as a list, or nothing when it is empty.
func one(block string) []string {
	if block == "" {
		return nil
	}
	return []string{block}
}

// list returns a ul or ol as one block.
func (c *converter) list(n *html.Node) string {
	number, ordered := 1, n.DataAtom == atom.Ol
	if start, err := strconv.Atoi(attr(n, "start")); err == nil && ordered {
		number = start
	}
	var items []string
	for child := range n.ChildNodes() {
		if child.Type != html.ElementNode || skip(child) {
			continue
		}
		// A list directly inside a list, which is not valid HTML and is
		// common, is shown as nested.
		var parts []string
		if child.DataAtom == atom.Li {
			parts = c.blocks(child)
		} else {
			parts = c.block(child)
			if len(items) > 0 && (child.DataAtom == atom.Ul || child.DataAtom == atom.Ol) {
				items[len(items)-1] += "\n" + prefixLines(strings.Join(parts, "\n"), "  ", "  ")
				continue
			}
		}
		if len(parts) == 0 {
			continue
		}
		// An item's text followed only by nested lists stays tight.
		sep := "\n"
		for _, part := range parts[1:] {
			if !startsList(part) {
				sep = "\n\n"
			}
		}
		marker := "- "
		if ordered {
			marker = strconv.Itoa(number) + ". "
			number++
		}
		items = append(items, prefixLines(strings.Join(parts, sep), marker, strings.Repeat(" ", len(marker))))
	}
	return strings.Join(items, "\n")
}

func startsList(block string) bool {
	if strings.HasPrefix(block, "- ") {
		return true
	}
	digits := 0
	for digits < len(block) && block[digits] >= '0' && block[digits] <= '9' {
		digits++
	}
	return digits > 0 && strings.HasPrefix(block[digits:], ". ")
}

// table returns a table as one pipe table, with its caption before it. A
// table that holds another table is laid out for position and not for
// data, and its cells are returned as blocks in reading order.
func (c *converter) table(n *html.Node) []string {
	for d := range n.Descendants() {
		if d.Type == html.ElementNode && d.DataAtom == atom.Table {
			var out []string
			for cell := range n.Descendants() {
				if cell.Type == html.ElementNode && (cell.DataAtom == atom.Td || cell.DataAtom == atom.Th || cell.DataAtom == atom.Caption) &&
					nearestTable(cell) == n && !skip(cell) {
					out = append(out, c.blocks(cell)...)
				}
			}
			return out
		}
	}

	var out []string
	var rows [][]string
	header := false
	addRow := func(tr *html.Node, inHead bool) {
		var cells []string
		allHeader := true
		for cell := range tr.ChildNodes() {
			if cell.Type != html.ElementNode || (cell.DataAtom != atom.Td && cell.DataAtom != atom.Th) || skip(cell) {
				continue
			}
			allHeader = allHeader && cell.DataAtom == atom.Th
			// A cell is one line: its blocks are joined with <br>, which
			// pipe tables allow, and a pipe in its text is escaped.
			text := strings.Join(c.blocks(cell), "\n")
			text = strings.ReplaceAll(strings.ReplaceAll(text, "|", `\|`), "\n", "<br>")
			cells = append(cells, text)
			span, _ := strconv.Atoi(attr(cell, "colspan"))
			for i := 1; i < span; i++ {
				cells = append(cells, "")
			}
		}
		if len(cells) == 0 {
			return
		}
		if len(rows) == 0 && (inHead || allHeader) {
			header = true
		}
		rows = append(rows, cells)
	}
	for child := range n.ChildNodes() {
		if child.Type != html.ElementNode || skip(child) {
			continue
		}
		switch child.DataAtom {
		case atom.Caption:
			out = append(out, c.blocks(child)...)
		case atom.Tr:
			addRow(child, false)
		case atom.Thead, atom.Tbody, atom.Tfoot:
			for tr := range child.ChildNodes() {
				if tr.Type == html.ElementNode && tr.DataAtom == atom.Tr && !skip(tr) {
					addRow(tr, child.DataAtom == atom.Thead)
				}
			}
		}
	}
	if len(rows) == 0 {
		return out
	}
	width := 0
	for _, row := range rows {
		width = max(width, len(row))
	}
	var b strings.Builder
	line := func(cells []string) {
		b.WriteString("|")
		for i := range width {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			b.WriteString(" " + cell + " |")
		}
		b.WriteString("\n")
	}
	// A pipe table needs a header row. A table without one gets an empty
	// header, so that no data row is passed off as a header.
	if header {
		line(rows[0])
		rows = rows[1:]
	} else {
		line(nil)
	}
	b.WriteString("|" + strings.Repeat(" --- |", width) + "\n")
	for _, row := range rows {
		line(row)
	}
	return append(out, strings.TrimRight(b.String(), "\n"))
}

func nearestTable(n *html.Node) *html.Node {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.DataAtom == atom.Table {
			return p
		}
	}
	return nil
}

// codeBlock returns a pre element as a fenced block, with the language its
// class names when there is one.
func codeBlock(n *html.Node) string {
	text := strings.Trim(textContent(n), "\n")
	if strings.TrimSpace(text) == "" {
		return ""
	}
	lang := language(n)
	if lang == "" {
		for d := range n.Descendants() {
			if d.Type == html.ElementNode && d.DataAtom == atom.Code {
				lang = language(d)
				break
			}
		}
	}
	// The fence is longer than any run of backticks in the code.
	fence := strings.Repeat("`", max(3, longestRun(text, '`')+1))
	return fence + lang + "\n" + text + "\n" + fence
}

func language(n *html.Node) string {
	for class := range strings.FieldsSeq(attr(n, "class")) {
		for _, prefix := range []string{"language-", "lang-"} {
			if lang, ok := strings.CutPrefix(class, prefix); ok && lang != "" {
				return lang
			}
		}
	}
	return ""
}

func longestRun(s string, b byte) int {
	longest, run := 0, 0
	for i := range len(s) {
		if s[i] == b {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return longest
}

// textContent returns the text in n as it is written, with a br as a line
// break and without the elements that are left out.
func textContent(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for child := range n.ChildNodes() {
			switch {
			case child.Type == html.TextNode:
				b.WriteString(child.Data)
			case child.Type != html.ElementNode:
			case child.DataAtom == atom.Br:
				b.WriteString("\n")
			case child.DataAtom == atom.Script || child.DataAtom == atom.Style:
			default:
				walk(child)
			}
		}
	}
	walk(n)
	return b.String()
}

func (c *converter) inlineChildren(n *html.Node) string {
	var b strings.Builder
	for child := range n.ChildNodes() {
		switch {
		case child.Type == html.TextNode:
			b.WriteString(collapse(child.Data))
		case child.Type == html.ElementNode && !skip(child):
			b.WriteString(c.inline(child))
		}
	}
	return b.String()
}

// inline returns one element as inline Markdown. Its text has single
// spaces, and a line break is "\n".
func (c *converter) inline(n *html.Node) string {
	switch n.DataAtom {
	case atom.Br:
		return "\n"
	case atom.Strong, atom.B:
		return wrap("**", c.inlineChildren(n))
	case atom.Em, atom.I:
		return wrap("*", c.inlineChildren(n))
	case atom.Del, atom.S, atom.Strike:
		return wrap("~~", c.inlineChildren(n))
	case atom.Code, atom.Kbd, atom.Samp, atom.Tt:
		text := oneLine(textContent(n))
		if text == "" {
			return ""
		}
		fence := strings.Repeat("`", longestRun(text, '`')+1)
		if strings.HasPrefix(text, "`") || strings.HasSuffix(text, "`") {
			text = " " + text + " "
		}
		return fence + text + fence
	case atom.Q:
		return wrap(`"`, c.inlineChildren(n))
	case atom.Img:
		alt := oneLine(attr(n, "alt"))
		src := c.resolve(attr(n, "src"))
		if src == "" {
			return alt
		}
		return "![" + alt + "](" + src + ")"
	case atom.A:
		text := c.inlineChildren(n)
		href := c.resolve(attr(n, "href"))
		// A link to a place in the same page, or one a reader cannot
		// follow, leaves its text.
		if href == "" || strings.HasPrefix(strings.TrimSpace(attr(n, "href")), "#") {
			return text
		}
		lead, core, trail := edges(strings.ReplaceAll(text, "\n", " "))
		switch core {
		case "":
			return text
		case href:
			return lead + "<" + href + ">" + trail
		}
		return lead + "[" + core + "](" + href + ")" + trail
	default:
		text := c.inlineChildren(n)
		if blockTags[n.DataAtom] {
			// A block inside a link: keep its text apart from its
			// neighbours'.
			return " " + text + " "
		}
		return text
	}
}

// resolve makes a link absolute. It returns "" for one that is empty, does
// not parse, or has a scheme a reader cannot follow.
func (c *converter) resolve(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	u, err := c.base.Parse(ref)
	if err != nil || !linkSchemes[u.Scheme] {
		return ""
	}
	return u.String()
}

// wrap puts mark around the text, keeping the spaces at its ends outside.
func wrap(mark, text string) string {
	lead, core, trail := edges(strings.ReplaceAll(text, "\n", " "))
	if core == "" {
		return text
	}
	return lead + mark + core + mark + trail
}

// edges splits text into the spaces at its start, the rest, and the spaces
// at its end.
func edges(text string) (lead, core, trail string) {
	core = strings.TrimLeft(text, " ")
	lead = text[:len(text)-len(core)]
	trimmed := strings.TrimRight(core, " ")
	return lead, trimmed, core[len(trimmed):]
}

// collapse turns each run of white space, including a no-break space, into
// one space, as a browser shows text.
func collapse(text string) string {
	var b strings.Builder
	space := false
	for _, r := range text {
		switch r {
		case ' ', '\t', '\n', '\r', '\f', '\u00a0':
			space = true
		default:
			if space {
				b.WriteByte(' ')
				space = false
			}
			b.WriteRune(r)
		}
	}
	if space {
		b.WriteByte(' ')
	}
	return b.String()
}

// oneLine returns text on one line with single spaces.
func oneLine(text string) string {
	return strings.TrimSpace(collapse(text))
}

// paragraph cleans up inline Markdown: single spaces, no space at the ends
// of a line, no empty line at either end, and one empty line at most where
// line breaks were repeated.
func paragraph(inline string) string {
	var lines []string
	for line := range strings.SplitSeq(inline, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" && (len(lines) == 0 || lines[len(lines)-1] == "") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// prefixLines puts first before the first line of a block and rest before
// each later line that is not empty.
func prefixLines(block, first, rest string) string {
	if block == "" {
		return ""
	}
	var b strings.Builder
	for i, line := range strings.Split(block, "\n") {
		switch {
		case i == 0:
			b.WriteString(first + line)
		case line == "":
			b.WriteString("\n" + strings.TrimRight(rest, " "))
		default:
			b.WriteString("\n" + rest + line)
		}
	}
	return b.String()
}
