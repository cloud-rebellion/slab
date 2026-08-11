package eval

import (
	"fmt"
	"strings"

	"golang.org/x/net/html"
)

// RunAccessibilityChecks evaluates ARIA landmarks, alt text, form labels,
// heading order, focus indicators. Skips contrast (needs computed styles).
func RunAccessibilityChecks(site *SiteContext) []CheckResult {
	var checks []CheckResult

	checks = append(checks, perPageCheck("Main Landmark", "landmarks", 2, site, func(p PageContext) (bool, string) {
		if hasLandmark(p.Doc, "main") {
			return true, ""
		}
		return false, "no <main> or role=main"
	}, "Wrap page content in <main>."))

	checks = append(checks, perPageCheck("Navigation Landmark", "landmarks", 2, site, func(p PageContext) (bool, string) {
		if hasLandmark(p.Doc, "nav") {
			return true, ""
		}
		return false, "no <nav> or role=navigation"
	}, "Wrap navigation in <nav>."))

	checks = append(checks, perPageCheck("Banner Landmark", "landmarks", 1, site, func(p PageContext) (bool, string) {
		if hasLandmark(p.Doc, "header") {
			return true, ""
		}
		return false, "no <header> or role=banner"
	}, "Wrap site header in <header>."))

	checks = append(checks, perPageCheck("Contentinfo Landmark", "landmarks", 1, site, func(p PageContext) (bool, string) {
		if hasLandmark(p.Doc, "footer") {
			return true, ""
		}
		return false, "no <footer> or role=contentinfo"
	}, "Wrap site footer in <footer>."))

	checks = append(checks, perPageCheck("Page Language", "structure", 2, site, func(p PageContext) (bool, string) {
		htmlNode := firstElementByTag(p.Doc, "html")
		if htmlNode == nil || attr(htmlNode, "lang") == "" {
			return false, "no lang attribute on <html>"
		}
		return true, ""
	}, "Set lang= on the <html> element."))

	checks = append(checks, perPageCheck("Page Has H1", "structure", 2, site, func(p PageContext) (bool, string) {
		if len(elementsByTag(p.Doc, "h1")) == 0 {
			return false, "no <h1>"
		}
		return true, ""
	}, "Add exactly one <h1>."))

	checks = append(checks, perPageCheck("Heading Order", "structure", 2, site, func(p PageContext) (bool, string) {
		ok, msg := validateHeadingOrder(p.Doc)
		if !ok {
			return false, msg
		}
		return true, ""
	}, "Don't skip heading levels."))

	checks = append(checks, perPageCheck("No Empty Headings", "structure", 1, site, func(p PageContext) (bool, string) {
		for _, level := range []string{"h1", "h2", "h3", "h4", "h5", "h6"} {
			for _, h := range elementsByTag(p.Doc, level) {
				if textContent(h) == "" {
					return false, fmt.Sprintf("empty <%s>", level)
				}
			}
		}
		return true, ""
	}, "All headings must contain visible text."))

	checks = append(checks, perPageCheck("Images Have Alt Text", "media", 3, site, func(p PageContext) (bool, string) {
		for _, img := range elementsByTag(p.Doc, "img") {
			if !hasAttr(img, "alt") {
				return false, "image missing alt attribute"
			}
		}
		return true, ""
	}, "Every <img> needs alt= (use alt=\"\" for decorative)."))

	checks = append(checks, perPageCheck("Form Inputs Have Labels", "forms", 3, site, func(p PageContext) (bool, string) {
		labelFor := map[string]bool{}
		// An IMPLICIT label (<label>Email <input></label>) is valid HTML and a
		// valid accessible name; rejecting it failed correct markup at weight
		// 3. The label has to carry text of its own, otherwise it names
		// nothing.
		wrapped := map[*html.Node]bool{}
		for _, l := range elementsByTag(p.Doc, "label") {
			if v := attr(l, "for"); v != "" {
				labelFor[v] = true
			}
			if textContent(l) == "" {
				continue
			}
			for _, f := range findAll(l, isFormField) {
				wrapped[f] = true
			}
		}
		// textarea and select need a label exactly as much as input does and
		// were never examined at all.
		for _, in := range findAll(p.Doc, isFormField) {
			t := strings.ToLower(attr(in, "type"))
			if in.Data == "input" && (t == "hidden" || t == "submit" || t == "button" || t == "reset" || t == "image") {
				continue
			}
			if attr(in, "aria-label") != "" || attr(in, "aria-labelledby") != "" {
				continue
			}
			if id := attr(in, "id"); id != "" && labelFor[id] {
				continue
			}
			if wrapped[in] {
				continue
			}
			return false, fmt.Sprintf("<%s%s> without associated <label> or aria-label", in.Data, typeSuffix(t))
		}
		return true, ""
	}, "Associate every input, textarea and select with a <label for=id>, a wrapping <label>, or an aria-label."))

	checks = append(checks, perPageCheck("Buttons Have Labels", "forms", 2, site, func(p PageContext) (bool, string) {
		for _, b := range elementsByTag(p.Doc, "button") {
			if textContent(b) != "" || attr(b, "aria-label") != "" || attr(b, "title") != "" {
				continue
			}
			return false, "button without text or aria-label"
		}
		return true, ""
	}, "Buttons need accessible text (visible content or aria-label)."))

	checks = append(checks, perPageCheck("Links Have Labels", "forms", 2, site, func(p PageContext) (bool, string) {
		for _, a := range elementsByTag(p.Doc, "a") {
			if textContent(a) != "" || attr(a, "aria-label") != "" || attr(a, "title") != "" {
				continue
			}
			// Allow links that wrap an <img alt="...">
			imgs := elementsByTag(a, "img")
			hasAltImg := false
			for _, im := range imgs {
				if attr(im, "alt") != "" {
					hasAltImg = true
					break
				}
			}
			if hasAltImg {
				continue
			}
			return false, "link without accessible name"
		}
		return true, ""
	}, "Links need visible text, aria-label, or alt-bearing images."))

	checks = append(checks, perPageCheck("No Positive Tabindex", "focus", 2, site, func(p PageContext) (bool, string) {
		for _, n := range findAll(p.Doc, func(n *html.Node) bool {
			return n.Type == html.ElementNode && hasAttr(n, "tabindex")
		}) {
			tv := strings.TrimSpace(attr(n, "tabindex"))
			if tv != "" && tv != "0" && tv != "-1" {
				return false, "element with tabindex=\"" + tv + "\""
			}
		}
		return true, ""
	}, "Avoid tabindex > 0; let DOM order define tab sequence."))

	checks = append(checks, perPageCheck("Tables Have Headers", "structure", 2, site, func(p PageContext) (bool, string) {
		for _, t := range elementsByTag(p.Doc, "table") {
			if len(elementsByTag(t, "th")) == 0 {
				return false, "<table> with no <th>"
			}
		}
		return true, ""
	}, "Use <th> in tables for accessible header cells."))

	// --- New checks ported from upgraded site-inspector (2026-04-27) ---

	checks = append(checks, perPageCheck("Skip Navigation Link", "focus", 1, site, func(p PageContext) (bool, string) {
		// Site Inspector wants the FIRST focusable anchor to jump to a content
		// landmark. Match that: walk anchors in DOM order, look at the first
		// one with an in-page href starting with #.
		for _, a := range elementsByTag(p.Doc, "a") {
			href := strings.TrimSpace(attr(a, "href"))
			if !strings.HasPrefix(href, "#") {
				continue
			}
			target := strings.ToLower(strings.TrimPrefix(href, "#"))
			if target == "main" || target == "content" || target == "main-content" || target == "skip" {
				return true, ""
			}
			// First in-page anchor isn't a skip link — fail.
			return false, fmt.Sprintf("first in-page anchor jumps to #%s, not a content landmark", target)
		}
		return false, "no skip-to-content link found"
	}, "Add a visually-hidden <a href=\"#main\">Skip to content</a> as the first focusable element of the page."))

	checks = append(checks, perPageCheck("Autocomplete Attributes", "forms", 1, site, func(p PageContext) (bool, string) {
		need := map[string]bool{"email": true, "tel": true, "url": true}
		nameHints := map[string]bool{
			"name": true, "fname": true, "lname": true, "first_name": true, "last_name": true,
			"organization": true, "organization-title": true, "country": true, "postal-code": true,
		}
		for _, in := range elementsByTag(p.Doc, "input") {
			t := strings.ToLower(attr(in, "type"))
			n := strings.ToLower(attr(in, "name"))
			if !need[t] && !nameHints[n] {
				continue
			}
			if attr(in, "autocomplete") == "" {
				return false, fmt.Sprintf("<input type=%q name=%q> has no autocomplete attribute", t, n)
			}
		}
		return true, ""
	}, "Add autocomplete=\"email|tel|name|organization|...\" so password managers and assistive tech can fill in correct values."))

	checks = append(checks, siteWideCheck("Focus Indicators", "focus", site, func() CheckResult {
		// Site-wide: at least one inline <style> or linked .css file must
		// declare a :focus / :focus-visible rule that actually PAINTS
		// something. Default visible focus indicators are essential for
		// keyboard users.
		for _, p := range site.Pages {
			for _, st := range elementsByTag(p.Doc, "style") {
				if hasFocusIndicator(textContent(st)) {
					return Pass("Focus Indicators", "focus", 2, "Found a :focus or :focus-visible rule with a visible indicator in inline CSS")
				}
			}
			for _, link := range elementsByTag(p.Doc, "link") {
				if strings.EqualFold(attr(link, "rel"), "stylesheet") {
					href := attr(link, "href")
					if href == "" || strings.HasPrefix(href, "http") {
						continue
					}
					// Read the linked CSS from dist/ relative to the page.
					if css := readDistCSS(site.DistDir, href); hasFocusIndicator(css) {
						return Pass("Focus Indicators", "focus", 2, "Found a :focus or :focus-visible rule with a visible indicator in a linked stylesheet")
					}
				}
			}
		}
		return Fail("Focus Indicators", "focus", 2, SeverityWarning,
			"no :focus or :focus-visible rule that paints a visible indicator was found in the built CSS",
			"Keep visible focus styles in your CSS (avoid outline:none without a replacement). Tailwind ships them via focus-visible utilities.")
	}))

	checks = append(checks, perPageCheck("No Focusable in aria-hidden", "focus", 2, site, func(p PageContext) (bool, string) {
		for _, hidden := range findAll(p.Doc, func(n *html.Node) bool {
			return n.Type == html.ElementNode && strings.EqualFold(attr(n, "aria-hidden"), "true")
		}) {
			focusables := findAll(hidden, func(n *html.Node) bool {
				if n.Type != html.ElementNode {
					return false
				}
				switch n.Data {
				case "a", "button", "input", "select", "textarea":
					return true
				}
				if hasAttr(n, "tabindex") && strings.TrimSpace(attr(n, "tabindex")) != "-1" {
					return true
				}
				return false
			})
			if len(focusables) > 0 {
				return false, fmt.Sprintf("element with aria-hidden=\"true\" contains %d focusable descendant(s)", len(focusables))
			}
		}
		return true, ""
	}, "Don't put focusable elements inside aria-hidden=\"true\". Either move them out, or set tabindex=-1 to take them out of the tab order."))

	return checks
}

// isFormField matches the three labelable form controls.
func isFormField(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	switch n.Data {
	case "input", "textarea", "select":
		return true
	}
	return false
}

func typeSuffix(t string) string {
	if t == "" {
		return ""
	}
	return " type=\"" + t + "\""
}

// focusIndicatorProps are the declarations that can actually paint a focus
// ring. outline-offset alone moves a ring that may not exist, so it is not on
// the list.
var focusIndicatorProps = map[string]bool{
	"outline": true, "outline-color": true, "outline-width": true, "outline-style": true,
	"box-shadow": true, "border": true, "border-color": true, "border-width": true,
	"border-bottom": true, "border-block-end": true,
	"background": true, "background-color": true, "color": true,
	"text-decoration": true, "text-decoration-color": true, "filter": true,
}

// hasFocusIndicator reports whether the CSS source declares a :focus or
// :focus-visible rule that paints a visible indicator.
//
// The old implementation was a substring match for ":focus" over the RAW PAGE
// HTML, so it passed on `:focus { outline: none }` (the exact anti-pattern the
// check exists to catch) and on a Tailwind class string like
// "md:focus:ring-2", which is an HTML attribute value and not CSS at all.
// Callers now pass stylesheet sources only.
func hasFocusIndicator(css string) bool {
	found := false
	forEachCSSRule(css, 0, func(selector, body string) {
		if found || !strings.Contains(strings.ToLower(selector), ":focus") {
			return
		}
		if declarationsPaintFocus(body) {
			found = true
		}
	})
	return found
}

// declarationsPaintFocus reports whether a rule body sets at least one
// indicator property to something other than "none" / zero.
func declarationsPaintFocus(body string) bool {
	for _, decl := range strings.Split(body, ";") {
		name, value, ok := strings.Cut(decl, ":")
		if !ok {
			continue
		}
		prop := strings.ToLower(strings.TrimSpace(name))
		val := strings.ToLower(strings.TrimSpace(value))
		val = strings.TrimSpace(strings.TrimSuffix(val, "!important"))
		if val == "" {
			continue
		}
		if strings.HasPrefix(prop, "--tw-ring") {
			// Tailwind's ring utilities set custom properties that feed a
			// box-shadow; a non-zero width is a real indicator.
			if val != "0" && val != "0px" && val != "none" {
				return true
			}
			continue
		}
		if !focusIndicatorProps[prop] {
			continue
		}
		switch val {
		case "none", "0", "0px", "unset", "initial", "revert", "transparent":
			continue
		}
		return true
	}
	return false
}

// maxCSSNesting bounds recursion into @media / @supports / @layer blocks.
const maxCSSNesting = 8

// forEachCSSRule walks top-level rules, descending into at-rule blocks so a
// focus rule inside @media still counts. Brace matching only; CSS comments
// carrying unbalanced braces are not a shape any minifier emits.
func forEachCSSRule(css string, depth int, fn func(selector, body string)) {
	if depth > maxCSSNesting {
		return
	}
	i := 0
	for i < len(css) {
		open := strings.IndexByte(css[i:], '{')
		if open < 0 {
			return
		}
		selector := strings.TrimSpace(css[i : i+open])
		start := i + open + 1
		nest := 1
		j := start
		for j < len(css) && nest > 0 {
			switch css[j] {
			case '{':
				nest++
			case '}':
				nest--
			}
			j++
		}
		end := j
		if nest == 0 {
			end = j - 1
		}
		body := css[start:end]
		if strings.HasPrefix(selector, "@") {
			forEachCSSRule(body, depth+1, fn)
		} else {
			fn(selector, body)
		}
		i = j
	}
}

// readDistCSS resolves a stylesheet href relative to the dist root and reads
// it. Returns "" on any error so the caller falls through to inline checks.
func readDistCSS(distDir, href string) string {
	if distDir == "" || href == "" {
		return ""
	}
	cleanHref := strings.TrimPrefix(href, "/")
	return readFileSafe(distDir + "/" + cleanHref)
}

func hasLandmark(doc *html.Node, tag string) bool {
	// For <header>/<footer>, only count those NOT nested inside <article>,
	// <section>, <aside>, or <nav>. Per HTML5 spec, only top-level header/footer
	// are banner/contentinfo landmarks.
	scopedTags := map[string]bool{"header": true, "footer": true}
	if scopedTags[tag] {
		var found bool
		var walk func(*html.Node, bool)
		walk = func(n *html.Node, insideSectioning bool) {
			if found || n == nil {
				return
			}
			if n.Type == html.ElementNode {
				switch n.Data {
				case "article", "section", "aside", "nav":
					insideSectioning = true
				}
				if n.Data == tag && !insideSectioning {
					found = true
					return
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c, insideSectioning)
			}
		}
		walk(doc, false)
		if found {
			return true
		}
	} else {
		if firstElementByTag(doc, tag) != nil {
			return true
		}
	}
	roleMap := map[string]string{
		"main":   "main",
		"nav":    "navigation",
		"header": "banner",
		"footer": "contentinfo",
	}
	role := roleMap[tag]
	if role == "" {
		return false
	}
	return findFirst(doc, func(n *html.Node) bool {
		return n.Type == html.ElementNode && strings.EqualFold(attr(n, "role"), role)
	}) != nil
}
