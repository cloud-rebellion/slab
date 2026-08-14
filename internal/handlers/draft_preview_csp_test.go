package handlers

import (
	"context"
	"database/sql"
	"go/ast"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"golang.org/x/tools/go/packages"

	"github.com/bright-interaction/slab/internal/builder"
	"github.com/bright-interaction/slab/internal/config"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

// ---------------------------------------------------------------------------
// The structural guard, type-resolved.
//
// The defect this replaces was not "the CSP is wrong", it was "the CSP is on
// one of three routes". A hand-listed route table would have reproduced the
// original mistake exactly.
//
// The FIRST cut of this guard scanned file text for the bytes
// "builder.RenderPageDraft(" and "serveDraftPreview(" and asserted both
// appeared somewhere in the same FILE. Mutation testing found four evasions
// that all compiled, ran, and passed: a second method appended to pages.go
// (the file already contained the helper token, so the new method was
// invisible), an aliased import, a package-level func value, and moving the
// route to another package entirely. So this now resolves the IMPORT PATH and
// the OBJECT through go/types and works per ENCLOSING FUNCTION, not per file.
// ---------------------------------------------------------------------------

const (
	builderPkgSuffix  = "/internal/builder"
	handlersPkgSuffix = "/internal/handlers"
)

// renderPageDraftRef is one place the module refers to builder.RenderPageDraft.
type renderPageDraftRef struct {
	pos       string // file:line
	enclosing string // enclosing function name, "" if package level
	guarded   bool   // enclosing function also references serveDraftPreview
}

// findRenderPageDraftRefs type-resolves every reference to
// builder.RenderPageDraft across the whole module.
//
// References, not calls: `var f = builder.RenderPageDraft` never appears as a
// CallExpr with that callee, so a call-only walk misses the func-value escape.
func findRenderPageDraftRefs(t *testing.T) []renderPageDraftRef {
	t.Helper()

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports |
			packages.NeedDeps,
		Dir:   "../..", // module root, so ./... is the WHOLE module
		Tests: false,   // production code only; tests may call the renderer freely
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		t.Fatalf("load packages: %v", err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		t.Fatal("package load reported errors; the guard cannot resolve types and would pass vacuously")
	}

	var refs []renderPageDraftRef
	for _, pkg := range pkgs {
		if pkg.TypesInfo == nil {
			continue
		}
		for _, file := range pkg.Syntax {
			// Walk each top-level FuncDecl separately, so a reference is
			// attributed to exactly the function that contains it. A single
			// ast.Inspect over the file with a push-only stack would misattribute
			// package-level references to the previous function.
			inFunc := map[*ast.Ident]bool{}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				guarded := funcReferencesServeHelper(pkg, fn)
				for _, id := range renderPageDraftIdents(pkg, fn) {
					inFunc[id] = true
					refs = append(refs, renderPageDraftRef{
						pos:       pkg.Fset.Position(id.Pos()).String(),
						enclosing: fn.Name.Name,
						guarded:   guarded,
					})
				}
			}
			// Anything left is a package-level reference: a func value, a var,
			// an init-time alias. Those escape the per-function rule entirely.
			for _, id := range renderPageDraftIdents(pkg, file) {
				if inFunc[id] {
					continue
				}
				refs = append(refs, renderPageDraftRef{
					pos: pkg.Fset.Position(id.Pos()).String(),
				})
			}
		}
	}
	return refs
}

// renderPageDraftIdents returns every identifier under n that type-resolves to
// builder.RenderPageDraft. Resolution is by OBJECT and IMPORT PATH, so an
// aliased or dot import is the same object and cannot evade.
func renderPageDraftIdents(pkg *packages.Package, n ast.Node) []*ast.Ident {
	var out []*ast.Ident
	ast.Inspect(n, func(node ast.Node) bool {
		id, ok := node.(*ast.Ident)
		if !ok {
			return true
		}
		obj := pkg.TypesInfo.Uses[id]
		if obj == nil || obj.Pkg() == nil {
			return true
		}
		if obj.Name() == "RenderPageDraft" && strings.HasSuffix(obj.Pkg().Path(), builderPkgSuffix) {
			out = append(out, id)
		}
		return true
	})
	return out
}

// funcReferencesServeHelper reports whether fn contains a resolved reference
// to handlers.serveDraftPreview.
func funcReferencesServeHelper(pkg *packages.Package, fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		obj := pkg.TypesInfo.Uses[id]
		if obj == nil || obj.Pkg() == nil {
			return true
		}
		if obj.Name() == "serveDraftPreview" &&
			strings.HasSuffix(obj.Pkg().Path(), handlersPkgSuffix) {
			found = true
			return false
		}
		return true
	})
	return found
}

// TestEveryRenderPageDraftCallerServesThroughHelper is the by-construction
// half. It does not care which routes exist; it cares that no FUNCTION
// anywhere in the module touches the draft renderer without handing the bytes
// to the one helper that attaches the containment policy.
func TestEveryRenderPageDraftCallerServesThroughHelper(t *testing.T) {
	refs := findRenderPageDraftRefs(t)

	// Drift check. If the renderer is renamed or moved and this finds nothing,
	// a silent pass would be worse than a failure.
	if len(refs) < 3 {
		t.Fatalf("found only %d references to builder.RenderPageDraft in the module; "+
			"expected at least 3 (the three preview routes). The guard is keyed on that "+
			"symbol and has drifted: %+v", len(refs), refs)
	}

	for _, ref := range refs {
		if ref.enclosing == "" {
			t.Errorf("%s: builder.RenderPageDraft is referenced at package level "+
				"(a func value or var). That escapes the per-function guard entirely: "+
				"call it directly inside a handler that also calls serveDraftPreview.", ref.pos)
			continue
		}
		if !ref.guarded {
			t.Errorf("%s: func %s calls builder.RenderPageDraft but never calls "+
				"serveDraftPreview. Draft-preview bytes must be written by that helper "+
				"or they ship without the sandbox CSP that contains agent-authored "+
				"component templates.", ref.pos, ref.enclosing)
		}
	}
}

// ---------------------------------------------------------------------------
// The policy guard, exact rather than substring.
//
// The first cut asserted PRESENCE of good tokens with strings.Contains, which
// pins the policy only against SUBTRACTIVE edits. Mutation testing showed
// additive widening was free: appending `connect-src *` while default-src
// 'none' stayed passed every test, silently killing one of the two controls
// the policy leans on. So this parses the policy into directive -> sources and
// asserts EXACT source lists, plus a known-directive whitelist so an
// unrecognised directive FAILS instead of sliding in.
// ---------------------------------------------------------------------------

// parseCSP splits a policy into directive -> source list.
func parseCSP(policy string) map[string][]string {
	out := map[string][]string{}
	for _, part := range strings.Split(policy, ";") {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) == 0 {
			continue
		}
		out[strings.ToLower(fields[0])] = fields[1:]
	}
	return out
}

func assertSources(t *testing.T, got map[string][]string, directive string, want ...string) {
	t.Helper()
	have, ok := got[directive]
	if !ok {
		t.Errorf("CSP is missing the %q directive entirely; with default-src 'none' "+
			"an unnamed directive falls back to 'none' and silently blocks that resource type", directive)
		return
	}
	if len(have) != len(want) {
		t.Errorf("CSP %q = %v, want exactly %v", directive, have, want)
		return
	}
	for i := range want {
		if have[i] != want[i] {
			t.Errorf("CSP %q = %v, want exactly %v", directive, have, want)
			return
		}
	}
}

func TestDraftPreviewCSPIsExactlyThePolicyWeIntend(t *testing.T) {
	got := parseCSP(DraftPreviewCSP)

	// The known-directive whitelist. This is the part that makes the test
	// additive-safe: a directive nobody pinned cannot appear.
	known := map[string]bool{
		"sandbox": true, "default-src": true, "img-src": true, "style-src": true,
		"font-src": true, "script-src": true, "base-uri": true, "form-action": true,
		"frame-ancestors": true, "media-src": true, "frame-src": true,
	}
	for d := range got {
		if !known[d] {
			t.Errorf("unpinned directive %q appeared in DraftPreviewCSP. Every directive "+
				"must be asserted here, or widening the policy costs nothing and no test "+
				"notices. Full policy: %q", d, DraftPreviewCSP)
		}
	}

	// The opaque origin. Without this the document is same-origin with the
	// admin app and a credentialed fetch is fully authorized.
	assertSources(t, got, "sandbox", "allow-scripts")

	// The connect-src family must stay dead. connect-src EXPLICITLY overrides
	// the default-src fallback, so its mere presence with a permissive value
	// would kill one of the two controls while every substring check passed.
	assertSources(t, got, "default-src", "'none'")
	if src, ok := got["connect-src"]; ok {
		if len(src) != 1 || src[0] != "'none'" {
			t.Errorf("connect-src = %v. It overrides the default-src 'none' fallback, "+
				"and a sandboxed document that can still fetch is the escalation "+
				"this policy exists to close", src)
		}
	}

	// Load-bearing and previously unpinned: form-action 'none' is what stops a
	// form POST leaving the browser under this policy, and base-uri 'none'
	// stops a <base> rewrite of every relative URL in the document.
	assertSources(t, got, "form-action", "'none'")
	assertSources(t, got, "base-uri", "'none'")

	// The feature. script-src 'none' would contain the payload but also kill
	// the component animation the preview exists to show. Host sources here
	// would re-enable remote script loads.
	assertSources(t, got, "script-src", "'unsafe-inline'")

	// The regression directives. Absent, these fall back to default-src 'none'
	// and the preview silently blocks self-hosted video and every iframe.
	assertSources(t, got, "media-src", "'self'")
	assertSources(t, got, "frame-src", "'self'")

	// The rest, pinned so they cannot be widened either.
	assertSources(t, got, "img-src", "'self'", "data:", "https:")
	assertSources(t, got, "style-src", "'self'", "'unsafe-inline'")
	assertSources(t, got, "font-src", "'self'", "https:", "data:")
	assertSources(t, got, "frame-ancestors", "'self'")
}

// TestDraftPreviewCSPNeverGrantsSameOrigin is separate because it is the one
// mutation that would leave every other assertion intact while making the
// sandbox decorative.
func TestDraftPreviewCSPNeverGrantsSameOrigin(t *testing.T) {
	for _, policy := range []string{
		DraftPreviewCSP,
		draftPreviewCSP(builder.AllowedDomains{Frame: []string{"https://cal.com"}}),
	} {
		if strings.Contains(policy, "allow-same-origin") {
			t.Errorf("policy grants allow-same-origin, which hands the admin origin back "+
				"to a document containing agent-stored script: %q", policy)
		}
	}
}

// TestDraftPreviewCSPMirrorsTheTenantAllowlist locks the property that makes
// the preview truthful: the frame and media hosts come from the SAME
// allowlist the built site uses, so the preview cannot show an embed that
// production will block, or block one production allows.
func TestDraftPreviewCSPMirrorsTheTenantAllowlist(t *testing.T) {
	allow := builder.AllowedDomains{
		Frame: []string{"https://www.youtube-nocookie.com"},
		Media: []string{"https://cdn.example.com"},
	}
	got := parseCSP(draftPreviewCSP(allow))

	assertSources(t, got, "frame-src", "'self'", "https://www.youtube-nocookie.com")
	assertSources(t, got, "media-src", "'self'", "https://cdn.example.com")
}

// ---------------------------------------------------------------------------
// The behavioural guard: all three routes, over real HTTP, in one table.
// ---------------------------------------------------------------------------

func seedPreviewPage(t *testing.T, q *store.Queries, siteID, pageID, slug string) {
	t.Helper()
	seedSite(t, q, siteID)
	if err := q.CreatePage(context.Background(), store.CreatePageParams{
		ID:        pageID,
		SiteID:    siteID,
		Title:     "CSP Preview",
		Slug:      slug,
		Layout:    "default",
		SortOrder: 0,
		ShowInNav: 0,
	}); err != nil {
		t.Fatalf("create page: %v", err)
	}
}

func agentPreviewRouter(t *testing.T, sqlDB *sql.DB, q *store.Queries, siteID string) http.Handler {
	t.Helper()
	h := NewAgentHandler(&config.Config{}, q, sqlDB)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := context.WithValue(req.Context(), authmw.AgentContextKey, &authmw.AgentIdentity{
				KeyID:        "csp-preview-key",
				SiteID:       siteID,
				Capabilities: []string{"read", "write"},
			})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Get("/api/agent/preview", h.DraftPreview)
	return r
}

// TestAllThreeDraftPreviewRoutesSetTheSandboxCSP drives each of the three
// routes that serve RenderPageDraft output. All three are in ONE table on
// purpose: when the agent route lived in its own test, deleting that test
// would have left a test called "AllThree" passing on two.
func TestAllThreeDraftPreviewRoutesSetTheSandboxCSP(t *testing.T) {
	sqlDB, q := setupDeployTestDB(t)
	siteID := "cspprevsite00000000aaaa1"
	pageID := "cspprevpage00000000aaaa1"
	seedPreviewPage(t, q, siteID, pageID, "/csp-preview")

	cases := []struct {
		name string
		do   func(t *testing.T) *http.Response
	}{
		{"agent.go DraftPreview", func(t *testing.T) *http.Response {
			r := agentPreviewRouter(t, sqlDB, q, siteID)
			req := httptest.NewRequest(http.MethodGet, "/api/agent/preview?page=csp-preview", nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			return w.Result()
		}},
		{"pages.go PageDraftPreview", func(t *testing.T) *http.Response {
			h := NewPageHandler(&config.Config{}, q)
			r := chi.NewRouter()
			r.Get("/api/sites/{siteID}/pages/{pageID}/draft-preview", h.PageDraftPreview)
			req := httptest.NewRequest(http.MethodGet,
				"/api/sites/"+siteID+"/pages/"+pageID+"/draft-preview", nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			return w.Result()
		}},
		{"preview_token.go Serve", func(t *testing.T) *http.Response {
			m := NewPreviewTokenManager(q)
			tok, err := m.Mint(siteID, pageID)
			if err != nil {
				t.Fatalf("mint: %v", err)
			}
			req := httptest.NewRequest(http.MethodGet, "/_preview/"+tok, nil)
			req.RemoteAddr = "127.0.0.1:12345"
			w := httptest.NewRecorder()
			m.Serve(w, req)
			return w.Result()
		}},
	}

	if len(cases) != 3 {
		t.Fatalf("this test is named AllThree and covers %d", len(cases))
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := tc.do(t)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected 200, got %d", resp.StatusCode)
			}
			got := resp.Header.Get("Content-Security-Policy")
			if got == "" {
				t.Fatalf("no Content-Security-Policy on the response; this route serves " +
					"agent-authored component templates as a top-level document on the admin origin")
			}
			if got != DraftPreviewCSP {
				t.Errorf("CSP differs from the shared policy.\n got: %q\nwant: %q", got, DraftPreviewCSP)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The regression guard: blocks that emit media or an iframe must still load.
//
// Nothing asserted that the preview actually RENDERS, which is why
// default-src 'none' swallowing media-src and frame-src shipped. This drives
// the real route with real blocks and checks the response's OWN policy admits
// the URLs the response's OWN markup asks for.
// ---------------------------------------------------------------------------

var (
	videoSourceRE = regexp.MustCompile(`<source[^>]+src="([^"]+)"`)
	iframeSrcRE   = regexp.MustCompile(`<iframe[^>]+src="([^"]+)"`)
)

// cspPermits reports whether a policy's directive admits a URL. Deliberately
// small: it understands 'self' (any same-origin, i.e. root-relative URL) and
// exact scheme+host prefixes, which is all these directives use.
func cspPermits(sources []string, rawURL string) bool {
	for _, s := range sources {
		if s == "'self'" && strings.HasPrefix(rawURL, "/") {
			return true
		}
		if s == "*" {
			return true
		}
		if strings.HasPrefix(s, "http") && strings.HasPrefix(rawURL, s) {
			return true
		}
	}
	return false
}

func TestPreviewPolicyAdmitsEveryResourceThePreviewEmits(t *testing.T) {
	sqlDB, q := setupDeployTestDB(t)
	_ = sqlDB
	siteID := "cspmediasite000000aaaa11"
	pageID := "cspmediapage000000aaaa11"
	seedPreviewPage(t, q, siteID, pageID, "/csp-media")

	// The tenant approved this host for the BUILT site. The preview must
	// mirror that, or it lies about what production will do.
	const embedHost = "https://www.youtube-nocookie.com"
	if err := q.CreateAllowedScript(context.Background(), store.CreateAllowedScriptParams{
		ID: "allow-frame-yt", SiteID: siteID, Domain: embedHost,
		Purpose: "video embeds", Kind: "frame",
	}); err != nil {
		t.Fatalf("seed allowed script: %v", err)
	}

	// A self-hosted video hero (media-src) and a third-party embed (frame-src).
	blocks := []struct{ id, blockType, data string }{
		{"blk-videohero-1", "video_hero", `{"headline":"Watch","video_source":"self_hosted","video_id":"vid123"}`},
		{"blk-embed-1", "embed", `{"src":"` + embedHost + `/embed/abc123","title":"Demo"}`},
	}
	for i, b := range blocks {
		if err := q.CreateBlock(context.Background(), store.CreateBlockParams{
			ID: b.id, PageID: pageID, BlockType: b.blockType,
			DataJson: b.data, StyleJson: "{}", IsVisible: 1, SortOrder: int64(10 * (i + 1)),
		}); err != nil {
			t.Fatalf("create %s block: %v", b.blockType, err)
		}
	}

	h := NewPageHandler(&config.Config{}, q)
	r := chi.NewRouter()
	r.Get("/api/sites/{siteID}/pages/{pageID}/draft-preview", h.PageDraftPreview)
	req := httptest.NewRequest(http.MethodGet,
		"/api/sites/"+siteID+"/pages/"+pageID+"/draft-preview", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	resp := w.Result()
	defer resp.Body.Close()

	body := w.Body.String()
	policy := parseCSP(resp.Header.Get("Content-Security-Policy"))

	// The allowlist must have reached the header.
	if !cspPermits(policy["frame-src"], embedHost+"/embed/abc123") {
		t.Errorf("frame-src %v does not admit the tenant-approved embed host %q; "+
			"the preview allowlist is not wired to builder.LoadAllowedDomains",
			policy["frame-src"], embedHost)
	}

	checked := 0
	for _, m := range videoSourceRE.FindAllStringSubmatch(body, -1) {
		checked++
		if !cspPermits(policy["media-src"], m[1]) {
			t.Errorf("the preview emits <source src=%q> but its own media-src %v blocks it. "+
				"This is the default-src 'none' fallback swallowing an unnamed directive: "+
				"the built site falls back to default-src 'self' and plays this video.",
				m[1], policy["media-src"])
		}
	}
	for _, m := range iframeSrcRE.FindAllStringSubmatch(body, -1) {
		checked++
		if !cspPermits(policy["frame-src"], m[1]) {
			t.Errorf("the preview emits <iframe src=%q> but its own frame-src %v blocks it",
				m[1], policy["frame-src"])
		}
	}

	// Without this the test passes when the renderer emits nothing at all,
	// which is exactly how a "renders fine" test goes vacuous.
	if checked < 2 {
		t.Fatalf("expected at least one <source> and one <iframe> in the preview, checked %d. "+
			"Block rendering changed and this guard is no longer testing anything.\nbody: %s",
			checked, body)
	}
}

// TestPreviewMediaAndFrameMatchProductionFallback pins the specific asymmetry
// that caused the regression: the built site's default-src is 'self', so
// unnamed media-src/frame-src still permit same-origin. The preview's
// default-src is 'none', so they must be named explicitly to get the same
// behaviour.
func TestPreviewMediaAndFrameMatchProductionFallback(t *testing.T) {
	got := parseCSP(DraftPreviewCSP)
	for _, d := range []string{"media-src", "frame-src"} {
		if !cspPermits(got[d], "/media/vid123/original.mp4") {
			t.Errorf("%s = %v does not admit a same-origin URL. The built site falls back "+
				"to default-src 'self' here; the preview falls back to 'none', so this "+
				"directive must name 'self' explicitly or the preview silently omits content.",
				d, got[d])
		}
	}
	// Sorted output keeps the failure message stable across map iteration.
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("policy directives: %v", keys)
}
