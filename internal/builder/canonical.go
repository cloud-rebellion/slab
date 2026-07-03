package builder

import (
	"context"
	"strings"

	"github.com/bright-interaction/slab/internal/store"
)

// CanonicalHostname returns the live canonical hostname for a site.
// Preference order:
//
//  1. site.Domain (the legacy admin "Domain" field, set explicitly)
//  2. The site_domains row marked is_canonical=1 with status='live'
//  3. The first site_domains row at status='live'
//  4. Empty string (caller falls back to a deployment default)
//
// Used by every build step that emits a canonical URL: astro.config
// site, sitemap base, hreflang alternates, OG URLs, robots.txt, llms.txt,
// CookieProof cookie domain. Centralising the lookup here keeps those
// outputs consistent the moment a domain flips from cert_ready to live.
func CanonicalHostname(ctx context.Context, queries *store.Queries, site store.Site) string {
	if v := strings.TrimSpace(site.Domain); v != "" {
		return v
	}
	rows, err := queries.ListDomainsBySite(ctx, site.ID)
	if err != nil {
		return ""
	}
	for _, d := range rows {
		if d.IsCanonical == 1 && d.Status == "live" {
			return d.Hostname
		}
	}
	for _, d := range rows {
		if d.Status == "live" {
			return d.Hostname
		}
	}
	return ""
}
