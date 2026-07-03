package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/bright-interaction/slab/internal/config"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

// InvitesHandler covers Tier-1 share-link invites: an admin mints a one-time
// signup token, copies the URL out-of-band, and the invitee redeems it at
// /signup/{token} to set a password and activate their account.
type InvitesHandler struct {
	cfg     *config.Config
	queries *store.Queries
}

func NewInvitesHandler(cfg *config.Config, queries *store.Queries) *InvitesHandler {
	return &InvitesHandler{cfg: cfg, queries: queries}
}

const inviteTTL = 7 * 24 * time.Hour

func newInviteToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

type inviteView struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	Token     string `json:"token"`
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
	CreatedAt string `json:"created_at"`
}

func (h *InvitesHandler) baseURL() string {
	if h.cfg != nil && h.cfg.BaseURL != "" {
		return strings.TrimRight(h.cfg.BaseURL, "/")
	}
	return ""
}

func (h *InvitesHandler) toView(row store.Invite) inviteView {
	return inviteView{
		ID:        row.ID,
		Email:     row.Email,
		Role:      row.Role,
		Token:     row.Token,
		URL:       h.baseURL() + "/signup/" + row.Token,
		ExpiresAt: row.ExpiresAt,
		CreatedAt: row.CreatedAt,
	}
}

// Create - POST /api/admin/invites { email, role } -> { invite, url }
func (h *InvitesHandler) Create(w http.ResponseWriter, r *http.Request) {
	me := authmw.GetUser(r)
	if me == nil {
		writeError(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" || !strings.Contains(req.Email, "@") {
		writeError(w, http.StatusBadRequest, "Valid email is required")
		return
	}
	if req.Role == "" {
		req.Role = "editor"
	}
	if req.Role != "admin" && req.Role != "editor" {
		writeError(w, http.StatusBadRequest, "Role must be admin or editor")
		return
	}
	if _, err := h.queries.GetUserByEmail(r.Context(), req.Email); err == nil {
		writeError(w, http.StatusConflict, "A member with this email already exists")
		return
	}

	token, err := newInviteToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to mint token")
		return
	}
	id := newID()
	expires := time.Now().UTC().Add(inviteTTL).Format("2006-01-02 15:04:05")
	if err := h.queries.CreateInvite(r.Context(), store.CreateInviteParams{
		ID:        id,
		Email:     req.Email,
		Role:      req.Role,
		Token:     token,
		CreatedBy: me.ID,
		ExpiresAt: expires,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create invite")
		return
	}
	row, err := h.queries.GetInviteByToken(r.Context(), token)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load invite")
		return
	}
	writeJSON(w, http.StatusCreated, h.toView(row))
}

// List - GET /api/admin/invites -> [pending invites]
func (h *InvitesHandler) List(w http.ResponseWriter, r *http.Request) {
	rows, err := h.queries.ListPendingInvites(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list invites")
		return
	}
	out := make([]inviteView, 0, len(rows))
	for _, row := range rows {
		out = append(out, h.toView(row))
	}
	writeJSON(w, http.StatusOK, out)
}

// Delete - DELETE /api/admin/invites/{inviteID}
func (h *InvitesHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := urlParam(r, "inviteID")
	if err := h.queries.DeleteInvite(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete invite")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// PublicInfo - GET /api/auth/signup/{token} -> { email, role } (no auth).
// Returns 404 for unknown / expired / used tokens. Lets the signup page
// render the invitee's email + role before they set a password.
func (h *InvitesHandler) PublicInfo(w http.ResponseWriter, r *http.Request) {
	token := urlParam(r, "token")
	row, err := h.queries.GetInviteByToken(r.Context(), token)
	if err != nil {
		writeError(w, http.StatusNotFound, "Invite not found or expired")
		return
	}
	if err := validateInvite(row); err != nil {
		writeError(w, http.StatusGone, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"email": row.Email,
		"role":  row.Role,
	})
}

// Redeem - POST /api/auth/signup/{token} { name, password } -> { user } + cookie.
// Creates the user, marks invite used, and logs them in.
func (h *InvitesHandler) Redeem(w http.ResponseWriter, r *http.Request) {
	token := urlParam(r, "token")
	row, err := h.queries.GetInviteByToken(r.Context(), token)
	if err != nil {
		writeError(w, http.StatusNotFound, "Invite not found or expired")
		return
	}
	if err := validateInvite(row); err != nil {
		writeError(w, http.StatusGone, err.Error())
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
	// Unified policy: 12-char floor everywhere a user sets a password
	// (matches /api/auth/change-password and the reset-password CLI).
	// Single source of truth so an invited editor can't slip in with
	// a weaker password than the admin floor.
	if len(req.Password) < 12 {
		writeError(w, http.StatusBadRequest, "Password must be at least 12 characters")
		return
	}
	if _, err := h.queries.GetUserByEmail(r.Context(), row.Email); err == nil {
		writeError(w, http.StatusConflict, "A member with this email already exists")
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
		Role:         row.Role,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create account")
		return
	}
	if err := h.queries.MarkInviteUsed(r.Context(), row.ID); err != nil {
		// Non-fatal (the user is created), but an unmarked invite stays
		// redeemable-looking forever, so it must be loud.
		slog.Error("invites: user created but MarkInviteUsed failed; invite remains unmarked", "invite_id", row.ID, "err", err)
	}

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
		"user": map[string]any{
			"id":    created.ID,
			"email": created.Email,
			"name":  created.Name,
			"role":  created.Role,
		},
	})
}

func validateInvite(row store.Invite) error {
	if row.UsedAt != "" {
		return errors.New("invite has already been used")
	}
	exp, err := time.Parse("2006-01-02 15:04:05", row.ExpiresAt)
	if err != nil {
		// Fail CLOSED: an unparseable expiry must not pass as "valid forever"
		// (the password-reset path already fails closed; this matches it).
		return errors.New("invite has an invalid expiry")
	}
	if time.Now().UTC().After(exp) {
		return errors.New("invite has expired")
	}
	return nil
}
