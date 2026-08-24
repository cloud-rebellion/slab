package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/slab/internal/config"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

// Agency mode (2026-08-24) put the already-built workspace layer behind a
// UI. These tests pin the authz rules that had to be tightened first,
// because a "Create workspace" button in front of every authenticated
// user turns each of them from theory into one click.
//
// Every test drives the handler as a NON-admin unless the rule under test
// is specifically about admins: a platform admin clears RequireWorkspaceRole
// unconditionally (workspace_access.go), so a test written with one proves
// nothing about the gate.

// asRoleUser is asUser with a caller-chosen platform role.
func asRoleUser(r chi.Router, userID, role string) chi.Router {
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := context.WithValue(req.Context(), authmw.UserContextKey, &authmw.AuthUser{ID: userID, Role: role})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	return r
}

// withWorkspaceRole injects a resolved workspace membership, standing in
// for WorkspaceAccessMiddleware.
func withWorkspaceRole(r chi.Router, workspaceID, role string) chi.Router {
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			c := context.WithValue(req.Context(), authmw.WorkspaceContextKey,
				&authmw.WorkspaceMembership{WorkspaceID: workspaceID, Role: role})
			next.ServeHTTP(w, req.WithContext(c))
		})
	})
	return r
}

func seedWorkspace(t *testing.T, q *store.Queries, id, slug, plan string) {
	t.Helper()
	if err := q.CreateWorkspace(context.Background(), store.CreateWorkspaceParams{
		ID: id, Name: "WS " + slug, Slug: slug, Plan: plan,
		Region: "eu", BillingEmail: "billing@example.com", Status: "active",
	}); err != nil {
		t.Fatalf("create workspace %s: %v", id, err)
	}
}

func seedWorkspaceUser(t *testing.T, q *store.Queries, userID string) {
	t.Helper()
	if err := q.CreateUser(context.Background(), store.CreateUserParams{
		ID: userID, Email: userID + "@example.com", PasswordHash: "x", Name: userID, Role: "editor",
	}); err != nil {
		t.Fatalf("create user %s: %v", userID, err)
	}
}

// ---------------------------------------------------------------------
// Workspace creation: who may create, and who may name a plan.
// ---------------------------------------------------------------------

func createWorkspaceRouter(h *WorkspaceHandler, userID, role string) chi.Router {
	r := asRoleUser(chi.NewRouter(), userID, role)
	r.Post("/api/workspaces", h.Create)
	return r
}

func TestCreateWorkspace_NonAdminDeniedOnSelfHostedInstance(t *testing.T) {
	_, q := setupDeployTestDB(t)
	seedWorkspaceUser(t, q, "u-editor")
	h := NewWorkspaceHandler(&config.Config{DeploymentMode: "single"}, q)

	code, body := doAuthzReq(t, createWorkspaceRouter(h, "u-editor", "editor"),
		http.MethodPost, "/api/workspaces", map[string]any{"name": "Mine"})
	if code != http.StatusForbidden {
		t.Fatalf("non-admin create on a single-deployment install: status=%d, want 403 (body=%s)", code, body)
	}
}

func TestCreateWorkspace_NonAdminCannotSelfGrantAPlanInCloudMode(t *testing.T) {
	_, q := setupDeployTestDB(t)
	seedWorkspaceUser(t, q, "u-cloud")
	h := NewWorkspaceHandler(&config.Config{DeploymentMode: config.DeploymentModeCloud}, q)

	// Cloud self-serve signup IS a workspace create, so this must succeed ,
	// but the caller does not get to pick the tier it lands on.
	code, body := doAuthzReq(t, createWorkspaceRouter(h, "u-cloud", "editor"),
		http.MethodPost, "/api/workspaces", map[string]any{"name": "Cloudy", "plan": "agency"})
	if code != http.StatusForbidden {
		t.Fatalf("non-admin naming a plan: status=%d, want 403 (body=%s)", code, body)
	}

	code, body = doAuthzReq(t, createWorkspaceRouter(h, "u-cloud", "editor"),
		http.MethodPost, "/api/workspaces", map[string]any{"name": "Cloudy"})
	if code != http.StatusCreated {
		t.Fatalf("cloud self-serve create: status=%d, want 201 (body=%s)", code, body)
	}
	var view map[string]any
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view["plan"] != cloudSignupWorkspacePlan {
		t.Errorf("cloud signup plan = %v, want %q", view["plan"], cloudSignupWorkspacePlan)
	}
	if view["trial_ends_at"] == "" {
		t.Error("cloud signup workspace should carry a trial end date")
	}
}

func TestCreateWorkspace_AdminPlanMustExist(t *testing.T) {
	_, q := setupDeployTestDB(t)
	seedWorkspaceUser(t, q, "u-admin")
	h := NewWorkspaceHandler(&config.Config{DeploymentMode: "single"}, q)

	// An unrecognised key used to be accepted, and billing.Lookup falls
	// back to oss = UNLIMITED: so a typo granted more than the most
	// expensive real plan.
	code, body := doAuthzReq(t, createWorkspaceRouter(h, "u-admin", "admin"),
		http.MethodPost, "/api/workspaces", map[string]any{"name": "Typo", "plan": "enterprise"})
	if code != http.StatusBadRequest {
		t.Fatalf("unknown plan: status=%d, want 400 (body=%s)", code, body)
	}

	// A real plan from an admin still works: tests/e2e/specs/admin-api/
	// workspaces.spec.ts depends on exactly this.
	code, body = doAuthzReq(t, createWorkspaceRouter(h, "u-admin", "admin"),
		http.MethodPost, "/api/workspaces", map[string]any{"name": "Studio", "plan": "studio"})
	if code != http.StatusCreated {
		t.Fatalf("admin choosing a real plan: status=%d, want 201 (body=%s)", code, body)
	}
	if !strings.Contains(body, `"plan":"studio"`) {
		t.Errorf("admin plan choice was not honoured: %s", body)
	}
}

// ---------------------------------------------------------------------
// PATCH is a patch, not a replace.
// ---------------------------------------------------------------------

func TestUpdateWorkspace_OmittedBillingEmailIsPreserved(t *testing.T) {
	_, q := setupDeployTestDB(t)
	const wsID = "ws-patch-0000000000000001"
	seedWorkspace(t, q, wsID, "ws-patch", "studio")
	seedWorkspaceUser(t, q, "u-owner")
	h := NewWorkspaceHandler(&config.Config{}, q)

	r := withWorkspaceRole(asRoleUser(chi.NewRouter(), "u-owner", "editor"), wsID, "owner")
	r.Patch("/api/workspaces/{workspaceID}", h.Update)

	// The settings form sends only the field it changed. Before this, the
	// value-typed struct wrote "" over billing_email on every rename.
	code, body := doAuthzReq(t, r, http.MethodPatch, "/api/workspaces/"+wsID,
		map[string]any{"name": "Renamed"})
	if code != http.StatusOK {
		t.Fatalf("patch name: status=%d, want 200 (body=%s)", code, body)
	}
	row, err := q.GetWorkspaceByID(context.Background(), wsID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if row.Name != "Renamed" {
		t.Errorf("name = %q, want Renamed", row.Name)
	}
	if row.BillingEmail != "billing@example.com" {
		t.Errorf("billing_email = %q, want it preserved", row.BillingEmail)
	}

	// An explicit empty string still clears it.
	empty := ""
	code, body = doAuthzReq(t, r, http.MethodPatch, "/api/workspaces/"+wsID,
		map[string]any{"name": "Renamed", "billing_email": empty})
	if code != http.StatusOK {
		t.Fatalf("patch clearing email: status=%d, want 200 (body=%s)", code, body)
	}
	row, _ = q.GetWorkspaceByID(context.Background(), wsID)
	if row.BillingEmail != "" {
		t.Errorf("billing_email = %q, want cleared", row.BillingEmail)
	}
}

// ---------------------------------------------------------------------
// Role escalation is monotone.
// ---------------------------------------------------------------------

func TestCreateInvite_WorkspaceAdminCannotMintAnOwnerInvite(t *testing.T) {
	_, q := setupDeployTestDB(t)
	const wsID = "ws-inv-00000000000000000001"
	seedWorkspace(t, q, wsID, "ws-inv", "studio")
	seedWorkspaceUser(t, q, "u-wsadmin")
	h := NewWorkspaceHandler(&config.Config{BaseURL: "https://example.com"}, q)

	router := func(role string) chi.Router {
		r := withWorkspaceRole(asRoleUser(chi.NewRouter(), "u-wsadmin", "editor"), wsID, role)
		r.Post("/api/workspaces/{workspaceID}/invites", h.CreateInvite)
		return r
	}

	// CreateInvite returns the raw token, so minting an owner invite is
	// self-promotion for whoever holds the response.
	code, body := doAuthzReq(t, router("admin"), http.MethodPost, "/api/workspaces/"+wsID+"/invites",
		map[string]any{"email": "x@example.com", "role": "owner"})
	if code != http.StatusForbidden {
		t.Fatalf("admin minting an owner invite: status=%d, want 403 (body=%s)", code, body)
	}

	// The same admin may still invite at or below their own level.
	code, body = doAuthzReq(t, router("admin"), http.MethodPost, "/api/workspaces/"+wsID+"/invites",
		map[string]any{"email": "y@example.com", "role": "member"})
	if code != http.StatusCreated {
		t.Fatalf("admin minting a member invite: status=%d, want 201 (body=%s)", code, body)
	}

	// An owner may.
	code, body = doAuthzReq(t, router("owner"), http.MethodPost, "/api/workspaces/"+wsID+"/invites",
		map[string]any{"email": "z@example.com", "role": "owner"})
	if code != http.StatusCreated {
		t.Fatalf("owner minting an owner invite: status=%d, want 201 (body=%s)", code, body)
	}
}

// ---------------------------------------------------------------------
// A workspace always keeps at least one owner, and PATCH cannot create
// a membership.
// ---------------------------------------------------------------------

func TestUpdateMemberRole_GuardsLastOwnerAndNonMembers(t *testing.T) {
	_, q := setupDeployTestDB(t)
	ctx := context.Background()
	const wsID = "ws-role-0000000000000001"
	seedWorkspace(t, q, wsID, "ws-role", "studio")
	seedWorkspaceUser(t, q, "u-solo-owner")
	if err := q.AddWorkspaceMember(ctx, store.AddWorkspaceMemberParams{
		WorkspaceID: wsID, UserID: "u-solo-owner", Role: "owner",
	}); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	h := NewWorkspaceHandler(&config.Config{}, q)
	r := withWorkspaceRole(asRoleUser(chi.NewRouter(), "u-solo-owner", "editor"), wsID, "owner")
	r.Patch("/api/workspaces/{workspaceID}/members/{userID}", h.UpdateMemberRole)

	code, body := doAuthzReq(t, r, http.MethodPatch, "/api/workspaces/"+wsID+"/members/u-solo-owner",
		map[string]any{"role": "member"})
	if code != http.StatusConflict {
		t.Fatalf("demoting the last owner: status=%d, want 409 (body=%s)", code, body)
	}

	// PATCH must not double as AddMember: the target has no membership row.
	seedWorkspaceUser(t, q, "u-stranger")
	code, body = doAuthzReq(t, r, http.MethodPatch, "/api/workspaces/"+wsID+"/members/u-stranger",
		map[string]any{"role": "admin"})
	if code != http.StatusNotFound {
		t.Fatalf("patching a non-member: status=%d, want 404 (body=%s)", code, body)
	}
	if _, err := q.GetWorkspaceMembership(ctx, store.GetWorkspaceMembershipParams{
		WorkspaceID: wsID, UserID: "u-stranger",
	}); err == nil {
		t.Error("PATCH created a membership row it should have refused")
	}

	// With a second owner present the demotion is allowed.
	seedWorkspaceUser(t, q, "u-second-owner")
	if err := q.AddWorkspaceMember(ctx, store.AddWorkspaceMemberParams{
		WorkspaceID: wsID, UserID: "u-second-owner", Role: "owner",
	}); err != nil {
		t.Fatalf("seed second owner: %v", err)
	}
	code, body = doAuthzReq(t, r, http.MethodPatch, "/api/workspaces/"+wsID+"/members/u-second-owner",
		map[string]any{"role": "member"})
	if code != http.StatusOK {
		t.Fatalf("demoting a non-last owner: status=%d, want 200 (body=%s)", code, body)
	}
}

// ---------------------------------------------------------------------
// The site list must agree with the site gate.
// ---------------------------------------------------------------------

func TestSiteList_ShowsWorkspaceSitesAndOnlyThose(t *testing.T) {
	db, q := setupDeployTestDB(t)
	ctx := context.Background()

	const mineWS = "ws-mine-0000000000000001"
	const otherWS = "ws-other-000000000000001"
	seedWorkspace(t, q, mineWS, "ws-mine", "oss")
	seedWorkspace(t, q, otherWS, "ws-other", "oss")
	seedWorkspaceUser(t, q, "u-member")
	if err := q.AddWorkspaceMember(ctx, store.AddWorkspaceMemberParams{
		WorkspaceID: mineWS, UserID: "u-member", Role: "member",
	}); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	mk := func(id, slug, ws string) {
		if err := q.CreateSite(ctx, store.CreateSiteParams{
			ID: id, WorkspaceID: ws, Name: slug, Slug: slug,
			PrimaryColor: "#000000", SecondaryColor: "#000000",
			BgColor: "#ffffff", TextColor: "#000000",
			FontHeading: "Inter", FontBody: "Inter", Lang: "en",
		}); err != nil {
			t.Fatalf("create site %s: %v", slug, err)
		}
	}
	mk("site-mine-00000000000001", "mine", mineWS)
	mk("site-other-0000000000001", "other", otherWS)
	// A legacy row that predates the workspace backfill. The empty
	// workspace_id must not join to anybody.
	mk("site-legacy-000000000001", "legacy", "")

	h := NewSiteHandler(&config.Config{}, q, db)
	r := asRoleUser(chi.NewRouter(), "u-member", "editor")
	r.Get("/api/sites", h.List)

	code, body := doAuthzReq(t, r, http.MethodGet, "/api/sites", nil)
	if code != http.StatusOK {
		t.Fatalf("list: status=%d, want 200 (body=%s)", code, body)
	}
	// The headline fix: membership of the workspace is enough, with no
	// site_members row anywhere.
	if !strings.Contains(body, `"slug":"mine"`) {
		t.Errorf("workspace member cannot see their own workspace's site: %s", body)
	}
	// The leak test.
	if strings.Contains(body, `"slug":"other"`) {
		t.Errorf("list leaked another workspace's site: %s", body)
	}
	if strings.Contains(body, `"slug":"legacy"`) {
		t.Errorf("list leaked an un-backfilled legacy site: %s", body)
	}
}

// ---------------------------------------------------------------------
// Slugs are validated on every write path, and structural names are
// reserved so the local-deploy scope check cannot be spoofed.
// ---------------------------------------------------------------------

func TestValidateSiteSlug(t *testing.T) {
	for _, bad := range []string{"", "../evil", "Has Caps", "under_score", "dist", "srv", "atomicsite", strings.Repeat("a", 51)} {
		if err := validateSiteSlug(bad); err == nil {
			t.Errorf("validateSiteSlug(%q) = nil, want an error", bad)
		}
	}
	for _, good := range []string{"acme", "acme-co", "demo-1700000000", "a1"} {
		if err := validateSiteSlug(good); err != nil {
			t.Errorf("validateSiteSlug(%q) = %v, want nil", good, err)
		}
	}
}

func TestCreateSite_RejectsReservedAndMalformedSlug(t *testing.T) {
	db, q := setupDeployTestDB(t)
	const wsID = "ws-slug-0000000000000001"
	seedWorkspace(t, q, wsID, "ws-slug", "oss")
	seedWorkspaceUser(t, q, "u-builder")
	if err := q.AddWorkspaceMember(context.Background(), store.AddWorkspaceMemberParams{
		WorkspaceID: wsID, UserID: "u-builder", Role: "member",
	}); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	h := NewSiteHandler(&config.Config{}, q, db)
	r := asRoleUser(chi.NewRouter(), "u-builder", "editor")
	r.Post("/api/sites", h.Create)

	// POST /api/sites skipped slug validation entirely; seed and PATCH
	// enforced it, so the rule was bypassable by picking the other route.
	for _, slug := range []string{"dist", "../../etc", "srv", "atomicsite", strings.Repeat("a", 51)} {
		code, body := doAuthzReq(t, r, http.MethodPost, "/api/sites",
			map[string]any{"name": "X", "slug": slug, "workspace_id": wsID})
		if code != http.StatusBadRequest {
			t.Errorf("create with slug %q: status=%d, want 400 (body=%s)", slug, code, body)
		}
	}

	// Normalisation still runs BEFORE validation, so a human-typed slug is
	// tidied rather than refused. That is pre-existing behaviour and worth
	// pinning: it is the reason "Not A Slug" is accepted while "../../etc"
	// is not.
	code, body := doAuthzReq(t, r, http.MethodPost, "/api/sites",
		map[string]any{"name": "X", "slug": "Not A Slug", "workspace_id": wsID})
	if code != http.StatusCreated {
		t.Fatalf("normalisable slug: status=%d, want 201 (body=%s)", code, body)
	}
	if !strings.Contains(body, `"slug":"not-a-slug"`) {
		t.Errorf("slug was not normalised: %s", body)
	}

	code, body = doAuthzReq(t, r, http.MethodPost, "/api/sites",
		map[string]any{"name": "Fine", "slug": "fine-site", "workspace_id": wsID})
	if code != http.StatusCreated {
		t.Fatalf("create with a good slug: status=%d, want 201 (body=%s)", code, body)
	}
}

func TestSeed_RefusesAForeignWorkspace(t *testing.T) {
	db, q := setupDeployTestDB(t)
	const mineWS = "ws-seed-0000000000000001"
	const otherWS = "ws-seed-other-0000000001"
	seedWorkspace(t, q, mineWS, "ws-seed", "oss")
	seedWorkspace(t, q, otherWS, "ws-seed-other", "oss")
	seedWorkspaceUser(t, q, "u-seeder")
	if err := q.AddWorkspaceMember(context.Background(), store.AddWorkspaceMemberParams{
		WorkspaceID: mineWS, UserID: "u-seeder", Role: "member",
	}); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	h := NewSiteHandler(&config.Config{}, q, db)
	r := asRoleUser(chi.NewRouter(), "u-seeder", "editor")
	r.Post("/api/sites/seed", h.Seed)

	body := map[string]any{
		"workspace_id": otherWS,
		"type":         "one-pager",
		"info":         map[string]any{"name": "Nope", "slug": "nope", "lang": "en"},
		"structure":    map[string]any{"structure_type": "one-pager", "max_depth": 1},
	}
	code, raw := doAuthzReq(t, r, http.MethodPost, "/api/sites/seed", body)
	if code != http.StatusBadRequest {
		t.Fatalf("seeding into a foreign workspace: status=%d, want 400 (body=%s)", code, raw)
	}
	if !strings.Contains(raw, "not a member") {
		t.Errorf("unexpected refusal reason: %s", raw)
	}
}

// Guards the soft-delete gate's blast radius: the retention sweep writes
// 'paused' automatically and there is no un-pause path, so only 'deleted'
// may ever be refused.
func TestWorkspaceStatusGate_OnlyDeletedIsRefused(t *testing.T) {
	if workspaceStatusDeleted != "deleted" {
		t.Fatalf("workspaceStatusDeleted = %q; the middleware gate and Delete must agree", workspaceStatusDeleted)
	}
	_ = time.Now
}

// ---------------------------------------------------------------------
// The last-owner invariant must hold under CONCURRENCY, not just in a
// sequential test.
//
// The original guard counted owners in Go and then deleted, which is
// check-then-act: two concurrent removals both read the same count, both
// decide they are not the last, and the workspace ends with zero owners and
// nobody able to manage it. The review crew reproduced that 300/300 under
// the production connection config. The guard now lives inside the DELETE
// and UPDATE statements, so this races them for real.
//
// NOTE ON THE DSN: this test opens its own database rather than using
// setupDeployTestDB, because that helper passes mattn-style parameters
// (_journal_mode=WAL&_busy_timeout=...) which the modernc/sqlite driver in
// use here silently IGNORES. Under the ignored DSN the race presents as
// SQLITE_BUSY 500s, which look like the guard working. The `_pragma=` form
// below is the one modernc actually honours, which is what makes this test
// able to fail.
// ---------------------------------------------------------------------

func TestLastOwnerInvariant_HoldsUnderConcurrentRemoval(t *testing.T) {
	sqlDB := newRaceDB(t, "race.sqlite")
	q := store.New(sqlDB)
	ctx := context.Background()

	const wsID = "ws-race-0000000000000001"
	if err := q.CreateWorkspace(ctx, store.CreateWorkspaceParams{
		ID: wsID, Name: "Race", Slug: "ws-race", Plan: "oss",
		Region: "eu", BillingEmail: "b@example.com", Status: "active",
	}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	owners := []string{"u-race-owner-a", "u-race-owner-b"}
	for _, id := range owners {
		if err := q.CreateUser(ctx, store.CreateUserParams{
			ID: id, Email: id + "@example.com", PasswordHash: "x", Name: id, Role: "editor",
		}); err != nil {
			t.Fatalf("create user %s: %v", id, err)
		}
		if err := q.AddWorkspaceMember(ctx, store.AddWorkspaceMemberParams{
			WorkspaceID: wsID, UserID: id, Role: "owner",
		}); err != nil {
			t.Fatalf("seed owner %s: %v", id, err)
		}
	}

	// Both co-owners remove each other, released from a barrier so the
	// statements contend directly. Proven to expose the old guard 30/30.
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, id := range owners {
		wg.Add(1)
		go func(userID string) {
			defer wg.Done()
			<-start
			_, _ = q.RemoveWorkspaceMemberGuarded(ctx, store.RemoveWorkspaceMemberGuardedParams{
				WorkspaceID: wsID, UserID: userID, WorkspaceID_2: wsID,
			})
		}(id)
	}
	close(start)
	wg.Wait()

	rows, err := q.ListWorkspaceMembers(ctx, wsID)
	if err != nil {
		t.Fatalf("list members: %v", err)
	}
	remaining := 0
	for _, m := range rows {
		if m.Role == "owner" {
			remaining++
		}
	}
	if remaining < 1 {
		t.Fatalf("workspace was left with %d owners: the last-owner guard lost the race", remaining)
	}
}

func TestLastOwnerInvariant_HoldsUnderConcurrentDemotion(t *testing.T) {
	sqlDB := newRaceDB(t, "race2.sqlite")
	q := store.New(sqlDB)
	ctx := context.Background()

	const wsID = "ws-race-0000000000000002"
	if err := q.CreateWorkspace(ctx, store.CreateWorkspaceParams{
		ID: wsID, Name: "Race2", Slug: "ws-race-2", Plan: "oss",
		Region: "eu", BillingEmail: "b@example.com", Status: "active",
	}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	owners := []string{"u-demote-a", "u-demote-b"}
	for _, id := range owners {
		if err := q.CreateUser(ctx, store.CreateUserParams{
			ID: id, Email: id + "@example.com", PasswordHash: "x", Name: id, Role: "editor",
		}); err != nil {
			t.Fatalf("create user %s: %v", id, err)
		}
		if err := q.AddWorkspaceMember(ctx, store.AddWorkspaceMemberParams{
			WorkspaceID: wsID, UserID: id, Role: "owner",
		}); err != nil {
			t.Fatalf("seed owner %s: %v", id, err)
		}
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, id := range owners {
		wg.Add(1)
		go func(userID string) {
			defer wg.Done()
			<-start
			_, _ = q.DemoteWorkspaceMemberGuarded(ctx, store.DemoteWorkspaceMemberGuardedParams{
				Role: "member", WorkspaceID: wsID, UserID: userID, WorkspaceID_2: wsID,
			})
		}(id)
	}
	close(start)
	wg.Wait()

	rows, err := q.ListWorkspaceMembers(ctx, wsID)
	if err != nil {
		t.Fatalf("list members: %v", err)
	}
	remaining := 0
	for _, m := range rows {
		if m.Role == "owner" {
			remaining++
		}
	}
	if remaining < 1 {
		t.Fatalf("workspace was left with %d owners: the demotion guard lost the race", remaining)
	}
}

// ---------------------------------------------------------------------
// The three defects the review crew found in the FIRST round of fixes.
// Each one is the same shape: an invariant enforced in some of the places
// that can break it, but not all of them.
// ---------------------------------------------------------------------

// AddMember is an upsert, which makes it a third writer of the owner count.
// It was left unguarded when RemoveMember and UpdateMemberRole were fixed, so
// the sole owner could demote themselves to zero owners in one request.
func TestAddMember_SoleOwnerCannotSelfDemoteToZeroOwners(t *testing.T) {
	_, q := setupDeployTestDB(t)
	ctx := context.Background()
	const wsID = "ws-addm-0000000000000001"
	seedWorkspace(t, q, wsID, "ws-addm", "oss")
	seedWorkspaceUser(t, q, "u-sole-owner")
	if err := q.AddWorkspaceMember(ctx, store.AddWorkspaceMemberParams{
		WorkspaceID: wsID, UserID: "u-sole-owner", Role: "owner",
	}); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	h := NewWorkspaceHandler(&config.Config{}, q)
	r := withWorkspaceRole(asRoleUser(chi.NewRouter(), "u-sole-owner", "editor"), wsID, "owner")
	r.Post("/api/workspaces/{workspaceID}/members", h.AddMember)

	code, body := doAuthzReq(t, r, http.MethodPost, "/api/workspaces/"+wsID+"/members",
		map[string]any{"user_id": "u-sole-owner", "role": "member"})
	if code != http.StatusConflict {
		t.Fatalf("sole owner self-demoting via AddMember: status=%d, want 409 (body=%s)", code, body)
	}
	rows, _ := q.ListWorkspaceMembers(ctx, wsID)
	owners := 0
	for _, m := range rows {
		if m.Role == "owner" {
			owners++
		}
	}
	if owners < 1 {
		t.Fatalf("workspace left with %d owners via AddMember", owners)
	}
}

// A role change must not bring back a membership that a concurrent removal
// deleted between the existence read and the write. The upsert did exactly
// that; a plain UPDATE cannot.
func TestUpdateMemberRole_DoesNotResurrectARemovedMember(t *testing.T) {
	_, q := setupDeployTestDB(t)
	ctx := context.Background()
	const wsID = "ws-resurrect-000000000001"
	seedWorkspace(t, q, wsID, "ws-resurrect", "oss")
	for _, id := range []string{"u-res-owner", "u-res-target"} {
		seedWorkspaceUser(t, q, id)
	}
	if err := q.AddWorkspaceMember(ctx, store.AddWorkspaceMemberParams{
		WorkspaceID: wsID, UserID: "u-res-owner", Role: "owner",
	}); err != nil {
		t.Fatalf("seed owner: %v", err)
	}

	// The target's row is gone by the time the write lands, which is the
	// state a concurrent RemoveMember leaves behind.
	affected, err := q.UpdateWorkspaceMemberRoleIfMember(ctx, store.UpdateWorkspaceMemberRoleIfMemberParams{
		Role: "admin", WorkspaceID: wsID, UserID: "u-res-target",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if affected != 0 {
		t.Fatalf("update touched %d rows for a non-member, want 0", affected)
	}
	if _, err := q.GetWorkspaceMembership(ctx, store.GetWorkspaceMembershipParams{
		WorkspaceID: wsID, UserID: "u-res-target",
	}); err == nil {
		t.Fatal("a removed member was resurrected by a role change")
	}
}

// Removing someone from a workspace has to revoke the sites they built there.
// site_members is checked first in SiteAccessMiddleware and short-circuits, and
// a site's creator holds an owner row in it, so removing only the workspace
// membership left permanent access to everything they had created.
func TestRemoveMember_RevokesSiteGrantsInThatWorkspace(t *testing.T) {
	_, q := setupDeployTestDB(t)
	ctx := context.Background()
	const wsID = "ws-offboard-00000000001"
	const otherWS = "ws-keepmine-00000000001"
	seedWorkspace(t, q, wsID, "ws-offboard", "oss")
	seedWorkspace(t, q, otherWS, "ws-keepmine", "oss")
	seedWorkspaceUser(t, q, "u-owner-off")
	seedWorkspaceUser(t, q, "u-leaver")
	for _, m := range []struct{ user, role string }{{"u-owner-off", "owner"}, {"u-leaver", "member"}} {
		if err := q.AddWorkspaceMember(ctx, store.AddWorkspaceMemberParams{
			WorkspaceID: wsID, UserID: m.user, Role: m.role,
		}); err != nil {
			t.Fatalf("seed member: %v", err)
		}
	}

	mkSite := func(id, slug, ws string) {
		if err := q.CreateSite(ctx, store.CreateSiteParams{
			ID: id, WorkspaceID: ws, Name: slug, Slug: slug,
			PrimaryColor: "#000000", SecondaryColor: "#000000",
			BgColor: "#ffffff", TextColor: "#000000",
			FontHeading: "Inter", FontBody: "Inter", Lang: "en",
		}); err != nil {
			t.Fatalf("create site %s: %v", slug, err)
		}
		if err := q.AddSiteMember(ctx, store.AddSiteMemberParams{
			SiteID: id, UserID: "u-leaver", Role: "owner",
		}); err != nil {
			t.Fatalf("grant site member: %v", err)
		}
	}
	mkSite("site-built-000000000001", "built-here", wsID)
	// A site in a DIFFERENT workspace they are still entitled to keep.
	mkSite("site-elsewhere-00000001", "built-elsewhere", otherWS)

	h := NewWorkspaceHandler(&config.Config{}, q)
	r := withWorkspaceRole(asRoleUser(chi.NewRouter(), "u-owner-off", "editor"), wsID, "owner")
	r.Delete("/api/workspaces/{workspaceID}/members/{userID}", h.RemoveMember)

	code, body := doAuthzReq(t, r, http.MethodDelete, "/api/workspaces/"+wsID+"/members/u-leaver", nil)
	if code != http.StatusOK {
		t.Fatalf("remove member: status=%d, want 200 (body=%s)", code, body)
	}

	if _, err := q.GetSiteMembership(ctx, store.GetSiteMembershipParams{
		SiteID: "site-built-000000000001", UserID: "u-leaver",
	}); err == nil {
		t.Error("removed member kept an owner grant on a site in that workspace")
	}
	// Scoped: their grants elsewhere are untouched.
	if _, err := q.GetSiteMembership(ctx, store.GetSiteMembershipParams{
		SiteID: "site-elsewhere-00000001", UserID: "u-leaver",
	}); err != nil {
		t.Errorf("removal reached into another workspace's site grants: %v", err)
	}
}

// An admin with no membership of their own gets the estate's only workspace,
// and only when there is exactly one.
//
// ensureDefaultWorkspace backfills a membership for the FIRST admin only, so a
// colleague promoted later has none either. With several workspaces present,
// "the first one" is somebody else's, and on this estate that is the workspace
// holding a client's site: their demo would have been filed inside it.
func TestResolveWorkspaceForCreate_AdminFallbackOnlyWhenUnambiguous(t *testing.T) {
	// One workspace: the single-tenant install keeps working.
	db, q := setupDeployTestDB(t)
	const soleWS = "ws-sole-0000000000000001"
	seedWorkspace(t, q, soleWS, "default", "oss")
	seedWorkspaceUser(t, q, "u-only-admin")
	h := NewSiteHandler(&config.Config{}, q, db)
	r := asRoleUser(chi.NewRouter(), "u-only-admin", "admin")
	r.Post("/api/sites", h.Create)

	code, body := doAuthzReq(t, r, http.MethodPost, "/api/sites",
		map[string]any{"name": "Solo", "slug": "solo-site"})
	if code != http.StatusCreated {
		t.Fatalf("admin on a single-workspace install: status=%d, want 201 (body=%s)", code, body)
	}

	// Two workspaces: ambiguous, so refuse rather than guess.
	db2, q2 := setupDeployTestDB(t)
	const clientWS = "ws-client-0000000000001"
	seedWorkspace(t, q2, clientWS, "default", "oss")
	seedWorkspace(t, q2, "ws-other-00000000000001", "other", "oss")
	seedWorkspaceUser(t, q2, "u-second-admin")
	h2 := NewSiteHandler(&config.Config{}, q2, db2)
	r2 := asRoleUser(chi.NewRouter(), "u-second-admin", "admin")
	r2.Post("/api/sites", h2.Create)

	code, body = doAuthzReq(t, r2, http.MethodPost, "/api/sites",
		map[string]any{"name": "Demo", "slug": "demo-site"})
	if code != http.StatusBadRequest {
		t.Fatalf("second admin, several workspaces: status=%d, want 400 (body=%s)", code, body)
	}
	if !strings.Contains(body, "not in a workspace") {
		t.Errorf("unhelpful refusal: %s", body)
	}
	sites, _ := q2.ListSitesByWorkspace(context.Background(), clientWS)
	if len(sites) != 0 {
		t.Errorf("a site landed in the client workspace: %d", len(sites))
	}
}

// newRaceDB is newSchemaDB with a DSN the driver actually honours.
//
// The shared fixture opens `?_journal_mode=WAL&_busy_timeout=5000&...`, which
// is mattn syntax that modernc.org/sqlite IGNORES, so those fixtures are not
// in WAL and have no busy timeout. Harmless for most tests, fatal to the point
// of a concurrency test: under the ignored DSN the owner-count race presents
// as SQLITE_BUSY rather than a lost update, which looks like the guard working
// (measured 0/300 vs 299/300).
//
// It still copies the prebuilt template rather than applying internal/db.Schema,
// which is what the schema-fixture guard protects (~38ms vs ~441ms).
func newRaceDB(t *testing.T, file string) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, file)
	blank, err := os.ReadFile(schemaTemplatePath(t))
	if err != nil {
		t.Fatalf("read schema template: %v", err)
	}
	if err := os.WriteFile(dbPath, blank, 0o600); err != nil {
		t.Fatalf("write test db: %v", err)
	}
	sqlDB, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// Production-exact: cmd/server runs a single connection, the configuration
	// under which the old check-then-act guard failed 300/300.
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	return sqlDB
}
