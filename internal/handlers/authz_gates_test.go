package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/slab/internal/builder"
	"github.com/bright-interaction/slab/internal/config"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

// Regression tests for the 2026-08-11 audit authz gates: five handler
// surfaces that trusted the coarse membership middleware to be an
// authorization decision. Every test drives the handler as a PLAIN
// MEMBER, which is exactly the identity the middleware admits, and
// asserts the handler itself refuses.

// asUser wraps a router so every request carries an authenticated
// non-admin user. Role "user" is deliberate: a platform admin bypasses
// every per-site check, so a test using one proves nothing.
func asUser(r chi.Router, userID string) chi.Router {
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := context.WithValue(req.Context(), authmw.UserContextKey, &authmw.AuthUser{ID: userID, Role: "user"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	return r
}

// seedSiteMember creates the user row site_members points at and grants
// the role on the site.
func seedSiteMember(t *testing.T, q *store.Queries, siteID, userID, role string) {
	t.Helper()
	ctx := context.Background()
	if err := q.CreateUser(ctx, store.CreateUserParams{
		ID: userID, Email: userID + "@example.com", PasswordHash: "x", Name: userID, Role: "user",
	}); err != nil {
		t.Fatalf("create user %s: %v", userID, err)
	}
	if err := q.AddSiteMember(ctx, store.AddSiteMemberParams{SiteID: siteID, UserID: userID, Role: role}); err != nil {
		t.Fatalf("add site member %s: %v", userID, err)
	}
}

// doAuthzReq issues a request and returns the status plus the raw body,
// so assertions can look for a leaked field by substring.
func doAuthzReq(t *testing.T, r chi.Router, method, path string, body any) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	resp := w.Result()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// ---------------------------------------------------------------------
// Finding 6: workspace member reads a pending owner invite token.
// ---------------------------------------------------------------------

func TestListInvites_MemberDeniedAndTokenNeverSerialised(t *testing.T) {
	_, q := setupDeployTestDB(t)
	ctx := context.Background()

	const wsID = "ws-authz-1"
	if err := q.CreateWorkspace(ctx, store.CreateWorkspaceParams{
		ID: wsID, Name: "WS", Slug: "ws-authz-1", Plan: "studio",
		Region: "eu", BillingEmail: "b@example.com", Status: "active",
		TrialEndsAt: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	const ownerToken = "ffffdeadbeefowner0000token"
	if err := q.CreateWorkspaceInvite(ctx, store.CreateWorkspaceInviteParams{
		ID: "inv1", WorkspaceID: wsID, Email: "incoming-owner@example.com",
		Role: "owner", Token: ownerToken, CreatedBy: "u-owner",
		ExpiresAt: time.Now().Add(workspaceInviteTTL).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("create invite: %v", err)
	}

	h := NewWorkspaceHandler(&config.Config{BaseURL: "https://example.com"}, q)

	router := func(role string) chi.Router {
		r := asUser(chi.NewRouter(), "u-member")
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				c := context.WithValue(req.Context(), authmw.WorkspaceContextKey,
					&authmw.WorkspaceMembership{WorkspaceID: wsID, Role: role})
				next.ServeHTTP(w, req.WithContext(c))
			})
		})
		r.Get("/api/workspaces/{workspaceID}/invites", h.ListInvites)
		return r
	}

	// A plain member is refused outright.
	code, body := doAuthzReq(t, router("member"), http.MethodGet, "/api/workspaces/"+wsID+"/invites", nil)
	if code != http.StatusForbidden {
		t.Errorf("member ListInvites status=%d, want 403 (body=%s)", code, body)
	}
	if strings.Contains(body, ownerToken) {
		t.Errorf("member response leaked the invite token: %s", body)
	}

	// A workspace admin is allowed to see that an invite is pending, but
	// the redeemable token is still not on the list path: it is handed
	// out exactly once, in the CreateInvite response.
	code, body = doAuthzReq(t, router("admin"), http.MethodGet, "/api/workspaces/"+wsID+"/invites", nil)
	if code != http.StatusOK {
		t.Fatalf("admin ListInvites status=%d, want 200 (body=%s)", code, body)
	}
	if strings.Contains(body, ownerToken) {
		t.Errorf("admin list path still serialises the raw token: %s", body)
	}
	if !strings.Contains(body, "incoming-owner@example.com") {
		t.Errorf("admin list lost its useful fields: %s", body)
	}
}

// ---------------------------------------------------------------------
// Finding 7: any site member rewrites security.* settings.
// ---------------------------------------------------------------------

func settingsAuthzRouter(h *SettingsHandler, userID string) chi.Router {
	r := asUser(chi.NewRouter(), userID)
	r.Get("/api/sites/{siteID}/settings", h.List)
	r.Get("/api/sites/{siteID}/settings/{category}", h.ListByCategory)
	r.Put("/api/sites/{siteID}/settings", h.Upsert)
	r.Put("/api/sites/{siteID}/settings/bulk", h.BulkUpsert)
	r.Delete("/api/sites/{siteID}/settings/{settingID}", h.Delete)
	return r
}

func TestSettings_SecurityCategoryIsOwnerOnly(t *testing.T) {
	_, q := setupDeployTestDB(t)
	ctx := context.Background()
	const siteID = "site-authz-settings"
	seedSite(t, q, siteID)
	seedSiteMember(t, q, siteID, "u-member", "member")
	seedSiteMember(t, q, siteID, "u-owner", "owner")

	h := NewSettingsHandler(&config.Config{}, q)
	member := settingsAuthzRouter(h, "u-member")
	owner := settingsAuthzRouter(h, "u-owner")

	const openCSP = "default-src *; script-src * 'unsafe-inline' 'unsafe-eval'"
	cspBody := map[string]any{"category": "security", "key": "csp_policy", "value": openCSP}

	// The sharp key: csp_policy has no validator and overrides csp_enabled.
	code, body := doAuthzReq(t, member, http.MethodPut, "/api/sites/"+siteID+"/settings", cspBody)
	if code != http.StatusForbidden {
		t.Errorf("member Upsert security.csp_policy status=%d, want 403 (body=%s)", code, body)
	}
	if got, err := q.GetSetting(ctx, store.GetSettingParams{SiteID: siteID, Category: "security", Key: "csp_policy"}); err == nil && got.Value != "" {
		t.Errorf("member write reached the DB: csp_policy=%q", got.Value)
	}

	// Same key, site owner: allowed, so the gate is authorization and not
	// a blanket refusal that would break the security settings page.
	if code, body = doAuthzReq(t, owner, http.MethodPut, "/api/sites/"+siteID+"/settings", cspBody); code != http.StatusOK {
		t.Errorf("owner Upsert security.csp_policy status=%d, want 200 (body=%s)", code, body)
	}

	// Ordinary categories stay open to members.
	seoBody := map[string]any{"category": "seo", "key": "meta_description", "value": "hello"}
	if code, body = doAuthzReq(t, member, http.MethodPut, "/api/sites/"+siteID+"/settings", seoBody); code != http.StatusOK {
		t.Errorf("member Upsert seo status=%d, want 200 (body=%s)", code, body)
	}

	// Bulk is the twin path. Shape: the privileged entry is dropped and
	// named back in rejected_admin_only, the caller's own permitted entry
	// still lands. Same contract as handlers/agent_settings.go
	// BulkUpsertSettings and the MCP bulk_upsert_settings tool, so one
	// policy has one shape on all three bulk surfaces.
	bulk := []map[string]any{
		{"category": "seo", "key": "og_title", "value": "ordinary"},
		{"category": "security", "key": "csp_enabled", "value": "false"},
	}
	if code, body = doAuthzReq(t, member, http.MethodPut, "/api/sites/"+siteID+"/settings/bulk", bulk); code != http.StatusOK {
		t.Errorf("member BulkUpsert status=%d, want 200 (body=%s)", code, body)
	}
	if !strings.Contains(body, "security.csp_enabled") {
		t.Errorf("bulk response did not name the rejected key: %s", body)
	}
	if got, err := q.GetSetting(ctx, store.GetSettingParams{SiteID: siteID, Category: "seo", Key: "og_title"}); err != nil || got.Value != "ordinary" {
		t.Errorf("permitted entry in the batch did not land: %+v (err=%v)", got, err)
	}
	if got, err := q.GetSetting(ctx, store.GetSettingParams{SiteID: siteID, Category: "security", Key: "csp_enabled"}); err == nil && got.Value != "" {
		t.Errorf("member disabled CSP through the bulk path: csp_enabled=%q", got.Value)
	}

	// Delete is the other write into the category: dropping a hardening
	// row must need the same role as setting one.
	row, err := q.GetSetting(ctx, store.GetSettingParams{SiteID: siteID, Category: "security", Key: "csp_policy"})
	if err != nil {
		t.Fatalf("owner-written row missing: %v", err)
	}
	if code, body = doAuthzReq(t, member, http.MethodDelete, "/api/sites/"+siteID+"/settings/"+row.ID, nil); code != http.StatusForbidden {
		t.Errorf("member Delete of a security row status=%d, want 403 (body=%s)", code, body)
	}
	if _, err := q.GetSetting(ctx, store.GetSettingParams{SiteID: siteID, Category: "security", Key: "csp_policy"}); err != nil {
		t.Errorf("member deleted the security row anyway: %v", err)
	}
}

// ---------------------------------------------------------------------
// P2 gate: deploy target reads.
// ---------------------------------------------------------------------

func TestDeployTargets_ReadsAreOwnerOnly(t *testing.T) {
	sqlDB, q := setupDeployTestDB(t)
	ctx := context.Background()
	const siteID = "site-authz-deploy"
	seedSite(t, q, siteID)
	seedSiteMember(t, q, siteID, "u-member", "member")
	const apiKey = "SUPER_SECRET_DOCKYARD_KEY"
	if err := q.CreateDeployTarget(ctx, store.CreateDeployTargetParams{
		ID: "tgt1", SiteID: siteID, Name: "dy", Kind: "dockyard",
		ConfigJson: `{"api_key":"` + apiKey + `"}`, IsDefault: 0,
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}

	h := NewDeployHandler(&config.Config{}, q, sqlDB)
	r := asUser(chi.NewRouter(), "u-member")
	r.Get("/api/sites/{siteID}/deploy-targets", h.ListTargets)
	r.Get("/api/sites/{siteID}/deploy-targets/{targetID}", h.GetTarget)

	for _, path := range []string{
		"/api/sites/" + siteID + "/deploy-targets",
		"/api/sites/" + siteID + "/deploy-targets/tgt1",
	} {
		code, body := doAuthzReq(t, r, http.MethodGet, path, nil)
		if code != http.StatusForbidden {
			t.Errorf("member GET %s status=%d, want 403 (body=%s)", path, code, body)
		}
		if strings.Contains(body, apiKey) {
			t.Errorf("member GET %s leaked the deploy credential: %s", path, body)
		}
	}
}

// ---------------------------------------------------------------------
// P2 gate: domain mutations.
// ---------------------------------------------------------------------

func TestDomains_MutationsAreOwnerOnly(t *testing.T) {
	_, q := setupDeployTestDB(t)
	ctx := context.Background()
	const siteID = "site-authz-domains"
	seedSite(t, q, siteID)
	seedSiteMember(t, q, siteID, "u-member", "member")
	if err := q.CreateDomain(ctx, store.CreateDomainParams{
		ID: "dom1", SiteID: siteID, Hostname: "live.example.com",
		VerifyToken: "vt", IsCanonical: 1,
	}); err != nil {
		t.Fatalf("create domain: %v", err)
	}

	h := NewDomainHandler(q, func() {}, "")
	r := asUser(chi.NewRouter(), "u-member")
	r.Post("/api/sites/{siteID}/domains", h.Create)
	r.Post("/api/sites/{siteID}/domains/{domainID}/canonical", h.SetCanonical)
	r.Post("/api/sites/{siteID}/domains/{domainID}/refresh", h.Refresh)

	cases := []struct {
		path string
		body any
	}{
		{"/api/sites/" + siteID + "/domains", map[string]any{"hostname": "evil.example.com"}},
		{"/api/sites/" + siteID + "/domains/dom1/canonical", nil},
		{"/api/sites/" + siteID + "/domains/dom1/refresh", nil},
	}
	for _, c := range cases {
		code, body := doAuthzReq(t, r, http.MethodPost, c.path, c.body)
		if code != http.StatusForbidden {
			t.Errorf("member POST %s status=%d, want 403 (body=%s)", c.path, code, body)
		}
	}
	rows, err := q.ListDomainsBySite(ctx, siteID)
	if err != nil {
		t.Fatalf("list domains: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("member attached a hostname anyway: %d rows", len(rows))
	}
}

// ---------------------------------------------------------------------
// P2 gate: read-only agent key triggers a build (and a republish).
// ---------------------------------------------------------------------

func TestTriggerBuild_ReadOnlyAgentKeyDenied(t *testing.T) {
	_, q := setupDeployTestDB(t)
	ctx := context.Background()
	const siteID = "site-authz-build"
	seedSite(t, q, siteID)

	h := NewBuildHandler(&config.Config{DataDir: t.TempDir()}, q, nil)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			c := context.WithValue(req.Context(), authmw.AgentContextKey, &authmw.AgentIdentity{
				KeyID: "k1", SiteID: siteID, Name: "readonly", Capabilities: []string{"read"},
			})
			next.ServeHTTP(w, req.WithContext(c))
		})
	})
	r.Post("/api/agent/build", h.TriggerBuild)

	code, body := doAuthzReq(t, r, http.MethodPost, "/api/agent/build", nil)
	if code != http.StatusForbidden {
		t.Fatalf("read-only agent key TriggerBuild status=%d, want 403 (body=%s)", code, body)
	}
	rows, err := q.ListDeploymentsBySite(ctx, siteID)
	if err != nil {
		t.Fatalf("list deployments: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("denied build still created %d deployment row(s)", len(rows))
	}
}

// ---------------------------------------------------------------------
// Blocker 3.1: the canonical-hostname gate on DomainHandler.SetCanonical
// is routed around by PATCH /api/sites/{id}, which takes a "domain"
// field that builder.CanonicalHostname ranks ABOVE the is_canonical row.
//
// This test asserts the ROUTE-AROUND is closed, not that a gate exists:
// it drives the repoint through the sites route and reads the answer back
// out of builder.CanonicalHostname, the function every build step uses.
// ---------------------------------------------------------------------

func TestSites_MemberCannotRepointCanonicalHostnameViaSitePatch(t *testing.T) {
	sqlDB, q := setupDeployTestDB(t)
	ctx := context.Background()
	const siteID = "site-authz-canonical"
	seedSite(t, q, siteID)
	seedSiteMember(t, q, siteID, "u-member", "member")
	seedSiteMember(t, q, siteID, "u-owner", "owner")

	// The owner's canonical hostname, in the state SetCanonical leaves it.
	if err := q.CreateDomain(ctx, store.CreateDomainParams{
		ID: "dom-live", SiteID: siteID, Hostname: "shop.example.com",
		VerifyToken: "vt", IsCanonical: 1,
	}); err != nil {
		t.Fatalf("create domain: %v", err)
	}
	if err := q.UpdateDomainStatus(ctx, store.UpdateDomainStatusParams{
		Status: "live", LastError: "", ID: "dom-live",
	}); err != nil {
		t.Fatalf("set domain live: %v", err)
	}

	canonical := func() string {
		site, err := q.GetSiteByID(ctx, siteID)
		if err != nil {
			t.Fatalf("get site: %v", err)
		}
		return builder.CanonicalHostname(ctx, q, site)
	}
	if got := canonical(); got != "shop.example.com" {
		t.Fatalf("baseline canonical=%q, want shop.example.com", got)
	}

	h := NewSiteHandler(&config.Config{}, q, sqlDB)
	siteRouter := func(userID string) chi.Router {
		r := asUser(chi.NewRouter(), userID)
		r.Patch("/api/sites/{siteID}", h.Update)
		return r
	}
	member, owner := siteRouter("u-member"), siteRouter("u-owner")

	// The attack: a plain member repoints the live canonical hostname at
	// a host they control, without touching the domains table.
	code, body := doAuthzReq(t, member, http.MethodPatch, "/api/sites/"+siteID,
		map[string]any{"domain": "attacker.example.com"})
	if code != http.StatusForbidden {
		t.Errorf("member PATCH domain status=%d, want 403 (body=%s)", code, body)
	}
	if got := canonical(); got != "shop.example.com" {
		t.Errorf("member repointed the canonical hostname to %q", got)
	}

	// Whitespace is not a workaround: CanonicalHostname trims before it
	// compares, so " shop.example.com " would have been accepted as an
	// unchanged value and still changed nothing, while "attacker.example.com "
	// would repoint. The gate compares the raw stored value.
	code, body = doAuthzReq(t, member, http.MethodPatch, "/api/sites/"+siteID,
		map[string]any{"domain": " attacker.example.com "})
	if code != http.StatusForbidden {
		t.Errorf("member PATCH padded domain status=%d, want 403 (body=%s)", code, body)
	}
	if got := canonical(); got != "shop.example.com" {
		t.Errorf("member repointed the canonical hostname to %q via a padded value", got)
	}

	// The gate is on the FIELD: the same member still edits the site.
	if code, body = doAuthzReq(t, member, http.MethodPatch, "/api/sites/"+siteID,
		map[string]any{"name": "Renamed By Member"}); code != http.StatusOK {
		t.Errorf("member PATCH name status=%d, want 200 (body=%s)", code, body)
	}
	if site, err := q.GetSiteByID(ctx, siteID); err != nil || site.Name != "Renamed By Member" {
		t.Errorf("member lost a legitimate field write: %+v (err=%v)", site, err)
	}

	// And the owner still has the capability, which is also the proof
	// that site.Domain really does outrank the is_canonical row: one
	// PATCH moves the canonical off the live domains row entirely.
	if code, body = doAuthzReq(t, owner, http.MethodPatch, "/api/sites/"+siteID,
		map[string]any{"domain": "newprimary.example.com"}); code != http.StatusOK {
		t.Errorf("owner PATCH domain status=%d, want 200 (body=%s)", code, body)
	}
	if got := canonical(); got != "newprimary.example.com" {
		t.Errorf("owner repoint did not take effect: canonical=%q", got)
	}
}

// ---------------------------------------------------------------------
// Blocker 3.2: payments was absent from privilegedCategories, and the
// human REST read path handed the live Mollie key to any member. The key
// is bearer-equivalent, so READ is the sharper half: a member who can
// only read it can already move the tenant's money.
// ---------------------------------------------------------------------

func TestSettings_PaymentsSecretIsUnreadableAndUnwritableByMembers(t *testing.T) {
	_, q := setupDeployTestDB(t)
	ctx := context.Background()
	const siteID = "site-authz-payments"
	const liveKey = "live_MOLLIEKEYTHATMOVESREALMONEY"
	const crmSecret = "crm_shared_secret_value"
	seedSite(t, q, siteID)
	seedSiteMember(t, q, siteID, "u-member", "member")
	seedSiteMember(t, q, siteID, "u-owner", "owner")

	for _, s := range []struct{ id, category, key, value string }{
		{"set-mollie", "payments", "mollie_api_key", liveKey},
		{"set-mode", "payments", "mollie_test_mode", "false"},
		{"set-crm", "analytics", "crm_webhook_secret", crmSecret},
		{"set-seo", "seo", "meta_description", "public copy"},
	} {
		if err := q.UpsertSetting(ctx, store.UpsertSettingParams{
			ID: s.id, SiteID: siteID, Category: s.category, Key: s.key, Value: s.value,
		}); err != nil {
			t.Fatalf("seed setting %s: %v", s.id, err)
		}
	}

	h := NewSettingsHandler(&config.Config{}, q)
	member := settingsAuthzRouter(h, "u-member")
	owner := settingsAuthzRouter(h, "u-owner")

	settingValue := func(category, key string) string {
		row, err := q.GetSetting(ctx, store.GetSettingParams{SiteID: siteID, Category: category, Key: key})
		if err != nil {
			return ""
		}
		return row.Value
	}

	// READ, every path a member can reach.
	for _, path := range []string{
		"/api/sites/" + siteID + "/settings",
		"/api/sites/" + siteID + "/settings/payments",
		"/api/sites/" + siteID + "/settings/analytics",
	} {
		code, body := doAuthzReq(t, member, http.MethodGet, path, nil)
		if code != http.StatusOK {
			t.Errorf("member GET %s status=%d, want 200 (body=%s)", path, code, body)
		}
		if strings.Contains(body, liveKey) {
			t.Errorf("member GET %s leaked the Mollie key: %s", path, body)
		}
		if strings.Contains(body, crmSecret) {
			t.Errorf("member GET %s leaked the CRM webhook secret: %s", path, body)
		}
	}
	// Non-secret rows in the same categories still reach the member, so
	// this is a value filter and not a category blackout.
	if _, body := doAuthzReq(t, member, http.MethodGet, "/api/sites/"+siteID+"/settings/payments", nil); !strings.Contains(body, "mollie_test_mode") {
		t.Errorf("member lost the non-secret payments rows: %s", body)
	}

	// The owner still reads the raw value: the admin Payments page needs
	// it to render the field and detect the live_/test_ prefix.
	if _, body := doAuthzReq(t, owner, http.MethodGet, "/api/sites/"+siteID+"/settings/payments", nil); !strings.Contains(body, liveKey) {
		t.Errorf("owner lost access to their own Mollie key: %s", body)
	}

	// WRITE, single key.
	code, body := doAuthzReq(t, member, http.MethodPut, "/api/sites/"+siteID+"/settings",
		map[string]any{"category": "payments", "key": "mollie_api_key", "value": "live_ATTACKERACCOUNT"})
	if code != http.StatusForbidden {
		t.Errorf("member Upsert payments.mollie_api_key status=%d, want 403 (body=%s)", code, body)
	}
	if got := settingValue("payments", "mollie_api_key"); got != liveKey {
		t.Errorf("member rewrote the Mollie key to %q", got)
	}

	// WRITE, bulk. The permitted entry lands, the two secrets do not,
	// and both are named back. The empty crm_webhook_secret is the exact
	// shape the analytics settings page sends after loading a filtered
	// list: refusing it is what stops a hidden field from being blanked
	// by the member's next save.
	code, body = doAuthzReq(t, member, http.MethodPut, "/api/sites/"+siteID+"/settings/bulk", []map[string]any{
		{"category": "seo", "key": "meta_description", "value": "edited by member"},
		{"category": "payments", "key": "mollie_api_key", "value": "live_ATTACKERACCOUNT"},
		{"category": "analytics", "key": "crm_webhook_secret", "value": ""},
	})
	if code != http.StatusOK {
		t.Errorf("member BulkUpsert status=%d, want 200 (body=%s)", code, body)
	}
	if strings.Contains(body, liveKey) {
		t.Errorf("bulk echo-back leaked the Mollie key to a member: %s", body)
	}
	for _, want := range []string{"payments.mollie_api_key", "analytics.crm_webhook_secret"} {
		if !strings.Contains(body, want) {
			t.Errorf("bulk response did not name %s as rejected: %s", want, body)
		}
	}
	if got := settingValue("payments", "mollie_api_key"); got != liveKey {
		t.Errorf("member rewrote the Mollie key through the bulk path: %q", got)
	}
	if got := settingValue("analytics", "crm_webhook_secret"); got != crmSecret {
		t.Errorf("member blanked the CRM webhook secret through the bulk path: %q", got)
	}
	if got := settingValue("seo", "meta_description"); got != "edited by member" {
		t.Errorf("permitted entry in the batch did not land: %q", got)
	}

	// DELETE is the third write into the key.
	if code, body = doAuthzReq(t, member, http.MethodDelete, "/api/sites/"+siteID+"/settings/set-mollie", nil); code != http.StatusForbidden {
		t.Errorf("member Delete of the Mollie key status=%d, want 403 (body=%s)", code, body)
	}
	if got := settingValue("payments", "mollie_api_key"); got != liveKey {
		t.Errorf("member deleted the Mollie key: %q", got)
	}

	// The owner keeps the whole surface: authorization, not a blackout.
	if code, body = doAuthzReq(t, owner, http.MethodPut, "/api/sites/"+siteID+"/settings",
		map[string]any{"category": "payments", "key": "mollie_api_key", "value": "live_OWNERROTATEDKEY"}); code != http.StatusOK {
		t.Errorf("owner Upsert payments.mollie_api_key status=%d, want 200 (body=%s)", code, body)
	}
	if got := settingValue("payments", "mollie_api_key"); got != "live_OWNERROTATEDKEY" {
		t.Errorf("owner could not rotate their own key: %q", got)
	}
}
