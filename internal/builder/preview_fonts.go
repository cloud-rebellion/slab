package builder

import (
	"context"
	"fmt"
	"strings"

	"github.com/bright-interaction/slab/internal/store"
)

// fontFaceDecl formats one @font-face rule for a site-uploaded font. Both the
// production emission (layouts.go, into a <style> in Base.astro) and the
// preview emission below go through it, because they were two copies of the
// same fmt.Sprintf and the escaping defect had to be fixed in both: %q quotes
// for Go and leaves `<` alone, so a font family carrying `</style>` closed the
// element and became live markup, and font-style went in with a bare %s.
//
// The braces of the rule itself are written here, never escaped: this string
// lands inside a <style> element, which Astro treats as raw text.
func fontFaceDecl(family, style string, weight int64, src, unicodeRange string) string {
	decl := fmt.Sprintf(
		`@font-face { font-family: "%s"; font-style: %s; font-weight: %d; font-display: swap; src: url("%s") format('woff2');`,
		escapeCSSStringForAstro(family), cssFontStyleKeyword(style), weight, escapeCSSStringForAstro(src),
	)
	if unicodeRange != "" {
		decl += fmt.Sprintf(" unicode-range: %s;", unicodeRange)
	}
	return decl + " }"
}

// SiteFontsCSS returns the @font-face block for one site's uploaded fonts.
// Self-hosted woff2 only — atomicsite does not load Google Fonts. Returns
// the empty string when the site has no uploaded fonts. Used by the
// per-block preview-html endpoint so the iframe matches the production
// font stack from layouts.go.
func SiteFontsCSS(ctx context.Context, queries *store.Queries, siteID string) string {
	rows, err := queries.ListSiteFonts(ctx, siteID)
	if err != nil || len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	for _, f := range rows {
		src := fmt.Sprintf("/atomicsite-fonts/%s/%s.woff2", siteID, f.ID)
		b.WriteString(fontFaceDecl(f.FamilyName, f.Style, f.Weight, src, "") + "\n")
	}
	return b.String()
}
