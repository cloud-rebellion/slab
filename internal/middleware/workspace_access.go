// Package middleware: workspace-scoped authorization (Phase 30, Cloud
// Tier MVP, 2026-05-05).
//
// Mirrors site_access.go but layers the workspace concept above sites.
// In OSS single-tenant deployments the workspace is invisible: a single
// auto-bootstrap workspace owns every site, every user is a member, and
// the workspace_access middleware never refuses a request that
// site_access already cleared.
//
// In Cloud (multi-tenant) deployments the workspace is the billing
// boundary. Cross-workspace reads are blocked here even when site_access
// would have cleared them via admin-bypass: a global admin in workspace
// A still does not see workspace B's sites.
//
// Mount this middleware on every /api/workspaces/{workspaceID}/* route
// and on the /api/sites/{siteID}/* routes (after site_access) when the
// site lookup needs the workspace_id read.
package middleware

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/slab/internal/store"
)

// workspaceIDPattern matches the 24-hex-char site-id shape (workspaces
// share the same id space). Validates before any DB call.
var workspaceIDPattern = regexp.MustCompile(`^[a-fA-F0-9]{24}$`)

// WorkspaceContextKey is the request-context key for the resolved
// workspace membership row. Handlers read it via GetWorkspaceMembership.
const WorkspaceContextKey contextKey = "workspace_membership"

// WorkspaceMembership is the request-scoped resolved (workspace, role)
// pair. Attached to ctx by WorkspaceAccessMiddleware so handlers can
// gate owner-only operations without a second DB roundtrip.
type WorkspaceMembership struct {
	WorkspaceID string
	Role        string
}

// GetWorkspaceMembership retrieves the resolved membership from ctx.
// Returns nil when the middleware did not run (route not gated by
// workspace_access) or when the user is admin (admin bypass).
func GetWorkspaceMembership(r *http.Request) *WorkspaceMembership {
	m, _ := r.Context().Value(WorkspaceContextKey).(*WorkspaceMembership)
	return m
}

// WorkspaceAccessMiddleware returns a chi middleware that gates a route
// by the authenticated user's workspace membership. Mount it after
// AuthMiddleware so the user is already in context. The {workspaceID}
// URL param is required.
//
// Cross-workspace access policy (Phase 30.6, GDPR-defensible audit
// trail for the Cloud tier):
//
//   - User has a workspace_members row → pass through with that role.
//   - user.role='admin' without a row → bypass (break-glass), write
//     audit_log with action="cross_workspace_admin". Both reads and
//     writes allowed; the audit row is the paper trail.
//   - user.role='support' without a row → bypass for GET only, write
//     audit_log with action="cross_workspace_support". Refuses
//     non-GET with 403 so the support team can never write to a
//     customer's content. Read-only access for hand-on-tech support
//     in the early Cloud phase.
//   - Any other role without a row → 403.
//
// Every cross-workspace bypass writes an audit row, so the BI team
// can answer "who looked at this workspace, when, why" via the
// existing /api/admin/audit-log surface.
func WorkspaceAccessMiddleware(queries *store.Queries) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := GetUser(r)
			if user == nil {
				writeAuthError(w, http.StatusUnauthorized, "Not authenticated")
				return
			}
			workspaceID := chi.URLParam(r, "workspaceID")
			if !workspaceIDPattern.MatchString(workspaceID) {
				writeAuthError(w, http.StatusBadRequest, "Invalid workspaceID")
				return
			}
			// A soft-deleted workspace is inert for its members. Delete
			// only ever set status='deleted' and nothing read it back, so
			// "delete this workspace" revoked precisely nothing: members
			// kept full read+write and it kept appearing in their list.
			// Platform admin keeps the audited break-glass path below so a
			// mistaken delete is still recoverable by an operator.
			//
			// Only 'deleted' is gated. 'paused' is written automatically by
			// the retention lifecycle sweep (retention.go) and has no
			// un-pause path, so enforcing it would black-hole a live
			// workspace with no way back.
			if ws, wsErr := queries.GetWorkspaceByID(r.Context(), workspaceID); wsErr == nil && ws.Status == "deleted" {
				if user.Role != "admin" {
					writeAuthError(w, http.StatusNotFound, "Workspace not found")
					return
				}
			}
			// Membership first. Owns the happy path.
			row, err := queries.GetWorkspaceMembership(r.Context(), store.GetWorkspaceMembershipParams{
				WorkspaceID: workspaceID,
				UserID:      user.ID,
			})
			if err == nil {
				ctx := context.WithValue(r.Context(), WorkspaceContextKey, &WorkspaceMembership{
					WorkspaceID: workspaceID,
					Role:        row.Role,
				})
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			if !errors.Is(err, sql.ErrNoRows) {
				writeAuthError(w, http.StatusInternalServerError, "Authorization check failed")
				return
			}

			// No membership row. Check global-role escalation.
			switch user.Role {
			case "admin":
				writeCrossWorkspaceAudit(r.Context(), queries, user, workspaceID, r, "cross_workspace_admin")
				ctx := context.WithValue(r.Context(), WorkspaceContextKey, &WorkspaceMembership{
					WorkspaceID: workspaceID,
					Role:        "owner",
				})
				next.ServeHTTP(w, r.WithContext(ctx))
			case "support":
				if r.Method != http.MethodGet && r.Method != http.MethodHead {
					writeAuthError(w, http.StatusForbidden, "Support role is read-only across workspaces")
					return
				}
				writeCrossWorkspaceAudit(r.Context(), queries, user, workspaceID, r, "cross_workspace_support")
				ctx := context.WithValue(r.Context(), WorkspaceContextKey, &WorkspaceMembership{
					WorkspaceID: workspaceID,
					Role:        "member",
				})
				next.ServeHTTP(w, r.WithContext(ctx))
			default:
				writeAuthError(w, http.StatusForbidden, "No access to this workspace")
			}
		})
	}
}

// writeCrossWorkspaceAudit records a cross-workspace bypass event
// in audit_log. Phase 30.6, 2026-05-06. Logged-best-effort: a write
// failure is non-fatal (we don't want a DB blip to gate the
// support-team's admin work) but slog'd so an operator can see it.
func writeCrossWorkspaceAudit(ctx context.Context, queries *store.Queries, user *AuthUser, workspaceID string, r *http.Request, action string) {
	changes, _ := json.Marshal(map[string]any{
		"method": r.Method,
		"path":   r.URL.Path,
		"role":   user.Role,
	})
	id := newAuditID()
	if err := queries.WriteAuditLog(ctx, store.WriteAuditLogParams{
		ID:           id,
		ActorUserID:  user.ID,
		ActorRole:    user.Role,
		ActorIp:      auditClientIP(r),
		SiteID:       "",
		Action:       action,
		ResourceType: "workspace",
		ResourceID:   workspaceID,
		ChangesJson:  string(changes),
	}); err != nil {
		slog.Warn("audit_log: cross-workspace bypass write failed",
			"actor", user.ID, "workspace_id", workspaceID, "action", action, "err", err)
	}
}

func newAuditID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// auditClientIP returns the visitor IP for audit_log. Reads only
// r.RemoteAddr because TrustedProxyRealIP at the top of the
// middleware stack has already canonicalized it (resolving XFF only
// when the immediate peer is in ATOMICSITE_TRUSTED_PROXIES). Reading
// XFF directly here would let an untrusted peer spoof the audit log.
func auditClientIP(r *http.Request) string {
	addr := r.RemoteAddr
	if colon := lastIndexByte(addr, ':'); colon > 0 {
		return addr[:colon]
	}
	return addr
}

// lastIndexByte locates the last occurrence of c in s, returning its
// index or -1. Used by auditClientIP to split host:port without
// importing the strings package (keeps this middleware dependency-free).
func lastIndexByte(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// RequireWorkspaceRole gates handlers that need a specific role
// (e.g. only "owner" can change billing). Reads the resolved membership
// from ctx so it does not double-roundtrip the DB. Returns true on
// authorized; on false the caller has already received a 403.
func RequireWorkspaceRole(w http.ResponseWriter, r *http.Request, requiredRole string) bool {
	user := GetUser(r)
	if user == nil {
		writeAuthError(w, http.StatusUnauthorized, "Not authenticated")
		return false
	}
	if user.Role == "admin" {
		return true
	}
	m := GetWorkspaceMembership(r)
	if m == nil {
		writeAuthError(w, http.StatusForbidden, "Workspace membership required")
		return false
	}
	switch requiredRole {
	case "owner":
		if m.Role != "owner" {
			writeAuthError(w, http.StatusForbidden, "Owner role required")
			return false
		}
	case "admin":
		if m.Role != "owner" && m.Role != "admin" {
			writeAuthError(w, http.StatusForbidden, "Admin role required")
			return false
		}
	case "member":
		// Any membership row clears.
	default:
		writeAuthError(w, http.StatusInternalServerError, "Unknown required role")
		return false
	}
	return true
}
