package builder

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/bright-interaction/slab/internal/imaging"
	"github.com/bright-interaction/slab/internal/store"
)

// RenderPicture emits a <picture> with srcset, sizes, and lazy loading. Kept
// as the default (non-priority) form so existing callsites stay byte-identical.
func RenderPicture(m store.Medium, cssClassName string) string {
	return RenderPictureWithPriority(m, cssClassName, false)
}

// RenderPictureWithPriority is the LCP-aware form. When priority is true the
// rendered <img> emits loading="eager" + fetchpriority="high" instead of
// loading="lazy"; below-the-fold images stay lazy. Hero blocks call this with
// priority=true so the LCP element is not deferred behind layout-related JS.
//
// Paths use /media/{mediaID}/... (no siteID) because CopyMedia flattens the
// workspace layout.
func RenderPictureWithPriority(m store.Medium, cssClassName string, priority bool) string {
	if m.VariantsJson == "" {
		return ""
	}
	var variants []imaging.Variant
	if err := json.Unmarshal([]byte(m.VariantsJson), &variants); err != nil {
		return ""
	}

	// Strip the {siteID}/ prefix from every variant path so URLs match
	// the workspace's /media/{mediaID}/... layout.
	for i := range variants {
		variants[i].Path = stripSitePrefix(variants[i].Path, m.SiteID)
	}

	// Partition by format
	webp := filterByFormat(variants, "webp")
	other := filterNotFormat(variants, "webp")

	// Sort ascending by width for srcset order
	sort.Slice(webp, func(i, j int) bool { return webp[i].Width < webp[j].Width })
	sort.Slice(other, func(i, j int) bool { return other[i].Width < other[j].Width })

	var b strings.Builder
	b.WriteString("<picture>\n")

	if len(webp) > 0 {
		b.WriteString(`  <source type="image/webp" srcset="`)
		for i, v := range webp {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(fmt.Sprintf("/media/%s %dw", v.Path, v.Width))
		}
		b.WriteString(`" sizes="(max-width: 768px) 100vw, 1200px" />` + "\n")
	}

	// Pick a fallback <img>. Prefer the widest non-original variant in source
	// format; if source format was webp (so `other` is empty), fall back to the
	// widest webp variant so browsers without <picture> support still get an img.
	var fallback imaging.Variant
	pickFrom := other
	if len(pickFrom) == 0 {
		pickFrom = webp
	}
	fallbackVariants := pickFrom
	for _, v := range fallbackVariants {
		if !strings.HasSuffix(v.Path, "/original."+imaging.ExtFromFormat(v.Format)) {
			if v.Width > fallback.Width {
				fallback = v
			}
		}
	}
	if fallback.Path == "" && len(fallbackVariants) > 0 {
		fallback = fallbackVariants[len(fallbackVariants)-1]
	}
	if fallback.Path != "" {
		b.WriteString(`  <img src="/media/` + fallback.Path + `"`)
		if len(fallbackVariants) > 0 {
			b.WriteString(` srcset="`)
			for i, v := range fallbackVariants {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(fmt.Sprintf("/media/%s %dw", v.Path, v.Width))
			}
			b.WriteString(`"`)
		}
		b.WriteString(fmt.Sprintf(` width="%d" height="%d"`, fallback.Width, fallback.Height))
		b.WriteString(fmt.Sprintf(` alt="%s"`, escapeAttrForAstro(m.AltText)))
		if priority {
			b.WriteString(` loading="eager" fetchpriority="high" decoding="async"`)
		} else {
			b.WriteString(` loading="lazy" decoding="async"`)
		}
		if cssClassName != "" {
			b.WriteString(fmt.Sprintf(` class="%s"`, escapeAttrForAstro(cssClassName)))
		}
		// Sprint 5 quick-win (2026-05-24): per-image focal point.
		// Default 50/50 = centered; CSS object-position only emits
		// when the operator moved it away from centered, so existing
		// images keep their current byte-identical markup.
		if m.FocalX != 50 || m.FocalY != 50 {
			b.WriteString(fmt.Sprintf(` style="object-position:%d%% %d%%"`, m.FocalX, m.FocalY))
		}
		b.WriteString(" />\n")
	}

	b.WriteString("</picture>")
	return b.String()
}

func filterByFormat(in []imaging.Variant, format string) []imaging.Variant {
	out := make([]imaging.Variant, 0, len(in))
	for _, v := range in {
		if v.Format == format {
			out = append(out, v)
		}
	}
	return out
}

func filterNotFormat(in []imaging.Variant, format string) []imaging.Variant {
	out := make([]imaging.Variant, 0, len(in))
	for _, v := range in {
		if v.Format != format {
			out = append(out, v)
		}
	}
	return out
}

func stripSitePrefix(path, siteID string) string {
	prefix := siteID + "/"
	if strings.HasPrefix(path, prefix) {
		return strings.TrimPrefix(path, prefix)
	}
	return path
}
