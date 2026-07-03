package eval

import (
	"strings"

	"golang.org/x/net/html"
)

// findAll returns every node satisfying matchFn (depth-first).
func findAll(n *html.Node, matchFn func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n == nil {
			return
		}
		if matchFn(n) {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

// findFirst returns the first node satisfying matchFn, or nil.
func findFirst(n *html.Node, matchFn func(*html.Node) bool) *html.Node {
	if n == nil {
		return nil
	}
	if matchFn(n) {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if r := findFirst(c, matchFn); r != nil {
			return r
		}
	}
	return nil
}

// elementsByTag finds all elements matching a tag name.
func elementsByTag(n *html.Node, tag string) []*html.Node {
	tag = strings.ToLower(tag)
	return findAll(n, func(x *html.Node) bool {
		return x.Type == html.ElementNode && x.Data == tag
	})
}

// firstElementByTag finds the first element matching a tag name.
func firstElementByTag(n *html.Node, tag string) *html.Node {
	tag = strings.ToLower(tag)
	return findFirst(n, func(x *html.Node) bool {
		return x.Type == html.ElementNode && x.Data == tag
	})
}

// attr returns the value of an attribute on an element node, or "".
func attr(n *html.Node, name string) string {
	if n == nil {
		return ""
	}
	name = strings.ToLower(name)
	for _, a := range n.Attr {
		if strings.ToLower(a.Key) == name {
			return a.Val
		}
	}
	return ""
}

// hasAttr returns true if the attribute is present (regardless of value).
func hasAttr(n *html.Node, name string) bool {
	if n == nil {
		return false
	}
	name = strings.ToLower(name)
	for _, a := range n.Attr {
		if strings.ToLower(a.Key) == name {
			return true
		}
	}
	return false
}

// textContent returns concatenated text of all descendants.
func textContent(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n == nil {
			return
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}

// findMetaContent looks up <meta name="x" content="y"> or property="x" or
// http-equiv="x" on the meta element.
func findMetaContent(doc *html.Node, name string) string {
	want := strings.ToLower(name)
	for _, m := range elementsByTag(doc, "meta") {
		for _, key := range []string{"name", "property", "http-equiv"} {
			if strings.ToLower(attr(m, key)) == want {
				return attr(m, "content")
			}
		}
	}
	return ""
}

// findLinkHref finds <link rel="X" href="Y">.
func findLinkHref(doc *html.Node, rel string) string {
	want := strings.ToLower(rel)
	for _, l := range elementsByTag(doc, "link") {
		if strings.Contains(strings.ToLower(attr(l, "rel")), want) {
			return attr(l, "href")
		}
	}
	return ""
}
