// Package middleware: site-scoped authorization.
//
// Closes audit finding C1 (Critical). Pre-fix, every authenticated admin
// could read, update, delete or rebuild ANY site in the workspace by
// enumerating site IDs. The fix has two parts:
//
//  1. New site_members table linking users to sites with a role (see
//     schema.sql).
//  2. This middleware, mounted on every /api/sites/{siteID}/* route, that
//     verifies the authenticated user has a membership row for the path's
//     siteID before allowing the request through.
//
// Admin bypass: users with role='admin' on the workspace pass through
// regardless of site_members. This preserves the legacy single-workspace
// behaviour where the seeded admin owns everything; non-admin "editor"
// users (added via the existing /api/admin/invites flow) are gated by
// site_members rows the owner explicitly grants.
//
// The middleware extracts siteID from the URL via chi's URLParam. Routes
// using a different param name need a different middleware instance (or
// rename the param to siteID, every site-scoped route in atomicsite
// already uses {siteID}, verified at registration time).
package middleware

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/slab/internal/store"
)

// siteIDPattern mirrors handlers.isSafeSiteID. We duplicate it here so the
// middleware has no handlers/middleware import cycle and can validate
// before any DB call.
var siteIDPattern = regexp.MustCompile(`^[a-fA-F0-9]{24}$`)

// SiteAccessMiddleware returns a chi middleware that gates a route by the
// authenticated user's site membership. Mount it after AuthMiddleware so
// the user is already in context.
//
// Behaviour:
//   - Missing user (auth not run, or expired token) → 401
//   - Missing/malformed {siteID} URL param → 400
//   - User is admin → pass through (no DB query)
//   - User is non-admin and has no site_members row → 403
//   - DB error during membership check → 500 (fail closed; never grant
//     access on infra failure)
func SiteAccessMiddleware(queries *store.Queries) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := GetUser(r)
			if user == nil {
				writeAuthError(w, http.StatusUnauthorized, "Not authenticated")
				return
			}
			siteID := chi.URLParam(r, "siteID")
			if !siteIDPattern.MatchString(siteID) {
				writeAuthError(w, http.StatusBadRequest, "Invalid siteID")
				return
			}

			// (1) Direct per-site grant always passes (the explicit
			// site_members invite). The role lives in the row; owner-only
			// operations re-check it inside their handlers.
			if _, err := queries.GetSiteMembership(r.Context(), store.GetSiteMembershipParams{
				SiteID: siteID,
				UserID: user.ID,
			}); err == nil {
				next.ServeHTTP(w, r)
				return
			} else if !errors.Is(err, sql.ErrNoRows) {
				// Infra failure (DB closed, query error). Fail closed.
				writeAuthError(w, http.StatusInternalServerError, "Authorization check failed")
				return
			}

			// The workspace is the tenant isolation boundary, so resolve
			// the site's workspace before honouring any global-role power.
			// Pre-fix this used an unconditional user.Role=="admin" bypass,
			// which let an admin (incl. an over-provisioned SSO admin) read
			// or mutate ANY tenant's site with no membership and no audit.
			site, err := queries.GetSiteByID(r.Context(), siteID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					writeAuthError(w, http.StatusForbidden, "No access to this site")
					return
				}
				writeAuthError(w, http.StatusInternalServerError, "Authorization check failed")
				return
			}

			// (2) Legacy single-workspace sites (workspace_id not yet
			// backfilled) keep the original admin-owns-everything model.
			// New sites always carry a workspace_id.
			if site.WorkspaceID == "" {
				if user.Role == "admin" {
					next.ServeHTTP(w, r)
					return
				}
				writeAuthError(w, http.StatusForbidden, "No access to this site")
				return
			}

			// (3) Member of the site's own workspace passes (this is the
			// tenant's own staff reaching their own sites), UNLESS the
			// workspace has been deleted.
			//
			// WorkspaceAccessMiddleware started refusing deleted workspaces,
			// but this branch did not, so "delete the workspace" revoked the
			// workspace routes and left roughly a hundred SITE routes wide
			// open to the same members, including form submissions and other
			// PII, plus the agent-key surface. The admin UI meanwhile promised
			// "Everyone loses access to it". Two halves of one invariant have
			// to move together.
			if _, err := queries.GetWorkspaceMembership(r.Context(), store.GetWorkspaceMembershipParams{
				WorkspaceID: site.WorkspaceID,
				UserID:      user.ID,
			}); err == nil {
				if ws, wsErr := queries.GetWorkspaceByID(r.Context(), site.WorkspaceID); wsErr == nil && ws.Status == "deleted" {
					writeAuthError(w, http.StatusForbidden, "No access to this site")
					return
				}
				next.ServeHTTP(w, r)
				return
			} else if !errors.Is(err, sql.ErrNoRows) {
				writeAuthError(w, http.StatusInternalServerError, "Authorization check failed")
				return
			}

			// (4) Not a member of the site's workspace. Mirror
			// WorkspaceAccessMiddleware: a genuine platform admin / support
			// user may cross workspaces, but the access is audited; every
			// other caller is denied.
			switch user.Role {
			case "admin":
				writeCrossWorkspaceAudit(r.Context(), queries, user, site.WorkspaceID, r, "cross_workspace_site_admin")
				next.ServeHTTP(w, r)
			case "support":
				if r.Method != http.MethodGet && r.Method != http.MethodHead {
					writeAuthError(w, http.StatusForbidden, "Support role is read-only across workspaces")
					return
				}
				writeCrossWorkspaceAudit(r.Context(), queries, user, site.WorkspaceID, r, "cross_workspace_site_support")
				next.ServeHTTP(w, r)
			default:
				writeAuthError(w, http.StatusForbidden, "No access to this site")
			}
		})
	}
}

// SiteAccessRole returns the membership row's role for the current
// (user, site) so handlers can enforce owner-only operations on top of the
// middleware's coarse pass/fail gate. Returns "" when the user is admin
// (no membership row needed) or when the user has no row.
func SiteAccessRole(ctx context.Context, queries *store.Queries, userID, siteID string) string {
	row, err := queries.GetSiteMembership(ctx, store.GetSiteMembershipParams{
		SiteID: siteID,
		UserID: userID,
	})
	if err != nil {
		return ""
	}
	return row.Role
}

// IsOwnerOrAdmin reports whether the caller owns the site in the URL (or is
// a platform admin) WITHOUT writing anything to the response.
//
// It is the same question RequireOwnerOrAdmin asks, split out because read
// paths need the answer to decide what to serialise rather than whether to
// refuse: a settings list is a mixed collection, so it drops the rows the
// caller may not see instead of failing the whole request. Two copies of
// this predicate would be exactly the drift this package keeps producing,
// so RequireOwnerOrAdmin is defined in terms of it.
func IsOwnerOrAdmin(r *http.Request, queries *store.Queries) bool {
	user := GetUser(r)
	if user == nil {
		return false
	}
	if user.Role == "admin" {
		return true
	}
	siteID := chi.URLParam(r, "siteID")
	return SiteAccessRole(r.Context(), queries, user.ID, siteID) == "owner"
}

// RequireOwnerOrAdmin gates owner-only handlers (delete site, add member,
// remove member). Admins always pass; non-admins need role='owner'.
// Returns true when authorized; on false the caller has already received
// a 403 written by this function.
func RequireOwnerOrAdmin(w http.ResponseWriter, r *http.Request, queries *store.Queries) bool {
	if GetUser(r) == nil {
		writeAuthError(w, http.StatusUnauthorized, "Not authenticated")
		return false
	}
	if !IsOwnerOrAdmin(r, queries) {
		writeAuthError(w, http.StatusForbidden, "Owner access required")
		return false
	}
	return true
}

// writeAuthError writes a JSON error without depending on handlers/helpers
// (which would create an import cycle).
func writeAuthError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"` + msg + `"}`))
}
