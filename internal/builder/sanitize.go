package builder

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"

	"github.com/microcosm-cc/bluemonday"
)

// componentNameRE accepts a safe component identifier. comp.Name is used as a
// filename under src/components/, so it must never contain a path separator,
// "..", or anything that could traverse out of that directory.
var componentNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

// ValidComponentName reports whether name is safe to use as a component filename.
func ValidComponentName(name string) bool {
	return componentNameRE.MatchString(name)
}

// fontFamilyRE accepts a safe CSS font-family name. The value is interpolated
// into `--font-heading: '<name>', ...` so it must not contain quotes, braces or
// semicolons that could break out of the declaration and inject arbitrary CSS.
var fontFamilyRE = regexp.MustCompile(`^[A-Za-z0-9 _-]{1,64}$`)

// cssFontFamilyDecl returns a safe `'<name>', system-ui, sans-serif` stack for a
// site-supplied font name. An empty or unsafe name (CSS injection via the
// branding API or a figma import) falls back to the system stack rather than
// emitting attacker-controlled CSS.
// CSSFontFamilyDecl is the exported form, for packages that ingest third-party
// font names and must not build the declaration themselves. internal/figma used
// fmt.Sprintf("font-family: %q, ...") on a value straight out of a shared Figma
// file: %q does stop a break out of the CSS string, so it was hygiene rather
// than a live injection, but this helper's own doc comment names "a figma
// import" as the threat it exists for, and a second hand-rolled path is how the
// four divergent SSRF predicates happened.
func CSSFontFamilyDecl(name string) string {
	return cssFontFamilyDecl(name)
}

func cssFontFamilyDecl(name string) string {
	if fontFamilyRE.MatchString(name) {
		return "'" + name + "', system-ui, sans-serif"
	}
	return "system-ui, sans-serif"
}

// cssCommentSafe neutralizes a string before it is placed inside a `/* ... */`
// CSS comment, so a usage note (e.g. seeded from raw figma style names) cannot
// close the comment with `*/` and inject live CSS.
func cssCommentSafe(s string) string {
	s = strings.ReplaceAll(s, "*/", "* /")
	s = strings.ReplaceAll(s, "/*", "/ *")
	return s
}

// rawHTMLPolicy sanitizes agent-authored raw-HTML block content. The raw-HTML
// block is an intentional author feature, but agent-controlled markup must not
// be able to inject executable content into the built page or the editor
// preview: bluemonday's UGC policy keeps ordinary formatting/markup while
// stripping <script>, <style>, event handlers (on*), and javascript:/data:
// URLs. Sites that genuinely need a script use the allowed-scripts surface, not
// a raw-HTML block. The policy is safe for concurrent Sanitize calls.
var rawHTMLPolicy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowStyling()              // permit class attributes for layout
	p.AllowAttrs("id").Globally() // keep anchor targets (href="#...") working
	return p
}()

// sanitizeRawHTML strips any executable content from agent-authored HTML before
// it is emitted into the generated Astro page or the editor preview. This is the
// render-time choke point that neutralizes stored XSS regardless of which write
// path (REST, MCP) or pre-existing row produced the content.
func sanitizeRawHTML(s string) string {
	return rawHTMLPolicy.Sanitize(s)
}

// safeCustomPropRE accepts an inline style attribute made only of CSS custom
// properties, which is what the block library uses to seed per-instance
// animation timings (style="--del:0.7s;--dur:6.2s"). A colon is not in the
// value class, so no scheme (javascript:, data:) can appear in a value.
var safeCustomPropRE = regexp.MustCompile(`^(?:\s*--[A-Za-z0-9_-]+\s*:\s*[A-Za-z0-9 .,%#()/_-]+\s*;?)+\s*$`)

// fragmentRefRE accepts only a same-document reference. <use href> and
// <textPath href> resolve and fetch, so an absolute URL there is an SSRF and
// exfiltration sink; a fragment cannot leave the page.
var fragmentRefRE = regexp.MustCompile(`^#[A-Za-z0-9_:.-]+$`)

// svgElements is the presentational SVG surface the block library draws with
// (card walls, flocks, the guarantee stamp). <script>, <foreignObject>,
// <animate> and <set> are deliberately absent: the first two execute, the last
// two can drive an attribute at runtime.
var svgElements = []string{
	"svg", "g", "defs", "symbol", "use", "title", "desc",
	"path", "rect", "circle", "ellipse", "line", "polyline", "polygon",
	"text", "tspan", "textPath", "clipPath", "mask", "pattern",
	"linearGradient", "radialGradient", "stop",
}

// svgAttrs are geometry and paint attributes. No event handlers, no href
// (granted separately, fragment-only), no xlink:href at all.
var svgAttrs = []string{
	"viewBox", "xmlns", "xmlns:xlink", "preserveAspectRatio", "focusable",
	"d", "x", "y", "dx", "dy", "x1", "y1", "x2", "y2", "cx", "cy", "r", "rx", "ry",
	"width", "height", "points", "transform", "transform-origin", "gradientUnits",
	"gradientTransform", "patternUnits", "patternTransform", "offset", "vector-effect",
	"fill", "fill-opacity", "fill-rule", "stroke", "stroke-width", "stroke-opacity",
	"stroke-linecap", "stroke-linejoin", "stroke-dasharray", "stroke-dashoffset",
	"opacity", "stop-color", "stop-opacity", "clip-path", "clip-rule", "mask",
	"font-family", "font-size", "font-weight", "letter-spacing", "word-spacing",
	"text-anchor", "dominant-baseline", "startOffset", "shape-rendering",
}

// customBlockPolicy sanitizes the `custom` block's markup. That block is the
// authoring surface the whole block library is built on: every section ships as
// a <style> plus semantic markup, inline SVG for the decorative motifs, aria-*
// for the landmarks, and data-* for the nav state.
//
// rawHTMLPolicy is wrong for it. bluemonday's UGC policy is tuned for untrusted
// comment HTML, so it drops <style>, all SVG, every aria-*/data-* attribute,
// loading/decoding, and (because it drops the elements that carry them) most
// class attributes too. Pointing renderCustomBlock at it to close a stored-XSS
// hole silently stripped the CSS off every custom block on the next build: a
// site authored this way renders as unstyled text, and nothing in the build log
// says so. See the 2026-08-10 post-mortem.
//
// What still cannot get through, which is the part that mattered: <script>,
// <iframe>, <object>, <embed>, on* handlers, and javascript:/data: URLs.
var customBlockPolicy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowStyling()
	p.AllowDataAttributes()
	p.AllowAttrs("id", "class", "role").Globally()
	p.AllowAttrs("style").Matching(safeCustomPropRE).Globally()
	p.AllowAttrs(
		"aria-hidden", "aria-label", "aria-labelledby", "aria-describedby",
		"aria-current", "aria-expanded", "aria-controls", "aria-live", "aria-atomic",
	).Globally()
	p.AllowElements(
		"section", "header", "footer", "nav", "article", "aside", "main",
		"figure", "figcaption", "picture", "details", "summary",
		"time", "mark", "small", "hr", "span", "div",
	)
	p.AllowAttrs("loading", "decoding", "width", "height", "sizes", "srcset").OnElements("img")
	p.AllowAttrs("media", "srcset", "type").OnElements("source")
	p.AllowAttrs("datetime").OnElements("time")
	p.AllowAttrs("open").OnElements("details")
	p.AllowElements(svgElements...)
	p.AllowAttrs(svgAttrs...).OnElements(svgElements...)
	p.AllowAttrs("href").Matching(fragmentRefRE).OnElements("use", "textPath")
	return p
}()

// styleBlockRE matches a whole <style> element, including its CSS text.
var styleBlockRE = regexp.MustCompile(`(?is)<style\b[^>]*>(.*?)</style\s*>`)

// cssUnsafeRE removes the CSS constructs that can close the element, reopen
// markup, fetch, or execute. bluemonday never sees the CSS text (it deletes
// <style> outright), so this is the whole filter for that content.
var cssUnsafeRE = regexp.MustCompile(`(?is)</\s*style|<!--|-->|<\s*script|@import|@charset|expression\s*\(|behaviou?r\s*:|-moz-binding|javascript\s*:|vbscript\s*:`)

func sanitizeBlockCSS(css string) string {
	return cssUnsafeRE.ReplaceAllString(css, "")
}

// styleSlotPrefix/Suffix bracket the placeholder that stands in for a <style>
// element while the surrounding markup goes through bluemonday. Letters and
// digits only, so the sanitizer passes it through as ordinary text and no
// entity encoding can break the round trip.
//
// The middle of the placeholder is a per-call random nonce. It used to be a
// fixed literal, and CSS text never passes through bluemonday, so an author
// could write a valid placeholder inside style block 0: splice 1 then replaced
// the FIRST occurrence of its token, which by then sat inside the CSS spliced
// for block 0. That closed the outer <style> early and handed the parser back
// to markup context, so `<img src=x onerror=...>` became a live element node.
// A nonce the author cannot predict makes the token unforgeable, and the splice
// below is a single left-to-right pass that never rescans what it inserted.
const (
	styleSlotPrefix = "slabstyleslot"
	styleSlotSuffix = "endslabstyleslot"
	styleSlotNonceN = 12 // bytes, hex-encoded into the placeholder
)

// styleSlotRE matches a placeholder of the shape
// slabstyleslot<nonce>s<index>endslabstyleslot. The nonce is checked against
// the one this call generated, so a token an author planted with some other
// nonce is left alone as the inert text it is.
var styleSlotRE = regexp.MustCompile(`slabstyleslot([0-9a-f]{` + strconv.Itoa(styleSlotNonceN*2) + `})s([0-9]{1,6})endslabstyleslot`)

// styleSlotRejected replaces the whole block when the CSS splice cannot be
// completed safely. Dropping the block is the fail-closed answer: the only way
// to reach it is a forged placeholder, and splicing anyway is exactly the
// sanitizer bypass this guard exists for.
const styleSlotRejected = "<!-- custom block: style placeholder rejected, content removed -->"

// sanitizeCustomBlockForAstro sanitizes custom-block markup while preserving the
// author's CSS, and neutralises Astro's build-time expression syntax in the
// markup. <style> is lifted out first because bluemonday deletes the element
// and its text no matter how the policy is configured, so the CSS gets its own
// filter (sanitizeBlockCSS) and is spliced back at the same position.
//
// Order matters. The brace neutralisation runs on the sanitized markup BEFORE
// the CSS is spliced back in, because the two contexts want opposite things:
// this output lands in generated .astro source, where `{...}` in markup is
// JavaScript that Astro evaluates in Node at build time, while `<style>`
// content is raw text to the Astro compiler and its CSS braces must stay
// literal. Neutralising after the splice would entity-encode every CSS rule and
// publish the site unstyled.
func sanitizeCustomBlockForAstro(s string) string {
	nonceBytes := make([]byte, styleSlotNonceN)
	if _, err := rand.Read(nonceBytes); err != nil {
		return styleSlotRejected
	}
	nonce := hex.EncodeToString(nonceBytes)

	var css []string
	withSlots := styleBlockRE.ReplaceAllStringFunc(s, func(m string) string {
		inner := styleBlockRE.FindStringSubmatch(m)[1]
		css = append(css, sanitizeBlockCSS(inner))
		return styleSlotPrefix + nonce + "s" + strconv.Itoa(len(css)-1) + styleSlotSuffix
	})

	// Belt to the nonce's braces: if a live token somehow appears inside CSS
	// text, refuse rather than splice CSS into CSS.
	for _, c := range css {
		if strings.Contains(c, styleSlotPrefix+nonce) {
			return styleSlotRejected
		}
	}

	out := svgCaseRestorer.Replace(customBlockPolicy.Sanitize(withSlots))
	out = neutraliseAstroExpressions(out)

	rejected := false
	out = styleSlotRE.ReplaceAllStringFunc(out, func(tok string) string {
		m := styleSlotRE.FindStringSubmatch(tok)
		if m[1] != nonce {
			return tok
		}
		i, err := strconv.Atoi(m[2])
		if err != nil || i < 0 || i >= len(css) {
			rejected = true
			return ""
		}
		return "<style>" + css[i] + "</style>"
	})
	if rejected {
		return styleSlotRejected
	}
	return out
}

// svgCaseRestorer puts the camelCase back into SVG names that bluemonday's
// HTML tokenizer lowercases. The HTML parser has a foreign-content fixup table
// that maps `viewbox` back to `viewBox`, so browsers do cope, but the value
// then depends on that table rather than on what the author wrote: the same
// markup copied into a standalone .svg, or read back through the agent API,
// has a dead viewBox and an unscaled graphic. Restoring it keeps the output
// identical to the input instead of merely equivalent in one parsing mode.
var svgCaseRestorer = strings.NewReplacer(
	"<clippath", "<clipPath", "</clippath", "</clipPath",
	"<textpath", "<textPath", "</textpath", "</textPath",
	"<lineargradient", "<linearGradient", "</lineargradient", "</linearGradient",
	"<radialgradient", "<radialGradient", "</radialgradient", "</radialGradient",
	" viewbox=", " viewBox=",
	" preserveaspectratio=", " preserveAspectRatio=",
	" gradientunits=", " gradientUnits=",
	" gradienttransform=", " gradientTransform=",
	" patternunits=", " patternUnits=",
	" patterntransform=", " patternTransform=",
	" clippathunits=", " clipPathUnits=",
	" maskunits=", " maskUnits=",
	" startoffset=", " startOffset=",
	" spreadmethod=", " spreadMethod=",
	" shape-rendering=", " shape-rendering=",
)

// astroExpressionReplacer neutralises Astro's build-time expression syntax.
//
// Block output is written into a generated .astro file, so `{...}` there is a
// JavaScript expression Astro evaluates in Node during the build, with access
// to the build process environment and to the filesystem. That makes it a code
// sink, not a markup sink, and sanitizing HTML alone does not close it:
// bluemonday has no reason to strip braces from text. The allowlist that bounds
// the build environment does not help either: it bounds what process.env holds,
// not what the expression may do, and
// `{(await import('node:fs')).readFileSync('/app/data/atomicsite.db')}` reads
// the multi-tenant database onto the attacker's own public page.
//
// Entities render as literal braces in the finished page and mean nothing to
// the Astro compiler, so tenant text still displays as written.
var astroExpressionReplacer = strings.NewReplacer("{", "&#123;", "}", "&#125;")

func neutraliseAstroExpressions(s string) string {
	return astroExpressionReplacer.Replace(s)
}

// sanitizeForAstro is the one choke point for tenant markup that is emitted
// verbatim into generated .astro source. Every raw-markup sink routes through
// it (or through sanitizeCustomBlockForAstro, the authoring-surface variant
// that also preserves <style>, SVG and aria-*), so a sink cannot get the
// markup half of the defence and miss the code half. Three of the four sinks
// had exactly that shape: the fix landed on raw_astro, the only admin-only one,
// and the three a plain site member can reach were missed.
func sanitizeForAstro(s string) string {
	return neutraliseAstroExpressions(sanitizeRawHTML(s))
}

// ---------------------------------------------------------------------------
// The Astro escaping engine.
//
// Everything this package writes lands in a generated .astro file, and .astro
// is not one output language. It is four, and the escape that makes one safe
// breaks another. Measured against a real Astro 5 build rather than reasoned
// about, because the two halves disagree in a way that is easy to get backwards:
//
//	context                    braces evaluated?   entity decoded on render?
//	markup text                YES  <- the sink    yes
//	quoted HTML attribute      no                  yes
//	quoted component prop      no                  yes
//	<style> body               no                  NO
//	<script> body              no                  NO
//
// So brace neutralisation is required in markup text, harmless in attributes,
// and actively wrong inside <script> or <style>, where an entity is not decoded
// by anything and `&#123;` stays seven literal characters in the JavaScript.
// That is the whole reason a blanket pass over rendered output cannot work, and
// why every tenant string takes a NAMED exit for the context it lands in.
//
// Exit 1: escapeTextForAstro       markup text
// Exit 2: escapeAttrForAstro       quoted HTML attribute
// Exit 3: escapeJSStringForAstro   inside a <script> the renderer writes
//         escapeCSSStringForAstro  inside a <style> the renderer writes
// Exit 4: escapeAstroPropString    a quoted prop on an Astro component
//
// A fifth context exists and is deliberately not an exit: JSON-LD, which is
// produced by json.Marshal. encoding/json escapes < > and & to \uXXXX by
// default, so it is already a correct raw-text escape, and routing it through
// exit 1 or 2 would corrupt the JSON.
// ---------------------------------------------------------------------------

// escapeTextForAstro is exit 1: tenant text in markup context. This is the one
// exit where the neutralisation is load-bearing rather than defensive, because
// markup text is the only measured context in which Astro evaluates `{...}`.
// escapeHTML replaces < > and & only, which leaves exactly the braces Astro
// runs, so the two steps belong together behind one name.
func escapeTextForAstro(s string) string {
	return neutraliseAstroExpressions(escapeHTML(s))
}

// escapeAttrForAstro is exit 2: tenant text in a quoted HTML attribute.
//
// The neutralisation here is defence in depth, not a live fix: a quoted
// attribute value is inert to the Astro compiler today, measured. It is applied
// anyway because the difference between an attribute and markup text is one
// refactor wide, the rendered result is identical either way (the browser
// decodes the entity), and the alternative is a rule that says "braces are
// dangerous except in the places somebody checked once".
func escapeAttrForAstro(s string) string {
	return neutraliseAstroExpressions(escapeAttr(s))
}

// jsStringReplacer escapes a value for use inside a single- or double-quoted
// JavaScript string literal that sits inside a <script> element.
//
// Braces are deliberately absent. Astro does not evaluate them inside a script
// body, and an HTML entity is not decoded there by anything, so neutralising
// would replace working JavaScript with the seven characters `&#123;`.
//
// `<` and `>` become < and > because the HTML parser, not the JS
// parser, decides where a script element ends: a literal `</script` inside a
// string closes the element and everything after it is markup again. That is a
// live hole today wherever a tenant value reaches a script body through %q,
// which quotes for JavaScript and does nothing about HTML.
// jsBackslash is one backslash, kept as its own constant so the escapes below
// read as the data they are and cannot be mangled by anything that rewrites Go
// escape sequences.
const jsBackslash = `\`

var jsStringReplacer = strings.NewReplacer(
	`\`, `\\`,
	`'`, `\'`,
	`"`, `\"`,
	"`", "\\`",
	"\n", `\n`,
	"\r", `\r`,
	// JS line terminators: legal inside a JSON string, fatal inside a JS one.
	string(rune(0x2028)), jsBackslash+"u2028",
	string(rune(0x2029)), jsBackslash+"u2029",
	"<", jsBackslash+"u003c",
	">", jsBackslash+"u003e",
)

// escapeJSStringForAstro is exit 3, script half. It returns the BODY of a JS
// string literal, without the surrounding quotes, so a call site keeps its own
// quoting style visible.
func escapeJSStringForAstro(s string) string {
	return jsStringReplacer.Replace(s)
}

// cssStringReplacer escapes a value for use inside a quoted CSS string token
// (a font-family name, a url(...) target) inside a <style> element.
//
// Same reasoning as the JS half, same reason braces are untouched. The hex
// escapes carry a trailing space, which is how CSS terminates a hex escape
// sequence, and a newline inside a string token is a parse error in CSS so it
// becomes \A instead.
var cssStringReplacer = strings.NewReplacer(
	`\`, `\\`,
	`"`, `\"`,
	`'`, `\'`,
	"\n", `\A `,
	"\r", `\A `,
	"<", `\3c `,
	">", `\3e `,
)

// escapeCSSStringForAstro is exit 3, style half. Like the JS half it returns
// the body of a string token, without the surrounding quotes.
func escapeCSSStringForAstro(s string) string {
	return cssStringReplacer.Replace(s)
}

// cssFontStyleKeyword clamps a stored font-style to the three keywords the CSS
// property accepts. The uploaded value was interpolated into a <style> body
// with a bare %s, so it was not an escaping problem at all: it was a keyword
// slot taking arbitrary CSS text. An unrecognised value becomes normal, which
// is the browser's own default for the property.
func cssFontStyleKeyword(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "italic":
		return "italic"
	case "oblique":
		return "oblique"
	default:
		return "normal"
	}
}

// escapeAstroPropString is exit 4: a quoted attribute on an Astro COMPONENT,
// which is a prop rather than markup.
//
// It deliberately does not neutralise. Measured on a real build:
// `<Base title="{process.env.SECRET}">` renders the literal text, so the value
// is inert, and Astro decodes an entity in a quoted prop before handing it to
// the component. The exit exists so these call sites read as a decision rather
// than as the ones somebody forgot, and so the guard test can tell them apart.
func escapeAstroPropString(s string) string {
	return escapeAttr(s)
}
