package builder

import (
	"context"
	"strings"

	"github.com/bright-interaction/slab/internal/store"
)

// resolveDefaultOgImageURL turns the operator-set default OG image into a
// fully-qualified absolute URL. Resolution order:
//
//  1. seo.og_default_image_id setting (Settings -> SEO -> Default Open Graph image)
//  2. sites.og_image_id legacy column (written by the onboarding wizard)
//
// Empty string is returned when neither is set or when the referenced media row
// is missing or still processing. Callers should treat the empty case as "do
// not emit og:image" rather than emitting a broken URL.
func resolveDefaultOgImageURL(ctx context.Context, queries *store.Queries, site store.Site, settingsMap map[string]string, domain string) string {
	mediaID := strings.TrimSpace(settingsMap["seo.og_default_image_id"])
	if mediaID == "" {
		mediaID = strings.TrimSpace(site.OgImageID)
	}
	if mediaID == "" {
		return ""
	}
	m, err := queries.GetMediaByID(ctx, mediaID)
	if err != nil || m.SiteID != site.ID {
		return ""
	}
	// /media/<id>/original.<ext> is the variant CopyMedia hardlinks for every
	// processed image (see internal/imaging.Process). Falls back to the raw
	// filename if OriginalPath isn't set yet.
	suffix := "original" + extOf(m.Filename)
	if m.OriginalPath != "" {
		// OriginalPath is "<dataDir>/media/<site>/<id>/original.<ext>" or similar;
		// keep only the basename so the URL stays stable.
		parts := strings.Split(m.OriginalPath, "/")
		if last := parts[len(parts)-1]; last != "" {
			suffix = last
		}
	}
	return strings.TrimRight(domain, "/") + "/media/" + mediaID + "/" + suffix
}

// domainForOgImage returns the canonical domain for the site's absolute URLs,
// matching the prefix layouts.go uses for canonicalURL. Falls back to localhost
// when the site row's domain is empty (dev / pre-deploy state).
func domainForOgImage(site store.Site) string {
	d := site.Domain
	if d == "" {
		d = "localhost"
	}
	if !strings.HasPrefix(d, "http") {
		d = "https://" + d
	}
	return d
}

func extOf(filename string) string {
	i := strings.LastIndex(filename, ".")
	if i < 0 {
		return ""
	}
	return filename[i:]
}

// resolveAnalyticsConfig collects every analytics-pipeline knob into one
// struct so layouts.go and the agent context can speak about them with a
// single shape. Reads precedence: the analytics-category settings rows
// (frontend Analytics page) override the legacy sites-table columns
// (ga4_id / umami_id / umami_url) because the settings rows are the
// authoritative surface going forward.
type analyticsConfig struct {
	GA4Enabled    bool
	GA4ID         string
	UmamiEnabled  bool
	UmamiURL      string
	UmamiSiteID   string
	CookieProofOn bool
	TrackPath     string
}

func resolveAnalyticsConfig(site store.Site, settingsMap map[string]string) analyticsConfig {
	cfg := analyticsConfig{
		GA4ID:       strings.TrimSpace(settingsMap["analytics.ga4_id"]),
		UmamiURL:    strings.TrimSpace(settingsMap["analytics.umami_url"]),
		UmamiSiteID: strings.TrimSpace(settingsMap["analytics.umami_site_id"]),
		TrackPath:   orDefault(settingsMap["analytics.track_path"], "/t"),
	}
	if cfg.GA4ID == "" {
		cfg.GA4ID = strings.TrimSpace(site.Ga4ID)
	}
	if cfg.UmamiURL == "" {
		cfg.UmamiURL = strings.TrimSpace(site.UmamiUrl)
	}
	if cfg.UmamiSiteID == "" {
		cfg.UmamiSiteID = strings.TrimSpace(site.UmamiID)
	}

	// Explicit toggles win, but a populated id implies enabled when the
	// toggle was never written. Rationale: the agent that PATCH'd ga4_id is
	// almost always done so to turn it on, not to silently stage it.
	cfg.GA4Enabled = boolSetting(settingsMap["analytics.ga4_enabled"], cfg.GA4ID != "")
	cfg.UmamiEnabled = boolSetting(settingsMap["analytics.umami_enabled"], cfg.UmamiURL != "" && cfg.UmamiSiteID != "")
	cfg.CookieProofOn = boolSetting(settingsMap["analytics.cookieproof_enabled"], false)
	return cfg
}
