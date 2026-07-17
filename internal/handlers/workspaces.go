// Package handlers: workspaces.go (Phase 30, Cloud Tier MVP, 2026-05-05).
//
// Workspaces are the billing + ownership boundary above sites. One
// workspace owns N sites; one user can belong to N workspaces. Routes:
//
//	GET    /api/workspaces                      list mine
//	POST   /api/workspaces                      create (cloud signup or admin)
//	GET    /api/workspaces/{workspaceID}        details
//	PATCH  /api/workspaces/{workspaceID}        update (name, billing_email)
//	DELETE /api/workspaces/{workspaceID}        soft-delete (owner only)
//	GET    /api/workspaces/{workspaceID}/members
//	POST   /api/workspaces/{workspaceID}/members           add member (owner)
//	DELETE /api/workspaces/{workspaceID}/members/{userID}  remove (owner)
//	POST   /api/workspaces/{workspaceID}/invites           mint invite token
//	GET    /api/workspaces/{workspaceID}/invites           list pending
//	DELETE /api/workspaces/{workspaceID}/invites/{id}      revoke
//	GET    /api/workspaces/{workspaceID}/sites             list this ws's sites
//
// Mounted under WorkspaceAccessMiddleware so cross-workspace reads are
// rejected before any handler runs.
package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/bright-interaction/slab/internal/config"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

const workspaceInviteTTL = 7 * 24 * time.Hour

// WorkspaceHandler covers CRUD + members + invites for workspaces.
type WorkspaceHandler struct {
	cfg     *config.Config
	queries *store.Queries
}

func NewWorkspaceHandler(cfg *config.Config, queries *store.Queries) *WorkspaceHandler {
	return &WorkspaceHandler{cfg: cfg, queries: queries}
}

type workspaceView struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Slug               string `json:"slug"`
	Plan               string `json:"plan"`
	Region             string `json:"region"`
	BillingEmail       string `json:"billing_email"`
	Status             string `json:"status"`
	TrialEndsAt        string `json:"trial_ends_at"`
	StripeCustomerID   string `json:"stripe_customer_id,omitempty"`
	StripeSubscription string `json:"stripe_subscription_id,omitempty"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
	Role               string `json:"role,omitempty"`
}

func workspaceToView(w store.Workspace, role string) workspaceView {
	return workspaceView{
		ID:                 w.ID,
		Name:               w.Name,
		Slug:               w.Slug,
		Plan:               w.Plan,
		Region:             w.Region,
		BillingEmail:       w.BillingEmail,
		Status:             w.Status,
		TrialEndsAt:        w.TrialEndsAt,
		StripeCustomerID:   w.StripeCustomerID,
		StripeSubscription: w.StripeSubscriptionID,
		CreatedAt:          w.CreatedAt,
		UpdatedAt:          w.UpdatedAt,
		Role:               role,
	}
}

// slugRE enforces lowercase letters/digits/hyphens for the
// public-facing workspace slug.
var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)

func slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 40 {
		out = out[:40]
	}
	if len(out) < 3 {
		out = out + hex.EncodeToString(randBytes(3))
	}
	return out
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// ListMine returns the workspaces the authenticated user is a member of.
func (h *WorkspaceHandler) ListMine(w http.ResponseWriter, r *http.Request) {
	user := authmw.GetUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	rows, err := h.queries.ListWorkspacesForUser(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list workspaces")
		return
	}
	out := make([]workspaceView, 0, len(rows))
	for _, row := range rows {
		out = append(out, workspaceView{
			ID:                 row.ID,
			Name:               row.Name,
			Slug:               row.Slug,
			Plan:               row.Plan,
			Region:             row.Region,
			BillingEmail:       row.BillingEmail,
			Status:             row.Status,
			TrialEndsAt:        row.TrialEndsAt,
			StripeCustomerID:   row.StripeCustomerID,
			StripeSubscription: row.StripeSubscriptionID,
			CreatedAt:          row.CreatedAt,
			UpdatedAt:          row.UpdatedAt,
			Role:               row.Role,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": out})
}

// Get returns one workspace by ID. WorkspaceAccessMiddleware has
// already verified membership.
func (h *WorkspaceHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := urlParam(r, "workspaceID")
	row, err := h.queries.GetWorkspaceByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "Workspace not found")
		return
	}
	role := ""
	if m := authmw.GetWorkspaceMembership(r); m != nil {
		role = m.Role
	}
	writeJSON(w, http.StatusOK, workspaceToView(row, role))
}

// Create mints a new workspace and adds the caller as owner. Used by:
//   - The OSS bootstrap (calling this directly with a service-account
//     context is fine; the caller controls authentication).
//   - Cloud signup (Phase 30.5) after a waitlist invite is redeemed.
//   - Admin tooling (the existing admin can create workspaces via the
//     /api/admin/workspaces alias mounted in 30.2).
func (h *WorkspaceHandler) Create(w http.ResponseWriter, r *http.Request) {
	user := authmw.GetUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	var req struct {
		Name         string `json:"name"`
		Slug         string `json:"slug"`
		Plan         string `json:"plan"`
		BillingEmail string `json:"billing_email"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	slug := strings.TrimSpace(strings.ToLower(req.Slug))
	if slug == "" {
		slug = slugify(name)
	}
	if !slugRE.MatchString(slug) {
		writeError(w, http.StatusBadRequest, "slug must be lowercase letters, digits, hyphens (3-40 chars, no leading/trailing hyphen)")
		return
	}
	plan := req.Plan
	if plan == "" {
		plan = "oss"
	}
	billing := strings.TrimSpace(req.BillingEmail)
	if billing == "" {
		billing = user.Email
	}
	id := newID()
	if err := h.queries.CreateWorkspace(r.Context(), store.CreateWorkspaceParams{
		ID:           id,
		Name:         name,
		Slug:         slug,
		Plan:         plan,
		Region:       "eu",
		BillingEmail: billing,
		Status:       "active",
		TrialEndsAt:  "",
	}); err != nil {
		writeError(w, http.StatusBadRequest, "Failed to create workspace (slug taken?)")
		return
	}
	if err := h.queries.AddWorkspaceMember(r.Context(), store.AddWorkspaceMemberParams{
		WorkspaceID: id,
		UserID:      user.ID,
		Role:        "owner",
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to add owner")
		return
	}
	row, err := h.queries.GetWorkspaceByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to read created workspace")
		return
	}
	writeJSON(w, http.StatusCreated, workspaceToView(row, "owner"))
}

// Update mutates name + billing_email. Owner-only.
func (h *WorkspaceHandler) Update(w http.ResponseWriter, r *http.Request) {
	if !authmw.RequireWorkspaceRole(w, r, "owner") {
		return
	}
	id := urlParam(r, "workspaceID")
	var req struct {
		Name         string `json:"name"`
		BillingEmail string `json:"billing_email"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := h.queries.UpdateWorkspace(r.Context(), store.UpdateWorkspaceParams{
		ID:           id,
		Name:         strings.TrimSpace(req.Name),
		BillingEmail: strings.TrimSpace(req.BillingEmail),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update workspace")
		return
	}
	row, _ := h.queries.GetWorkspaceByID(r.Context(), id)
	role := ""
	if m := authmw.GetWorkspaceMembership(r); m != nil {
		role = m.Role
	}
	writeJSON(w, http.StatusOK, workspaceToView(row, role))
}

// Delete soft-deletes a workspace (sets status='deleted'). Hard-delete
// runs in a follow-up cron after a 90-day grace period (Phase 30.5).
// Owner-only.
func (h *WorkspaceHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if !authmw.RequireWorkspaceRole(w, r, "owner") {
		return
	}
	id := urlParam(r, "workspaceID")
	row, err := h.queries.GetWorkspaceByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "Workspace not found")
		return
	}
	if err := h.queries.UpdateWorkspacePlan(r.Context(), store.UpdateWorkspacePlanParams{
		ID:     id,
		Plan:   row.Plan,
		Status: "deleted",
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete workspace")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

// ListMembers returns members of one workspace.
func (h *WorkspaceHandler) ListMembers(w http.ResponseWriter, r *http.Request) {
	id := urlParam(r, "workspaceID")
	rows, err := h.queries.ListWorkspaceMembers(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list members")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": rows})
}

// AddMember adds a user to the workspace by user_id (the user must
// already exist; signup-then-add is the workflow). Owner-only.
func (h *WorkspaceHandler) AddMember(w http.ResponseWriter, r *http.Request) {
	if !authmw.RequireWorkspaceRole(w, r, "owner") {
		return
	}
	id := urlParam(r, "workspaceID")
	var req struct {
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	role := req.Role
	if role == "" {
		role = "member"
	}
	if role != "owner" && role != "admin" && role != "member" {
		writeError(w, http.StatusBadRequest, "role must be owner, admin, or member")
		return
	}
	if err := h.queries.AddWorkspaceMember(r.Context(), store.AddWorkspaceMemberParams{
		WorkspaceID: id,
		UserID:      req.UserID,
		Role:        role,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to add member")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"status": "ok"})
}

// RemoveMember strips a user from the workspace. Owner-only. Refuses to
// remove the last owner (would leave the workspace unmanaged).
func (h *WorkspaceHandler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	if !authmw.RequireWorkspaceRole(w, r, "owner") {
		return
	}
	id := urlParam(r, "workspaceID")
	userID := urlParam(r, "userID")
	rows, err := h.queries.ListWorkspaceMembers(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load members")
		return
	}
	owners := 0
	targetIsOwner := false
	for _, m := range rows {
		if m.Role == "owner" {
			owners++
			if m.UserID == userID {
				targetIsOwner = true
			}
		}
	}
	if targetIsOwner && owners <= 1 {
		writeError(w, http.StatusConflict, "Cannot remove the last owner")
		return
	}
	if err := h.queries.RemoveWorkspaceMember(r.Context(), store.RemoveWorkspaceMemberParams{
		WorkspaceID: id,
		UserID:      userID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to remove member")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// CreateInvite mints a one-time signup token scoped to this workspace.
// Token URL is returned in the response (operator copies it
// out-of-band; mail delivery via MailSender lands in 30.5).
func (h *WorkspaceHandler) CreateInvite(w http.ResponseWriter, r *http.Request) {
	if !authmw.RequireWorkspaceRole(w, r, "admin") {
		return
	}
	user := authmw.GetUser(r)
	id := urlParam(r, "workspaceID")
	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	email := strings.TrimSpace(strings.ToLower(req.Email))
	if email == "" {
		writeError(w, http.StatusBadRequest, "email is required")
		return
	}
	role := req.Role
	if role == "" {
		role = "member"
	}
	if role != "owner" && role != "admin" && role != "member" {
		writeError(w, http.StatusBadRequest, "role must be owner, admin, or member")
		return
	}
	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to generate token")
		return
	}
	token := hex.EncodeToString(tokenBytes)
	inviteID := newID()
	createdBy := ""
	if user != nil {
		createdBy = user.ID
	}
	if err := h.queries.CreateWorkspaceInvite(r.Context(), store.CreateWorkspaceInviteParams{
		ID:          inviteID,
		WorkspaceID: id,
		Email:       email,
		Role:        role,
		Token:       token,
		CreatedBy:   createdBy,
		ExpiresAt:   time.Now().Add(workspaceInviteTTL).UTC().Format(time.RFC3339),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create invite")
		return
	}
	base := strings.TrimRight(h.cfg.BaseURL, "/")
	url := base + "/cloud/signup/" + token
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":           inviteID,
		"workspace_id": id,
		"email":        email,
		"role":         role,
		"token":        token,
		"url":          url,
	})
}

// ListInvites returns pending invites for the workspace.
func (h *WorkspaceHandler) ListInvites(w http.ResponseWriter, r *http.Request) {
	id := urlParam(r, "workspaceID")
	rows, err := h.queries.ListPendingWorkspaceInvites(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list invites")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": rows})
}

// DeleteInvite revokes a pending invite.
func (h *WorkspaceHandler) DeleteInvite(w http.ResponseWriter, r *http.Request) {
	if !authmw.RequireWorkspaceRole(w, r, "admin") {
		return
	}
	inviteID := urlParam(r, "inviteID")
	// Scope the delete to the workspace the caller is authorized for. Without the
	// workspace_id predicate an admin of workspace A could revoke another tenant's
	// pending invite by id (cross-workspace write / onboarding griefing).
	workspaceID := urlParam(r, "workspaceID")
	if err := h.queries.DeleteWorkspaceInvite(r.Context(), store.DeleteWorkspaceInviteParams{ID: inviteID, WorkspaceID: workspaceID}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete invite")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// ListSites returns sites belonging to the workspace.
func (h *WorkspaceHandler) ListSites(w http.ResponseWriter, r *http.Request) {
	id := urlParam(r, "workspaceID")
	rows, err := h.queries.ListSitesByWorkspace(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list sites")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": rows})
}
