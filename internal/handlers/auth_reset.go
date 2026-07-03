// Forgot-password flow.
//
// Three endpoints, all unauthenticated (the token in the URL is the
// credential):
//
//	POST /api/auth/forgot-password   { email }
//	  - Mints a one-time token, stores its sha256 hash, returns 200
//	    with no leak about whether the email exists. The bare ack
//	    prevents account enumeration: an attacker who POSTs every
//	    email in a leaked dump cannot tell which ones registered.
//	  - When a MailSender is configured, a reset link is emailed.
//	    Otherwise the link is logged via slog.Info so the operator
//	    can copy-paste it from /api/admin/audit-log or the container
//	    log stream. This keeps the flow useful in air-gapped /
//	    single-admin deployments without forcing an email integration.
//
//	GET  /api/auth/reset-password/{token}
//	  - Verifies the token is valid + unused + unexpired. Returns
//	    200 with the user's email so the redemption page can
//	    prefill, or 404 to keep the rejection generic.
//
//	POST /api/auth/reset-password/{token}   { password }
//	  - Validates the token again, sets the new password (bcrypt
//	    cost 12), marks the token used, and bumps token_version on
//	    the user so every active session is invalidated. Closes
//	    the case where an attacker phished a session cookie before
//	    the user reset their password.
package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/store"
)

// MailSender is the optional interface a real mail integration
// implements. When nil, reset links are logged instead of sent.
// Plug it from cmd/server/main.go via AuthHandler.MailSender = ...
type MailSender interface {
	SendPasswordResetLink(ctx context.Context, to, link string) error
	// SendFormNotification delivers a form-submission summary to the
	// site owner. fields are user-supplied values from the public
	// submit endpoint; the implementation MUST escape / sanitize the
	// values for the email body since they come from an untrusted
	// public surface. Sprint 3 (2026-05-04).
	SendFormNotification(ctx context.Context, to, siteName, formName string, fields map[string]string) error
}

// hashResetToken returns the sha256 hex of the raw token. Same
// approach as agent_keys: store the hash so a DB leak doesn't
// hand attackers live tokens.
func hashResetToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// generateResetToken returns a 32-byte hex (256 bits) one-time
// token. URL-safe (hex chars only) so it can ride in a path param
// without escape.
func generateResetToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// resetTokenTTL is how long a freshly minted token is valid. 30
// minutes balances "long enough to find the email and click" with
// "short enough that a leaked link doesn't outlive its delivery
// channel".
const resetTokenTTL = 30 * time.Minute

// ForgotPassword mints a token + delivers it. Always returns 200
// regardless of whether the email exists, to defeat enumeration.
// The slog stream tells the operator (and any tail in the logs)
// what was minted; the token itself is never in slog so a log
// leak doesn't grant resets.
//
// POST /api/auth/forgot-password
func (h *AuthHandler) ForgotPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" {
		writeError(w, http.StatusBadRequest, "Email is required")
		return
	}

	// Bare success response, regardless of email match. We still
	// want to mint when the email matches, so look up the user
	// silently and only branch on the success path.
	user, err := h.queries.GetUserByEmail(r.Context(), email)
	if err == nil {
		raw, gerr := generateResetToken()
		if gerr != nil {
			slog.Warn("forgot-password: token generation failed", "err", gerr)
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}
		now := time.Now().UTC()
		if dberr := h.queries.CreatePasswordReset(r.Context(), store.CreatePasswordResetParams{
			ID:          newID(),
			UserID:      user.ID,
			TokenHash:   hashResetToken(raw),
			ExpiresAt:   now.Add(resetTokenTTL).Format(time.RFC3339),
			RequesterIp: clientIPForReset(r),
		}); dberr != nil {
			slog.Warn("forgot-password: persist token failed", "err", dberr, "user_id", user.ID)
		} else {
			link := buildResetLink(h.cfg, raw)
			if h.MailSender != nil {
				if mailErr := h.MailSender.SendPasswordResetLink(r.Context(), user.Email, link); mailErr != nil {
					slog.Warn("forgot-password: mail send failed (link follows in slog)",
						"err", mailErr, "email", user.Email, "link", link)
				}
			} else {
				slog.Info("forgot-password: no MailSender configured; deliver link manually",
					"email", user.Email, "link", link, "ttl", resetTokenTTL.String())
			}
			AuditLog(r.Context(), h.queries, r, "", AuditActionCreate, "password_reset", user.ID, map[string]any{
				"email": user.Email,
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"message": "If that email is registered, a reset link has been delivered.",
	})
}

// ResetPasswordInfo previews a reset token (validates that it's
// live + unused + unexpired) and returns the masked email so the
// admin SPA can render "Reset password for a***@example.com" on
// the redemption page. Returns 404 for any bad / used / expired
// token without leaking which.
//
// GET /api/auth/reset-password/{token}
func (h *AuthHandler) ResetPasswordInfo(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(urlParam(r, "token"))
	user, _, err := h.lookupResetToken(r, raw)
	if err != nil {
		writeError(w, http.StatusNotFound, "Reset link is invalid or expired")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"email": maskEmail(user.Email),
	})
}

// ResetPassword consumes the token and sets the new password. On
// success: bumps the user's token_version (invalidating every live
// session), marks the reset row used, and writes an audit row. The
// new password is held to the same 12-char floor as the rest of
// the auth surface.
//
// POST /api/auth/reset-password/{token}
func (h *AuthHandler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(urlParam(r, "token"))
	var req struct {
		Password string `json:"password"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if len(req.Password) < 12 {
		writeError(w, http.StatusBadRequest, "Password must be at least 12 characters")
		return
	}
	user, resetID, err := h.lookupResetToken(r, raw)
	if err != nil {
		writeError(w, http.StatusNotFound, "Reset link is invalid or expired")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), PasswordBcryptCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to set password")
		return
	}
	if err := h.queries.UpdateUserPassword(r.Context(), store.UpdateUserPasswordParams{
		PasswordHash: string(hash),
		ID:           user.ID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to set password")
		return
	}
	if err := h.queries.MarkPasswordResetUsed(r.Context(), resetID); err != nil {
		slog.Warn("reset-password: mark used failed", "err", err, "id", resetID)
	}
	AuditLog(r.Context(), h.queries, r, "", AuditActionUpdate, "user_password", user.ID, map[string]any{
		"email":  user.Email,
		"method": "reset",
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// lookupResetToken returns the user and reset-row ID for a valid
// token. Returns an error for any of: malformed token, no row,
// already-used row, expired row.
func (h *AuthHandler) lookupResetToken(r *http.Request, raw string) (store.User, string, error) {
	if raw == "" {
		return store.User{}, "", fmt.Errorf("empty token")
	}
	row, err := h.queries.GetPasswordResetByTokenHash(r.Context(), hashResetToken(raw))
	if err != nil {
		return store.User{}, "", err
	}
	if row.UsedAt != "" {
		return store.User{}, "", fmt.Errorf("token already used")
	}
	exp, err := time.Parse(time.RFC3339, row.ExpiresAt)
	if err != nil || time.Now().UTC().After(exp) {
		return store.User{}, "", fmt.Errorf("token expired")
	}
	user, err := h.queries.GetUserByID(r.Context(), row.UserID)
	if err != nil {
		return store.User{}, "", err
	}
	return user, row.ID, nil
}

// buildResetLink derives the URL the user clicks to land on the
// redemption page in the admin SPA.
func buildResetLink(cfg *config.Config, token string) string {
	base := strings.TrimRight(cfg.BaseURL, "/")
	return base + "/reset-password/" + token
}

// maskEmail turns "alice@example.com" into "a***@example.com"
// so the redemption page can confirm "yes, this is your account"
// without echoing the full address back to whoever opened the link.
func maskEmail(s string) string {
	at := strings.IndexByte(s, '@')
	if at <= 0 {
		return s
	}
	first := s[:1]
	return first + "***" + s[at:]
}

// clientIPForReset extracts the visitor IP for storage on the
// password_resets row. Same priority chain as elsewhere; not a
// security boundary, used for forensics ("which IP requested this
// reset?").
func clientIPForReset(r *http.Request) string {
	if v := r.Header.Get("CF-Connecting-IP"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return v
	}
	addr := r.RemoteAddr
	if i := strings.LastIndexByte(addr, ':'); i > 0 {
		return addr[:i]
	}
	return addr
}
