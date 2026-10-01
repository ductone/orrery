package webtools

import (
	"bytes"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Pages are mostly markup: a documentation page of a few thousand words can
// arrive as megabytes of scripts, styles, and navigation. Fetch returns the
// readable text instead, so a handful of fetches cannot fill a context window.

// skipped elements never contain reading text.
var skipped = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Template: true, atom.Svg: true,
	atom.Iframe: true, atom.Canvas: true, atom.Form: true, atom.Button: true, atom.Select: true,
	atom.Nav: true, atom.Header: true, atom.Footer: true, atom.Aside: true, atom.Head: true,
}

// blocks start a new line in the extracted text.
var blocks = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Section: true, atom.Article: true, atom.Main: true,
	atom.Br: true, atom.Hr: true, atom.Li: true, atom.Ul: true, atom.Ol: true, atom.Dl: true,
	atom.Dt: true, atom.Dd: true, atom.Table: true, atom.Tr: true, atom.Blockquote: true,
	atom.Pre: true, atom.Figure: true, atom.Figcaption: true,
	atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
}

var headingLevel = map[atom.Atom]int{atom.H1: 1, atom.H2: 2, atom.H3: 3, atom.H4: 4, atom.H5: 5, atom.H6: 6}

// htmlText extracts a page's title and readable text. It prefers the page's
// <main> or <article> when one holds most of the text, dropping site chrome.
func htmlText(b []byte) (title, text string) {
	doc, err := html.Parse(bytes.NewReader(b))
	if err != nil {
		return "", string(b)
	}
	if t := find(doc, atom.Title); t != nil {
		title = strings.TrimSpace(collapse(innerText(t)))
	}
	root := doc
	if body := find(doc, atom.Body); body != nil {
		root = body
	}
	whole := render(root)
	for _, a := range []atom.Atom{atom.Main, atom.Article} {
		if n := find(root, a); n != nil {
			if main := render(n); len(main) >= len(whole)/3 {
				return title, main
			}
		}
	}
	return title, whole
}

func find(n *html.Node, a atom.Atom) *html.Node {
	if n.Type == html.ElementNode && n.DataAtom == a {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := find(c, a); found != nil {
			return found
		}
	}
	return nil
}

func innerText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// render writes a node's readable text: headings marked with #, list items
// with -, preformatted text kept as is, and whitespace collapsed elsewhere.
func render(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node, bool)
	walk = func(n *html.Node, pre bool) {
		switch n.Type {
		case html.TextNode:
			if pre {
				b.WriteString(n.Data)
			} else if t := collapse(n.Data); t != "" {
				if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") && !strings.HasSuffix(b.String(), " ") {
					b.WriteByte(' ')
				}
				b.WriteString(t)
			}
			return
		case html.ElementNode:
			if skipped[n.DataAtom] || hidden(n) {
				return
			}
		}
		block := n.Type == html.ElementNode && blocks[n.DataAtom]
		if block {
			newline(&b)
			if level := headingLevel[n.DataAtom]; level > 0 {
				b.WriteString(strings.Repeat("#", level) + " ")
			}
			if n.DataAtom == atom.Li {
				b.WriteString("- ")
			}
		}
		inPre := pre || n.DataAtom == atom.Pre
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inPre)
		}
		if block {
			newline(&b)
		}
	}
	walk(n, false)
	return tidy(b.String())
}

func hidden(n *html.Node) bool {
	for _, a := range n.Attr {
		if a.Key == "hidden" || (a.Key == "aria-hidden" && a.Val == "true") {
			return true
		}
	}
	return false
}

func newline(b *strings.Builder) {
	if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
		b.WriteByte('\n')
	}
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// tidy trims lines and allows at most one blank line in a row.
func tidy(s string) string {
	var out []string
	blank := 0
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, " \t")
		if strings.TrimSpace(line) == "" {
			if blank++; blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
