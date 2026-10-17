// Package htmltext turns an HTML document into plain text lines, one line per
// block element, which is the input format of the structure parsers.
package htmltext

import (
	"bytes"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "li": true, "tr": true, "h1": true, "h2": true,
	"h3": true, "h4": true, "h5": true, "h6": true, "section": true, "article": true,
	"header": true, "footer": true, "table": true, "blockquote": true, "pre": true,
	"ul": true, "ol": true, "dd": true, "dt": true, "td": true, "th": true,
}

var skipTags = map[string]bool{"script": true, "style": true, "head": true, "noscript": true}

// Lines extracts non-empty, whitespace-normalized text lines.
func Lines(doc []byte) ([]string, error) {
	d, err := goquery.NewDocumentFromReader(bytes.NewReader(doc))
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && skipTags[n.Data] {
			return
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		// Superscripts carry article numbers like "Art. 4¹"; keep them
		// distinguishable from plain digits ("Art. 41").
		if n.Type == html.ElementNode && n.Data == "sup" {
			b.WriteByte('^')
		}
		block := n.Type == html.ElementNode && blockTags[n.Data]
		if block {
			b.WriteByte('\n')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if block {
			b.WriteByte('\n')
		}
	}
	for _, n := range d.Nodes {
		walk(n)
	}
	return SplitLines(b.String()), nil
}

// SplitLines normalizes whitespace (including non-breaking spaces) and drops
// empty lines.
func SplitLines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		l = strings.Join(strings.Fields(strings.ReplaceAll(l, " ", " ")), " ")
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
