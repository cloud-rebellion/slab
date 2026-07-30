// Package billing holds the plan ladder + per-plan limits used by the
// Cloud Tier MVP (Phase 30.2). The package is OSS-safe: it ships in
// every build and the OSS path looks up "oss" plan which returns -1
// (unlimited) for every limit. Cloud builds (-tags ee) read from the
// same map and gate quotas accordingly.
//
// Pricing is in EUR cents (atomicsite is EU-sovereign by design):
//
//	solo   = €19/mo  (1 site, atomicsite subdomain only, 1GB media)
//	studio = €79/mo  (5 sites, custom domain, 10GB media, 3000 build mins)
//	agency = €299/mo (25 sites, white-label, 50GB media, 12000 build mins)
//	oss    = free    (no limits, self-hosted)
package billing

import "log/slog"

// Plan describes one tier in the plan ladder. -1 means unlimited.
//
// MaxPagesPerSite + MaxRedirectsPerSite were added 2026-05-06 (Sprint 3
// of the post-Layer-3d migration plan) to gate migration Apply + bulk
// redirect import against plan limits. Without these dimensions the
// migration porter could insert thousands of rows into a free-tier site
// in one call, blowing past the customer's actual contract.
type Plan struct {
	Key                 string `json:"key"`
	Name                string `json:"name"`
	PriceCents          int64  `json:"price_cents"`
	Currency            string `json:"currency"`
	MaxSites            int64  `json:"max_sites"`
	MaxPagesPerSite     int64  `json:"max_pages_per_site"`
	MaxRedirectsPerSite int64  `json:"max_redirects_per_site"`
	StorageGB           int64  `json:"storage_gb"`
	BuildMinutes        int64  `json:"build_minutes"`
	CustomDomain        bool   `json:"custom_domain"`
	WhiteLabel          bool   `json:"white_label"`
	PrioritySupport     bool   `json:"priority_support"`
}

// Plans is the canonical plan ladder. Lookup by key. The OSS plan is
// always present; Solo/Studio/Agency are present in every build but
// only enforced when the EE Provider routes quota checks through here.
var Plans = map[string]Plan{
	"oss": {
		Key:                 "oss",
		Name:                "OSS",
		PriceCents:          0,
		Currency:            "EUR",
		MaxSites:            -1,
		MaxPagesPerSite:     -1,
		MaxRedirectsPerSite: -1,
		StorageGB:           -1,
		BuildMinutes:        -1,
		CustomDomain:        true,
		WhiteLabel:          true,
	},
	"solo": {
		Key:                 "solo",
		Name:                "Solo",
		PriceCents:          1900,
		Currency:            "EUR",
		MaxSites:            1,
		MaxPagesPerSite:     50,
		MaxRedirectsPerSite: 500,
		StorageGB:           1,
		BuildMinutes:        600,
		CustomDomain:        false,
		WhiteLabel:          false,
	},
	"studio": {
		Key:                 "studio",
		Name:                "Studio",
		PriceCents:          7900,
		Currency:            "EUR",
		MaxSites:            5,
		MaxPagesPerSite:     250,
		MaxRedirectsPerSite: 2500,
		StorageGB:           10,
		BuildMinutes:        3000,
		CustomDomain:        true,
		WhiteLabel:          false,
	},
	"agency": {
		Key:                 "agency",
		Name:                "Agency",
		PriceCents:          29900,
		Currency:            "EUR",
		MaxSites:            25,
		MaxPagesPerSite:     1000,
		MaxRedirectsPerSite: 10000,
		StorageGB:           50,
		BuildMinutes:        12000,
		CustomDomain:        true,
		WhiteLabel:          true,
		PrioritySupport:     true,
	},
}

// Lookup returns the plan for a key. Unknown keys fall back to the
// oss plan so a malformed workspace.plan column can't break a build.
func Lookup(key string) Plan {
	if p, ok := Plans[key]; ok {
		return p
	}
	return Plans["oss"]
}

// Limit returns the cap for a (plan, resource) pair. resource is one
// of "max_sites", "storage_gb", "build_minutes", "custom_domain"
// (1 if allowed, 0 if not), "white_label" (same shape). Returns -1
// for unlimited. Unknown resources return -1 so a check can't
// accidentally refuse on a typo.
func Limit(planKey, resource string) int64 {
	p := Lookup(planKey)
	switch resource {
	case "max_sites":
		return p.MaxSites
	case "max_pages_per_site":
		return p.MaxPagesPerSite
	case "max_redirects_per_site":
		return p.MaxRedirectsPerSite
	case "storage_gb":
		return p.StorageGB
	case "build_minutes":
		return p.BuildMinutes
	case "custom_domain":
		if p.CustomDomain {
			return 1
		}
		return 0
	case "white_label":
		if p.WhiteLabel {
			return 1
		}
		return 0
	default:
		// Fail CLOSED on an unrecognised resource. This returned -1
		// ("unlimited"), and every gate treats -1 as allow, so a typo'd
		// resource name in a NEW gate would silently never apply the cap on any
		// tier, and no test would catch it because a passing quota check is the
		// expected result in most tests. Allows() inherits the same fix, so
		// Allows(plan, "whitelabel") no longer grants white-label to the free
		// oss plan. All current call sites pass correct literals, so this
		// changes no live behaviour; it changes the direction of the next
		// mistake.
		slog.Warn("billing: unknown plan resource, denying by default", "plan", planKey, "resource", resource)
		return 0
	}
}

// Allows reports whether a plan permits a boolean feature
// ("custom_domain" or "white_label"). Convenience wrapper over Limit.
func Allows(planKey, feature string) bool {
	return Limit(planKey, feature) > 0 || Limit(planKey, feature) == -1
}

// All returns the public plan list (everything except oss) so the
// pricing page + plan picker can render them. Sorted by price ascending.
func All() []Plan {
	out := []Plan{Plans["solo"], Plans["studio"], Plans["agency"]}
	return out
}
