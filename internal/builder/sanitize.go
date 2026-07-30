package builder

import (
	"regexp"
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

// astroExpressionReplacer neutralises Astro's build-time expression syntax.
//
// The raw_astro block's output is written into a generated .astro file, so
// `{...}` there is a JavaScript expression Astro evaluates in Node during the
// build, with access to the build process environment. That makes it a code
// sink, not a markup sink, and sanitizing HTML alone does not close it:
// bluemonday has no reason to strip braces from text.
//
// Entities render as literal braces in the finished page and mean nothing to
// the Astro compiler, so tenant text still displays as written.
var astroExpressionReplacer = strings.NewReplacer("{", "&#123;", "}", "&#125;")

func neutraliseAstroExpressions(s string) string {
	return astroExpressionReplacer.Replace(s)
}
