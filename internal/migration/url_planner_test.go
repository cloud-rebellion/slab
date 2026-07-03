package migration

import (
	"strings"
	"testing"
)

func TestPlanURLs_VerbatimSlugByDefault(t *testing.T) {
	manifest := &MigrationManifest{
		Pages: []MigrationPage{
			{SourcePath: "/about", Title: "About"},
			{SourcePath: "/contact", Title: "Contact"},
		},
	}
	plan := PlanURLs(manifest, nil, DefaultPlanOptions())
	if len(plan.Pages) != 2 {
		t.Fatalf("want 2 pages, got %d", len(plan.Pages))
	}
	for _, p := range plan.Pages {
		if p.NewSlug != p.SourcePath {
			t.Errorf("verbatim rule violated: source=%q new=%q", p.SourcePath, p.NewSlug)
		}
	}
	if len(plan.Redirects) != 0 {
		t.Errorf("verbatim slugs should produce no redirects, got %d", len(plan.Redirects))
	}
}

func TestPlanURLs_StripsPlatformSuffixes(t *testing.T) {
	cases := map[string]string{
		"/about-us.html": "/about-us",
		"/about-us.htm":  "/about-us",
		"/about-us.php":  "/about-us",
		"/about.aspx":    "/about",
	}
	for in, want := range cases {
		manifest := &MigrationManifest{Pages: []MigrationPage{{SourcePath: in, Title: "X"}}}
		plan := PlanURLs(manifest, nil, DefaultPlanOptions())
		if len(plan.Pages) != 1 {
			t.Fatalf("%s: want 1 page", in)
		}
		if plan.Pages[0].NewSlug != want {
			t.Errorf("%s: got %q, want %q", in, plan.Pages[0].NewSlug, want)
		}
		if len(plan.Redirects) != 1 {
			t.Errorf("%s: should emit redirect, got %d", in, len(plan.Redirects))
		}
		if plan.Redirects[0].StatusCode != 301 {
			t.Errorf("%s: status_code: %d", in, plan.Redirects[0].StatusCode)
		}
	}
}

func TestPlanURLs_CollapsesDateArchive(t *testing.T) {
	manifest := &MigrationManifest{Pages: []MigrationPage{
		{SourcePath: "/2020/03/15/welcome", Title: "Welcome"},
	}}
	plan := PlanURLs(manifest, nil, DefaultPlanOptions())
	if plan.Pages[0].NewSlug != "/welcome" {
		t.Errorf("date archive not collapsed: %q", plan.Pages[0].NewSlug)
	}
	if len(plan.Redirects) != 1 || plan.Redirects[0].FromPath != "/2020/03/15/welcome" || plan.Redirects[0].ToPath != "/welcome" {
		t.Errorf("redirect missing: %+v", plan.Redirects)
	}
}

func TestPlanURLs_DateArchiveOptOut(t *testing.T) {
	manifest := &MigrationManifest{Pages: []MigrationPage{
		{SourcePath: "/2020/welcome", Title: "Welcome"},
	}}
	opts := DefaultPlanOptions()
	opts.CollapseDateArchives = false
	plan := PlanURLs(manifest, nil, opts)
	if plan.Pages[0].NewSlug != "/2020/welcome" {
		t.Errorf("opt-out failed: %q", plan.Pages[0].NewSlug)
	}
	if len(plan.Redirects) != 0 {
		t.Errorf("no slug change should mean no redirect, got %d", len(plan.Redirects))
	}
}

func TestPlanURLs_TrailingSlashStripped(t *testing.T) {
	manifest := &MigrationManifest{Pages: []MigrationPage{
		{SourcePath: "/about/", Title: "About"},
		{SourcePath: "/", Title: "Home"},
	}}
	plan := PlanURLs(manifest, nil, DefaultPlanOptions())
	if plan.Pages[0].NewSlug != "/about" {
		t.Errorf("trailing slash not stripped: %q", plan.Pages[0].NewSlug)
	}
	if plan.Pages[1].NewSlug != "/" {
		t.Errorf("root must stay /: %q", plan.Pages[1].NewSlug)
	}
}

func TestPlanURLs_CollisionWithExistingPageBecomesConflict(t *testing.T) {
	existing := map[string]bool{"about": true}
	manifest := &MigrationManifest{Pages: []MigrationPage{
		{SourcePath: "/about", Title: "About"},
	}}
	plan := PlanURLs(manifest, existing, DefaultPlanOptions())
	if len(plan.Pages) != 0 {
		t.Errorf("conflicting page should not be in plan.Pages, got %+v", plan.Pages)
	}
	if len(plan.Conflicts) != 1 {
		t.Fatalf("want 1 conflict, got %d", len(plan.Conflicts))
	}
	if plan.Conflicts[0].ExistingKind != "page" {
		t.Errorf("conflict kind: %q", plan.Conflicts[0].ExistingKind)
	}
}

func TestPlanURLs_IntraPlanCollisionDetected(t *testing.T) {
	// Two date-collapsed posts that resolve to the same slug.
	manifest := &MigrationManifest{Pages: []MigrationPage{
		{SourcePath: "/2020/03/15/welcome", Title: "Welcome 2020"},
		{SourcePath: "/2021/06/01/welcome", Title: "Welcome 2021"},
	}}
	plan := PlanURLs(manifest, nil, DefaultPlanOptions())
	if len(plan.Pages) != 1 {
		t.Fatalf("first should win, got %d in plan.Pages", len(plan.Pages))
	}
	if len(plan.Conflicts) != 1 || plan.Conflicts[0].ExistingKind != "plan_collision" {
		t.Fatalf("intra-plan collision not detected: %+v", plan.Conflicts)
	}
}

func TestPlanURLs_SkipPathsBecome410(t *testing.T) {
	opts := DefaultPlanOptions()
	opts.SkipPaths = map[string]bool{"/old-promo": true}
	manifest := &MigrationManifest{Pages: []MigrationPage{
		{SourcePath: "/old-promo", Title: "Promo"},
		{SourcePath: "/about", Title: "About"},
	}}
	plan := PlanURLs(manifest, nil, opts)
	if len(plan.Pages) != 2 {
		t.Fatalf("want 2 page entries (skipped tracked), got %d", len(plan.Pages))
	}
	skipped := false
	for _, p := range plan.Pages {
		if p.SourcePath == "/old-promo" && p.Skipped {
			skipped = true
		}
	}
	if !skipped {
		t.Errorf("/old-promo not marked Skipped: %+v", plan.Pages)
	}
	gone := false
	for _, r := range plan.Redirects {
		if r.FromPath == "/old-promo" && r.StatusCode == 410 {
			gone = true
		}
	}
	if !gone {
		t.Errorf("skipped path should produce 410 redirect: %+v", plan.Redirects)
	}
}

func TestPlanURLs_LocalePrefixPreserved(t *testing.T) {
	manifest := &MigrationManifest{Pages: []MigrationPage{
		{SourcePath: "/sv/om-oss", Title: "Om oss"},
	}}
	plan := PlanURLs(manifest, nil, DefaultPlanOptions())
	if plan.Pages[0].NewSlug != "/sv/om-oss" {
		t.Errorf("locale prefix not preserved: %q", plan.Pages[0].NewSlug)
	}
}

func TestPlanURLs_QueryStringStripped(t *testing.T) {
	manifest := &MigrationManifest{Pages: []MigrationPage{
		{SourcePath: "/about?ref=email", Title: "About"},
	}}
	plan := PlanURLs(manifest, nil, DefaultPlanOptions())
	if plan.Pages[0].NewSlug != "/about" {
		t.Errorf("query string not stripped: %q", plan.Pages[0].NewSlug)
	}
}

func TestPlanURLs_WPIndexFallbackRoutedToRoot(t *testing.T) {
	manifest := &MigrationManifest{Pages: []MigrationPage{
		{SourcePath: "/index.php?p=42", Title: "Legacy"},
	}}
	plan := PlanURLs(manifest, nil, DefaultPlanOptions())
	if plan.Pages[0].NewSlug != "/" {
		t.Errorf("wp index.php fallback should route to /: %q", plan.Pages[0].NewSlug)
	}
}

func TestPlanURLs_HostileCharsSanitized(t *testing.T) {
	// Path with a forbidden char (semicolon, which is safe-list excluded).
	manifest := &MigrationManifest{Pages: []MigrationPage{
		{SourcePath: "/about;jsessionid=abc", Title: "About"},
	}}
	plan := PlanURLs(manifest, nil, DefaultPlanOptions())
	if strings.ContainsAny(plan.Pages[0].NewSlug, ";=") {
		t.Errorf("forbidden chars not sanitised: %q", plan.Pages[0].NewSlug)
	}
}

func TestPlanURLs_CollectionItemsTracked(t *testing.T) {
	manifest := &MigrationManifest{
		Pages: []MigrationPage{},
		Collections: []MigrationCollection{
			{
				Slug: "posts", Name: "Posts",
				Items: []MigrationCollectionItem{
					{Slug: "hello", Title: "Hello", SourceURL: "https://x/posts/hello/"},
					{Slug: "world", Title: "World", SourceURL: "https://x/posts/world/"},
				},
			},
		},
	}
	plan := PlanURLs(manifest, nil, DefaultPlanOptions())
	if len(plan.CollectionItems) != 2 {
		t.Fatalf("want 2 item plans, got %d", len(plan.CollectionItems))
	}
	if plan.CollectionItems[0].CollectionSlug != "posts" {
		t.Errorf("collection slug missing: %+v", plan.CollectionItems[0])
	}
}

func TestNormalizePath_CollisionKey(t *testing.T) {
	cases := map[string]string{
		"/About-Us":       "/about-us",
		"/about-us/":      "/about-us",
		"/about-us.html":  "/about-us",
		"/about-us.HTML":  "/about-us",
		"/about-us?ref=x": "/about-us",
		"/":               "/",
	}
	for in, want := range cases {
		if got := normalizePath(in); got != want {
			t.Errorf("normalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}
