package webfetch

import (
	"net/url"
	"strings"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/strikethrough"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
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

// linkSchemes are the schemes a link or an image is kept with. Any other,
// such as javascript: or data:, leaves only the text.
var linkSchemes = map[string]bool{"http": true, "https": true, "mailto": true, "tel": true, "ftp": true}

// markdown writes the Markdown. A cell that holds a line break stays in its
// table, and columns are not padded, which would only spend the result limit.
var markdown = converter.NewConverter(converter.WithPlugins(
	base.NewBasePlugin(),
	commonmark.NewCommonmarkPlugin(
		commonmark.WithListEndComment(false),
		commonmark.WithLinkEmptyHrefBehavior(commonmark.LinkBehaviorSkip),
		commonmark.WithLinkEmptyContentBehavior(commonmark.LinkBehaviorSkip),
	),
	table.NewTablePlugin(
		table.WithNewlineBehavior(table.NewlineBehaviorPreserve),
		table.WithCellPaddingBehavior(table.CellPaddingBehaviorMinimal),
	),
	strikethrough.NewStrikethroughPlugin(),
))

// toMarkdown converts an HTML document to Markdown and returns its title.
// Before the conversion it takes out what a reader would not see, and makes
// links and images absolute against base, or against the document's base
// element when it has one.
func toMarkdown(doc string, base *url.URL) (text, title string, err error) {
	root, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		return "", "", err
	}
	linkBase := base
	var remove []*html.Node
	for n := range root.Descendants() {
		if n.Type != html.ElementNode {
			continue
		}
		switch n.DataAtom {
		case atom.Title:
			if title == "" && n.Namespace == "" && n.FirstChild != nil {
				title = strings.Join(strings.Fields(n.FirstChild.Data), " ")
			}
		case atom.Base:
			if u, err := base.Parse(attr(n, "href")); err == nil && linkBase == base && attr(n, "href") != "" {
				linkBase = u
			}
		}
		if hidden(n) {
			remove = append(remove, n)
		}
	}
	for _, n := range remove {
		// An element inside one already removed has no parent to leave.
		if n.Parent != nil {
			n.Parent.RemoveChild(n)
		}
	}
	var unlinked []*html.Node
	for n := range root.Descendants() {
		switch {
		case n.Type != html.ElementNode:
		case n.DataAtom == atom.A:
			// A link to a place in the same page leaves its text.
			if strings.HasPrefix(strings.TrimSpace(attr(n, "href")), "#") {
				setAttr(n, "href", "")
			} else {
				setAttr(n, "href", resolve(linkBase, attr(n, "href")))
			}
		case n.DataAtom == atom.Img:
			src := resolve(linkBase, attr(n, "src"))
			setAttr(n, "src", src)
			if src == "" {
				unlinked = append(unlinked, n)
			}
		}
	}
	for _, n := range unlinked {
		// An image with nowhere to point leaves its description.
		n.Parent.InsertBefore(&html.Node{Type: html.TextNode, Data: attr(n, "alt")}, n)
		n.Parent.RemoveChild(n)
	}
	out, err := markdown.ConvertNode(root)
	if err != nil {
		return "", "", err
	}
	if len(out) > 0 {
		out = append(out, '\n')
	}
	return string(out), title, nil
}

func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name && a.Namespace == "" {
			return a.Val
		}
	}
	return ""
}

// setAttr sets an attribute the element has. One it lacks stays absent.
func setAttr(n *html.Node, name, value string) {
	for i, a := range n.Attr {
		if a.Key == name && a.Namespace == "" {
			n.Attr[i].Val = value
		}
	}
}

// hidden says the element and everything in it is left out.
func hidden(n *html.Node) bool {
	if dropped[n.DataAtom] || attr(n, "aria-hidden") == "true" {
		return true
	}
	for _, a := range n.Attr {
		if a.Key == "hidden" && a.Namespace == "" {
			return true
		}
	}
	style := strings.ReplaceAll(strings.ToLower(attr(n, "style")), " ", "")
	return strings.Contains(style, "display:none")
}

// resolve makes a link absolute. It returns "" for one that is empty, does
// not parse, or has a scheme a reader cannot follow.
func resolve(base *url.URL, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	u, err := base.Parse(ref)
	if err != nil || !linkSchemes[u.Scheme] {
		return ""
	}
	return u.String()
}
