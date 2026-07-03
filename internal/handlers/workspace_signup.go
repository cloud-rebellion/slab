// Workspace invite redemption (the /cloud/signup/{token} flow).
//
// CreateInvite has been handing out /cloud/signup/{token} URLs since
// Phase 30, but no route or redemption code existed: every workspace
// invite was a dead link. This closes the loop, mirroring the platform
// invite flow (invites.go PublicInfo/Redeem) with one difference: a
// workspace invite may target an EXISTING account, in which case the
// membership is granted without touching the account or issuing a
// session (the invitee logs in with their own credentials).

package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

// validateWorkspaceInvite fails CLOSED on unparseable expiry (matches
// validateInvite + the reset flow: an invite must never become valid
// forever through a malformed timestamp). expires_at is RFC3339 (see
// CreateInvite), unlike the platform invites' SQLite datetime format.
func validateWorkspaceInvite(row store.WorkspaceInvite) error {
	if row.UsedAt != "" {
		return errors.New("invite has already been used")
	}
	exp, err := time.Parse(time.RFC3339, row.ExpiresAt)
	if err != nil {
		return errors.New("invite has an invalid expiry")
	}
	if time.Now().UTC().After(exp) {
		return errors.New("invite has expired")
	}
	return nil
}

// SignupInfo - GET /api/cloud/signup/{token} (no auth). Lets the signup
// page render the invitee's email, workspace, and role, plus whether an
// account with that email already exists (drives which form the page
// shows: set-a-password vs log-in-to-accept).
func (h *WorkspaceHandler) SignupInfo(w http.ResponseWriter, r *http.Request) {
	token := urlParam(r, "token")
	row, err := h.queries.GetWorkspaceInviteByToken(r.Context(), token)
	if err != nil {
		writeError(w, http.StatusNotFound, "Invite not found or expired")
		return
	}
	if err := validateWorkspaceInvite(row); err != nil {
		writeError(w, http.StatusGone, err.Error())
		return
	}
	workspaceName := ""
	if ws, err := h.queries.GetWorkspaceByID(r.Context(), row.WorkspaceID); err == nil {
		workspaceName = ws.Name
	}
	_, userErr := h.queries.GetUserByEmail(r.Context(), row.Email)
	writeJSON(w, http.StatusOK, map[string]any{
		"email":            row.Email,
		"role":             row.Role,
		"workspace_name":   workspaceName,
		"existing_account": userErr == nil,
	})
}

// SignupRedeem - POST /api/cloud/signup/{token} { name, password }.
// New email: creates the account (platform role editor; workspace admin
// rights come from the membership role), grants the membership, marks
// the invite used, and logs the new user in. Existing email: grants the
// membership and marks the invite used WITHOUT touching the account or
// issuing a session; possession of the invite link must never become a
// login to someone else's account.
func (h *WorkspaceHandler) SignupRedeem(w http.ResponseWriter, r *http.Request) {
	token := urlParam(r, "token")
	row, err := h.queries.GetWorkspaceInviteByToken(r.Context(), token)
	if err != nil {
		writeError(w, http.StatusNotFound, "Invite not found or expired")
		return
	}
	if err := validateWorkspaceInvite(row); err != nil {
		writeError(w, http.StatusGone, err.Error())
		return
	}

	markUsed := func() {
		if err := h.queries.MarkWorkspaceInviteUsed(r.Context(), row.ID); err != nil {
			slog.Error("workspace signup: membership granted but MarkWorkspaceInviteUsed failed; invite remains unmarked",
				"invite_id", row.ID, "workspace_id", row.WorkspaceID, "err", err)
		}
	}

	// Existing account: grant membership only.
	if existing, err := h.queries.GetUserByEmail(r.Context(), row.Email); err == nil {
		if err := h.queries.AddWorkspaceMember(r.Context(), store.AddWorkspaceMemberParams{
			WorkspaceID: row.WorkspaceID,
			UserID:      existing.ID,
			Role:        row.Role,
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to add workspace membership")
			return
		}
		markUsed()
		writeJSON(w, http.StatusOK, map[string]any{
			"status":           "membership_added",
			"existing_account": true,
			"message":          "You already have an account with this email. Log in to access the workspace.",
		})
		return
	}

	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	// Unified policy: 12-char floor everywhere a user sets a password.
	if len(req.Password) < 12 {
		writeError(w, http.StatusBadRequest, "Password must be at least 12 characters")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), PasswordBcryptCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to hash password")
		return
	}
	userID := newID()
	if err := h.queries.CreateUser(r.Context(), store.CreateUserParams{
		ID:           userID,
		Email:        row.Email,
		PasswordHash: string(hash),
		Name:         strings.TrimSpace(req.Name),
		Role:         "editor",
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create account")
		return
	}
	if err := h.queries.AddWorkspaceMember(r.Context(), store.AddWorkspaceMemberParams{
		WorkspaceID: row.WorkspaceID,
		UserID:      userID,
		Role:        row.Role,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Account created but workspace membership failed; contact the workspace owner")
		return
	}
	markUsed()

	created, err := h.queries.GetUserByID(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load new account")
		return
	}
	authUser := &authmw.AuthUser{
		ID:           created.ID,
		Email:        created.Email,
		Name:         created.Name,
		Role:         created.Role,
		TokenVersion: created.TokenVersion,
	}
	tokenStr, err := authmw.SignToken(h.cfg, authUser)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create session")
		return
	}
	authmw.SetTokenCookie(w, h.cfg, tokenStr)
	writeJSON(w, http.StatusCreated, map[string]any{
		"status":           "account_created",
		"existing_account": false,
		"user": map[string]any{
			"id":    created.ID,
			"email": created.Email,
			"name":  created.Name,
			"role":  created.Role,
		},
	})
}
