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

	"github.com/bright-interaction/slab/internal/billing"
	"github.com/bright-interaction/slab/internal/config"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

const workspaceInviteTTL = 7 * 24 * time.Hour

// workspaceStatusDeleted is the soft-delete sentinel written by Delete
// and by the retention lifecycle sweep. A workspace in this state is
// invisible to its members (ListMine filters it, WorkspaceAccessMiddleware
// refuses it) but the row survives for the 90-day grace period.
const workspaceStatusDeleted = "deleted"

// defaultWorkspacePlan is what a self-hosted (single-deployment) install
// gets: the OSS plan, which billing.Limit treats as unlimited. Cloud
// self-serve signups instead start on the smallest real tier with a
// trial, matching the schema note at internal/db/schema.sql:56.
const (
	defaultWorkspacePlan     = "oss"
	cloudSignupWorkspacePlan = "solo"
	cloudSignupTrial         = 14 * 24 * time.Hour
)

// cloudMode reports whether this binary is serving the multi-tenant
// Cloud deployment rather than a single self-hosted install.
func (h *WorkspaceHandler) cloudMode() bool {
	return h.cfg != nil && h.cfg.DeploymentMode == config.DeploymentModeCloud
}

// validWorkspaceRoles is the workspace-role vocabulary. Distinct from the
// platform vocabulary in users.role (admin|editor|support): do not mix
// the two.
func validWorkspaceRole(role string) bool {
	return role == "owner" || role == "admin" || role == "member"
}

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
	// Who may create at all. On a self-hosted instance workspaces are an
	// operator concept: the admin creates one per client and invites into
	// it. Cloud is the opposite, self-serve signup IS a workspace create,
	// so any authenticated user may create there.
	if user.Role != "admin" && !h.cloudMode() {
		writeError(w, http.StatusForbidden, "Only an administrator can create a workspace on this instance")
		return
	}
	// The plan decides the quota caps, so it is not caller-controlled.
	// Before this gate any authenticated user could POST {"plan":"agency"}
	// and self-grant the €299 caps, and because billing.Lookup falls back
	// to oss (which is UNLIMITED) for an unrecognised key, a typo granted
	// more than the most expensive real plan. Only a platform admin may
	// name a plan, and only one that actually exists.
	plan := defaultWorkspacePlan
	if h.cloudMode() {
		plan = cloudSignupWorkspacePlan
	}
	if requested := strings.TrimSpace(req.Plan); requested != "" {
		if user.Role != "admin" {
			writeError(w, http.StatusForbidden, "Only an administrator can choose a plan")
			return
		}
		if _, ok := billing.Plans[requested]; !ok {
			writeError(w, http.StatusBadRequest, "Unknown plan")
			return
		}
		plan = requested
	}
	billing := strings.TrimSpace(req.BillingEmail)
	if billing == "" {
		billing = user.Email
	}
	// A cloud self-serve workspace lands on a trial; an operator-created
	// one has no trial clock at all.
	trialEndsAt := ""
	if h.cloudMode() && user.Role != "admin" {
		trialEndsAt = time.Now().UTC().Add(cloudSignupTrial).Format(time.RFC3339)
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
		TrialEndsAt:  trialEndsAt,
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
	// Pointers, so an omitted field is distinguishable from an explicit
	// empty one. UpdateWorkspace writes both columns unconditionally, so
	// the previous value-typed struct silently blanked billing_email on
	// every rename: the settings form only ever sends the field it
	// changed. nil now means "leave it", "" means "clear it".
	var req struct {
		Name         *string `json:"name"`
		BillingEmail *string `json:"billing_email"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	current, err := h.queries.GetWorkspaceByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "Workspace not found")
		return
	}
	name := current.Name
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	billingEmail := current.BillingEmail
	if req.BillingEmail != nil {
		billingEmail = strings.TrimSpace(*req.BillingEmail)
	}
	if err := h.queries.UpdateWorkspace(r.Context(), store.UpdateWorkspaceParams{
		ID:           id,
		Name:         name,
		BillingEmail: billingEmail,
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

// ListMembers returns members of one workspace. Readable by ANY member,
// deliberately: a colleague roster is ordinary product information and
// the members page has to render for a plain member. The asymmetry with
// ListInvites (admin-only) is intentional: a pending invite is a
// privilege-escalation primitive, a roster is not.
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
	if !validWorkspaceRole(role) {
		writeError(w, http.StatusBadRequest, "role must be owner, admin, or member")
		return
	}
	// AddMember doubles as a role change, because AddWorkspaceMember is an
	// upsert. That makes it a THIRD writer of the owner count, and it was left
	// unguarded when the other two were fixed: the sole owner could post
	// themselves at role "member" and take the workspace to zero owners in a
	// single request. Any transition that moves an existing owner off owner
	// goes through the same atomic guard the other two writers use.
	if existing, err := h.queries.GetWorkspaceMembership(r.Context(), store.GetWorkspaceMembershipParams{
		WorkspaceID: id,
		UserID:      req.UserID,
	}); err == nil && existing.Role == "owner" && role != "owner" {
		affected, err := h.queries.DemoteWorkspaceMemberGuarded(r.Context(), store.DemoteWorkspaceMemberGuardedParams{
			Role:          role,
			WorkspaceID:   id,
			UserID:        req.UserID,
			WorkspaceID_2: id,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to add member")
			return
		}
		if affected == 0 {
			writeError(w, http.StatusConflict, "Cannot demote the last owner")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"status": "ok"})
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
	// Existence first, so "no such member" is a 404 rather than being folded
	// into the guard's zero-rows result below.
	if _, err := h.queries.GetWorkspaceMembership(r.Context(), store.GetWorkspaceMembershipParams{
		WorkspaceID: id,
		UserID:      userID,
	}); err != nil {
		writeError(w, http.StatusNotFound, "Not a member of this workspace")
		return
	}
	// The last-owner guard is IN the delete. Counting owners in Go first and
	// then deleting is check-then-act: two concurrent removals both read the
	// same count, both pass, and the workspace is left with no owner at all.
	affected, err := h.queries.RemoveWorkspaceMemberGuarded(r.Context(), store.RemoveWorkspaceMemberGuardedParams{
		WorkspaceID:   id,
		UserID:        userID,
		WorkspaceID_2: id,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to remove member")
		return
	}
	if affected == 0 {
		writeError(w, http.StatusConflict, "Cannot remove the last owner")
		return
	}
	// Removing the workspace membership is not enough on its own: site_members
	// is checked FIRST in SiteAccessMiddleware and short-circuits, and every
	// site's creator holds an owner row there. Without this the removed member
	// keeps permanent access to everything they built, which is the opposite
	// of what the UI tells the operator.
	if _, err := h.queries.RemoveSiteMembershipsForWorkspaceUser(r.Context(), store.RemoveSiteMembershipsForWorkspaceUserParams{
		UserID:      userID,
		WorkspaceID: id,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Member removed but site access could not be revoked; retry")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// UpdateMemberRole changes an existing member's role. Owner-only, and
// granting "owner" additionally requires the caller to be an owner, so
// escalation stays monotone with CreateInvite.
//
// Deliberately refuses to create a membership: the target must already be
// a member, otherwise PATCH would be a second AddMember that skips its
// checks. Reuses AddWorkspaceMember, which is an upsert
// (workspace_members.sql:1-4), rather than adding a near-duplicate query.
func (h *WorkspaceHandler) UpdateMemberRole(w http.ResponseWriter, r *http.Request) {
	if !authmw.RequireWorkspaceRole(w, r, "owner") {
		return
	}
	id := urlParam(r, "workspaceID")
	userID := urlParam(r, "userID")
	var req struct {
		Role string `json:"role"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if !validWorkspaceRole(req.Role) {
		writeError(w, http.StatusBadRequest, "role must be owner, admin, or member")
		return
	}
	current, err := h.queries.GetWorkspaceMembership(r.Context(), store.GetWorkspaceMembershipParams{
		WorkspaceID: id,
		UserID:      userID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "Not a member of this workspace")
		return
	}
	// Only a change that takes someone OUT of owner can strip the last one.
	// That path carries the guard inside the UPDATE, for the same
	// check-then-act reason as RemoveMember. Promotions and owner->owner
	// no-ops cannot reduce the count, so they take the plain upsert.
	if current.Role == "owner" && req.Role != "owner" {
		affected, err := h.queries.DemoteWorkspaceMemberGuarded(r.Context(), store.DemoteWorkspaceMemberGuardedParams{
			Role:          req.Role,
			WorkspaceID:   id,
			UserID:        userID,
			WorkspaceID_2: id,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to update member role")
			return
		}
		if affected == 0 {
			writeError(w, http.StatusConflict, "Cannot demote the last owner")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "role": req.Role})
		return
	}
	// Every other transition (promotion, or owner->owner) cannot reduce the
	// owner count, but it must still not RESURRECT a membership: the existence
	// read above happened before any concurrent RemoveMember, and an upsert
	// would re-create the row that removal just deleted. A plain UPDATE
	// touches nothing when the row is gone.
	affected, err := h.queries.UpdateWorkspaceMemberRoleIfMember(r.Context(), store.UpdateWorkspaceMemberRoleIfMemberParams{
		Role:        req.Role,
		WorkspaceID: id,
		UserID:      userID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update member role")
		return
	}
	if affected == 0 {
		writeError(w, http.StatusNotFound, "Not a member of this workspace")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "role": req.Role})
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
	if !validWorkspaceRole(role) {
		writeError(w, http.StatusBadRequest, "role must be owner, admin, or member")
		return
	}
	// Escalation is monotone: you cannot invite someone in above your own
	// level. RequireWorkspaceRole("admin") above accepts owner OR admin, so
	// without this a workspace admin could mint an owner invite, redeem it
	// (the token is returned in the response body) and promote themselves.
	if role == "owner" && !authmw.RequireWorkspaceRole(w, r, "owner") {
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

// workspaceInviteView is the wire shape for the invite LIST path. It
// deliberately omits the token: the raw token is bearer-equivalent (it
// redeems into a live session carrying the invite's role) and is handed
// out exactly once, in the CreateInvite response. Listing it let any
// workspace member read a pending "owner" token and redeem it, since
// redemption does not bind to the invited email address.
type workspaceInviteView struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Email       string `json:"email"`
	Role        string `json:"role"`
	CreatedBy   string `json:"created_by"`
	CreatedAt   string `json:"created_at"`
	ExpiresAt   string `json:"expires_at"`
}

// ListInvites returns pending invites for the workspace.
//
// Admin-gated to match CreateInvite and DeleteInvite: a pending invite is
// a privilege-escalation primitive, not workspace metadata. The enclosing
// WorkspaceAccessMiddleware only proves membership, so without this check
// a plain member could read the invite list.
func (h *WorkspaceHandler) ListInvites(w http.ResponseWriter, r *http.Request) {
	if !authmw.RequireWorkspaceRole(w, r, "admin") {
		return
	}
	id := urlParam(r, "workspaceID")
	rows, err := h.queries.ListPendingWorkspaceInvites(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list invites")
		return
	}
	out := make([]workspaceInviteView, 0, len(rows))
	for _, row := range rows {
		out = append(out, workspaceInviteView{
			ID:          row.ID,
			WorkspaceID: row.WorkspaceID,
			Email:       row.Email,
			Role:        row.Role,
			CreatedBy:   row.CreatedBy,
			CreatedAt:   row.CreatedAt,
			ExpiresAt:   row.ExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": out})
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
