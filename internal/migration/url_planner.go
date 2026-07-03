package migration

import (
	"path"
	"regexp"
	"strings"
)

// URLPlan is the proposed shape of the new atomicsite site once a manifest
// is committed. Built from a MigrationManifest plus the existing-page set
// for collision detection. The agent or operator reviews this BEFORE the
// porter writes anything to the DB; the verify endpoint (Layer 2) reads
// from the same plan to surface "X URLs will 404 after launch".
type URLPlan struct {
	// Pages: source-page paths and their proposed atomicsite slug.
	Pages []PagePlan `json:"pages"`

	// CollectionItems: keyed by collection slug; the planner doesn't
	// rewrite item slugs (they stay verbatim from the source) but it
	// still records collisions and the redirects required when the
	// collection-item URL pattern differs from the source.
	CollectionItems []ItemPlan `json:"collection_items,omitempty"`

	// Redirects: every from->to pair the porter must insert. A row
	// appears here when (a) the source path differs from the proposed
	// atomicsite path, or (b) the source page was explicitly skipped
	// (status_code 410, "gone").
	Redirects []RedirectPlan `json:"redirects"`

	// Conflicts: source URLs whose proposed slug collides with an
	// existing live atomicsite page or another item in the same plan.
	// Non-empty Conflicts blocks Apply unless the operator picks an
	// override per row.
	Conflicts []PlanConflict `json:"conflicts,omitempty"`
}

// PagePlan is one source page mapped to its target atomicsite slug.
type PagePlan struct {
	SourcePath string `json:"source_path"`
	SourceURL  string `json:"source_url,omitempty"`
	NewSlug    string `json:"new_slug"`
	Title      string `json:"title"`
	Skipped    bool   `json:"skipped,omitempty"` // 410-gone
	Reason     string `json:"reason,omitempty"`  // why the slug differs
}

// ItemPlan is one collection item; CollectionSlug is the parent collection.
type ItemPlan struct {
	CollectionSlug string `json:"collection_slug"`
	SourceURL      string `json:"source_url,omitempty"`
	SourcePath     string `json:"source_path,omitempty"`
	NewSlug        string `json:"new_slug"`
	Title          string `json:"title"`
}

// RedirectPlan mirrors a row in the redirects table. is_auto stays 1 because
// the planner derives these mechanically; humans manage manual rules via the
// CRUD API from Layer 2.
type RedirectPlan struct {
	FromPath   string `json:"from_path"`
	ToPath     string `json:"to_path"`
	StatusCode int    `json:"status_code"` // 301 default; 410 for skipped
}

// PlanConflict surfaces a slug collision so the operator can resolve it
// before commit. The migration UI groups these and offers per-row actions
// (rename, skip, replace).
type PlanConflict struct {
	SourcePath   string `json:"source_path"`
	ProposedSlug string `json:"proposed_slug"`
	ExistingKind string `json:"existing_kind"` // "page" or "plan_collision"
	Detail       string `json:"detail"`
}

// PlanOptions controls planner behaviour. Empty struct uses safe defaults
// (preserve slugs verbatim, strip platform suffixes, collapse date archives).
type PlanOptions struct {
	// CollapseDateArchives turns `/YYYY/MM/DD/post-slug` into `/post-slug`
	// and emits the redirect from the old date-bearing path. Default true:
	// every modern CMS abandoned date-based permalinks for this exact
	// reason and we want migrations to follow.
	CollapseDateArchives bool

	// PreserveLocalePrefix keeps `/sv/about` mapped to `/sv/about` so
	// hreflang alternates already in atomicsite stay intact. Default true.
	PreserveLocalePrefix bool

	// SkipPaths is the operator's allow-list of source paths to NOT import.
	// Each becomes a 410 ("gone") redirect so Google drops the URL cleanly
	// instead of leaving a soft-404. Path-only, with leading slash.
	SkipPaths map[string]bool
}

// DefaultPlanOptions returns the recommended defaults: collapse date archives,
// preserve locale prefix.
func DefaultPlanOptions() PlanOptions {
	return PlanOptions{
		CollapseDateArchives: true,
		PreserveLocalePrefix: true,
		SkipPaths:            map[string]bool{},
	}
}

// Compile-once regexes used by the slug normaliser.
var (
	dateArchiveRE = regexp.MustCompile(`^/(\d{4})(?:/(\d{2}))?(?:/(\d{2}))?/`)
	wpQueryPostRE = regexp.MustCompile(`^/index\.php\?p=\d+$`)
)

// Slug shape kept conservative: lowercase, alphanumerics, hyphens, slashes
// for paths, and unicode letters/digits (so accented WP slugs survive). The
// shape mirrors what handlers/agent.go ValidatePageSlug accepts.
var slugSafeRE = regexp.MustCompile(`[^a-zA-Z0-9\-_/\p{L}\p{N}]`)

// PlanURLs is the planner's main entry point. It walks the manifest,
// applies the slug-derivation rules, detects collisions against the
// existingSlugs set (use the result of ListPagesBySite via the caller),
// and returns a fully-populated URLPlan.
//
// Link-juice rules in priority order:
//  1. SkipPaths overrides everything → 410.
//  2. Locale prefix preserved verbatim when enabled.
//  3. Date archive collapse (`/2020/03/15/foo` → `/foo`) when enabled.
//  4. Strip platform suffixes (`.html`, `.php`, trailing slash).
//  5. WordPress fallback `/index.php?p=N` → drop (no semantic equivalent).
//  6. If the proposed slug collides with an existing page, emit a Conflict
//     row instead of silently overwriting.
//
// Every step that changes the slug also creates a redirect from the source
// path so Google walks one 301 to the new URL.
func PlanURLs(m *MigrationManifest, existingSlugs map[string]bool, opts PlanOptions) URLPlan {
	if existingSlugs == nil {
		existingSlugs = map[string]bool{}
	}
	plan := URLPlan{}
	planSlugs := map[string]string{} // proposed -> source (intra-plan dup detection)

	for _, p := range m.Pages {
		src := p.SourcePath
		if src == "" {
			continue
		}
		// Always normalise the source path itself first (so
		// `/about-us.html` and `/about-us` collapse to the same key
		// for collision detection).
		canon := normalizePath(src)

		// Skipped paths short-circuit to 410.
		if opts.SkipPaths[canon] || opts.SkipPaths[src] {
			plan.Pages = append(plan.Pages, PagePlan{
				SourcePath: src, SourceURL: p.SourceURL,
				NewSlug: "", Title: p.Title, Skipped: true,
				Reason: "operator marked as skipped",
			})
			plan.Redirects = append(plan.Redirects, RedirectPlan{
				FromPath: src, ToPath: "/", StatusCode: 410,
			})
			continue
		}

		newSlug, reason := proposeSlug(src, opts)

		// Collision check: existing live atomicsite page wins, the
		// porter would then create a redirect not a page (handled
		// below as a Conflict so the operator can decide).
		if existingSlugs[strings.TrimPrefix(newSlug, "/")] {
			plan.Conflicts = append(plan.Conflicts, PlanConflict{
				SourcePath: src, ProposedSlug: newSlug,
				ExistingKind: "page",
				Detail:       "atomicsite already serves a page at this slug",
			})
			continue
		}
		// Intra-plan duplicate: two source pages collapsed to the same
		// proposed slug (common with date-archive collapse when two
		// posts shared a slug across years). Surface so the operator
		// can rename one.
		if other, dup := planSlugs[newSlug]; dup {
			plan.Conflicts = append(plan.Conflicts, PlanConflict{
				SourcePath: src, ProposedSlug: newSlug,
				ExistingKind: "plan_collision",
				Detail:       "another source page (" + other + ") proposed the same slug",
			})
			continue
		}
		planSlugs[newSlug] = src

		plan.Pages = append(plan.Pages, PagePlan{
			SourcePath: src, SourceURL: p.SourceURL,
			NewSlug: newSlug, Title: p.Title, Reason: reason,
		})
		// Emit redirect when the slug actually changed.
		if src != newSlug {
			plan.Redirects = append(plan.Redirects, RedirectPlan{
				FromPath: src, ToPath: newSlug, StatusCode: 301,
			})
		}
	}

	// Collection items: slugs are verbatim from the source CMS (Webflow's
	// `slug` field, WP's `slug`, Ghost's `slug`). The planner records
	// them so the porter has one place to walk.
	for _, coll := range m.Collections {
		for _, it := range coll.Items {
			plan.CollectionItems = append(plan.CollectionItems, ItemPlan{
				CollectionSlug: coll.Slug,
				SourceURL:      it.SourceURL,
				SourcePath:     pathOf(it.SourceURL),
				NewSlug:        it.Slug,
				Title:          it.Title,
			})
		}
	}

	return plan
}

// proposeSlug runs the link-juice-preserving normalisation rules in priority
// order. Returns the new slug and a short reason string when it differs
// from the input (empty reason = verbatim match).
func proposeSlug(src string, opts PlanOptions) (string, string) {
	out := src
	reason := ""

	// WP fallback URLs like /index.php?p=42 have no slug to preserve.
	// Drop the query string and route to root; the operator will
	// manually map these few rows if any survive in the sitemap.
	if wpQueryPostRE.MatchString(out) {
		return "/", "wp index.php fallback URL has no slug; route to /"
	}

	// Strip query string + fragment unconditionally.
	if i := strings.IndexAny(out, "?#"); i >= 0 {
		out = out[:i]
		if reason == "" {
			reason = "stripped query string"
		}
	}

	// Strip platform file suffixes.
	for _, suf := range []string{".html", ".htm", ".php", ".aspx"} {
		if strings.HasSuffix(strings.ToLower(out), suf) {
			out = out[:len(out)-len(suf)]
			if reason == "" {
				reason = "stripped " + suf
			}
			break
		}
	}

	// Collapse date archives BEFORE trailing-slash strip so we still
	// match the leading shape.
	if opts.CollapseDateArchives {
		if m := dateArchiveRE.FindStringSubmatchIndex(out); m != nil {
			collapsed := "/" + strings.TrimPrefix(out[m[1]:], "/")
			if collapsed != "" && collapsed != "/" {
				out = collapsed
				if reason == "" {
					reason = "collapsed date archive"
				}
			}
		}
	}

	// Trailing slash: strip except for root.
	if out != "/" {
		out = strings.TrimRight(out, "/")
	}

	// Index sentinel: `/index` → `/`.
	if out == "/index" {
		out = "/"
		if reason == "" {
			reason = "/index → /"
		}
	}

	// Sanitise any character outside the safe set.
	if cleaned := slugSafeRE.ReplaceAllString(out, "-"); cleaned != out {
		out = cleaned
		if reason == "" {
			reason = "sanitised"
		}
	}

	// Collapse repeated dashes / slashes left by sanitisation.
	out = collapseSeparators(out)
	if out == "" {
		out = "/"
	}
	if !strings.HasPrefix(out, "/") {
		out = "/" + out
	}

	if out == src {
		return out, ""
	}
	return out, reason
}

// collapseSeparators turns `--` into `-` and `//` into `/` after
// regex-replace produced runs of dashes for forbidden characters.
func collapseSeparators(s string) string {
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	return s
}

// normalizePath is the canonical form used for collision keys: lowercase,
// no trailing slash, no .html/.php suffix, no query string. Used so two
// paths that differ only in cosmetic detail collide intentionally.
func normalizePath(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	for _, suf := range []string{".html", ".htm", ".php", ".aspx"} {
		if strings.HasSuffix(s, suf) {
			s = s[:len(s)-len(suf)]
			break
		}
	}
	if s != "/" {
		s = strings.TrimRight(s, "/")
	}
	return path.Clean(s)
}
