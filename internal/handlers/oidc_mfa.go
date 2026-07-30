package handlers

// SSO second factor. The password login enforces TOTP once a user is
// enrolled (auth.go), but the OIDC callback used to mint a full session
// straight from the issuer's word, so a user who had deliberately turned on
// TOTP had it enforced on one login path and skipped entirely on the other.
// Slab is self-hosted by third parties, so we cannot assume every
// deployment's issuer enforces MFA of its own.
//
// The flow: the callback stops short of a session, sets a short-lived
// MFA-pending cookie naming the user, and redirects to the login screen,
// which already has a TOTP input for the password path's totp_required.
// CompleteOIDCMFA then trades a valid code for the real session.
//
// The pending cookie is authentication-bearing, which the OIDC state cookie
// is not, so it is signed with an explicit purpose string. Without that
// domain separation a signed blob minted for one context could be presented
// as the other; both live under the same JWT secret.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bright-interaction/slab/internal/config"
	authmw "github.com/bright-interaction/slab/internal/middleware"
)

const (
	oidcMFACookieName = "__oidc_mfa"
	// Long enough to read a code off a phone, short enough that a stolen
	// cookie is close to useless. This is a half-authenticated state, so it
	// gets a much tighter budget than a session.
	oidcMFAMaxAge = 5 * time.Minute
	// Domain separation. Any other signed cookie minted from JWTSecret must
	// use a different purpose, or the two become interchangeable.
	oidcMFAPurpose = "oidc_mfa_pending_v1"
)

type oidcMFACookie struct {
	UserID string `json:"uid"`
	Expiry int64  `json:"exp"`
}

func hmacSignPurpose(cfg *config.Config, purpose, data string) string {
	mac := hmac.New(sha256.New, []byte(cfg.JWTSecret))
	mac.Write([]byte(purpose))
	mac.Write([]byte{0})
	mac.Write([]byte(data))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func setOIDCMFACookie(w http.ResponseWriter, cfg *config.Config, userID string) error {
	payload, err := json.Marshal(oidcMFACookie{
		UserID: userID,
		Expiry: time.Now().Add(oidcMFAMaxAge).Unix(),
	})
	if err != nil {
		return err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	http.SetCookie(w, &http.Cookie{
		Name:     oidcMFACookieName,
		Value:    encoded + "." + hmacSignPurpose(cfg, oidcMFAPurpose, encoded),
		Path:     "/",
		MaxAge:   int(oidcMFAMaxAge.Seconds()),
		HttpOnly: true,
		Secure:   !cfg.IsLocalDev(),
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// readOIDCMFACookie returns the pending user id, or an error. It fails closed
// on every branch: a bad signature, a stale expiry and an empty user id are
// all refusals. The empty-id check is load-bearing rather than defensive,
// because json.Unmarshal ignores unknown fields, so some other signed payload
// would otherwise decode into this struct as a valid-looking zero value.
func readOIDCMFACookie(r *http.Request, cfg *config.Config) (string, error) {
	c, err := r.Cookie(oidcMFACookieName)
	if err != nil {
		return "", fmt.Errorf("no mfa cookie: %w", err)
	}
	encoded, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return "", fmt.Errorf("malformed mfa cookie")
	}
	if !hmac.Equal([]byte(hmacSignPurpose(cfg, oidcMFAPurpose, encoded)), []byte(sig)) {
		return "", fmt.Errorf("mfa cookie signature mismatch")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode mfa cookie: %w", err)
	}
	var mc oidcMFACookie
	if err := json.Unmarshal(payload, &mc); err != nil {
		return "", fmt.Errorf("unmarshal mfa cookie: %w", err)
	}
	if mc.UserID == "" {
		return "", fmt.Errorf("mfa cookie has no user")
	}
	if time.Now().Unix() > mc.Expiry {
		return "", fmt.Errorf("mfa cookie expired")
	}
	return mc.UserID, nil
}

func clearOIDCMFACookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     oidcMFACookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// CompleteOIDCMFA - POST /api/auth/oidc/mfa { totp_code } -> session cookie.
//
// Lives on AuthHandler, not OIDCHandler, so it reuses the password path's
// validateTOTPForLogin (which already carries the replay guard and the
// single-use recovery-code contract) and the same login rate limiter, rather
// than growing a second, weaker copy of either.
func (h *AuthHandler) CompleteOIDCMFA(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.OIDCEnabled {
		writeError(w, http.StatusNotFound, "SSO is not configured")
		return
	}

	userID, err := readOIDCMFACookie(r, h.cfg)
	if err != nil {
		slog.Warn("oidc mfa: pending cookie rejected", "error", err)
		writeError(w, http.StatusUnauthorized, "SSO session expired, start again")
		return
	}

	// Rate limit on the pending user and the IP, same limiter and same two
	// axes as the password path, so this cannot be used as a TOTP
	// brute-force oracle.
	userKey := loginKey("oidcmfa", userID)
	ipKey := loginKey("i", clientIP(r))
	for _, key := range []string{userKey, ipKey} {
		if allowed, retry := h.loginLimiter.allow(key); !allowed {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			writeError(w, http.StatusTooManyRequests, "Too many failed attempts; try again later")
			return
		}
	}

	var req struct {
		TOTPCode string `json:"totp_code"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	user, err := h.queries.GetUserByID(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "SSO session expired, start again")
		return
	}
	// If enrolment was removed between the redirect and now there is no second
	// factor left to check, and continuing would mean issuing a session on a
	// half-finished handshake. Fail closed and make them start over.
	if user.TotpEnrolledAt == "" {
		clearOIDCMFACookie(w)
		writeError(w, http.StatusUnauthorized, "SSO session expired, start again")
		return
	}
	if !h.validateTOTPForLogin(r, user, req.TOTPCode) {
		h.loginLimiter.recordFailure(userKey)
		h.loginLimiter.recordFailure(ipKey)
		writeError(w, http.StatusUnauthorized, "Invalid MFA code")
		return
	}
	h.loginLimiter.recordSuccess(userKey)
	h.loginLimiter.recordSuccess(ipKey)

	tokenStr, err := authmw.SignToken(h.cfg, &authmw.AuthUser{
		ID:           user.ID,
		Email:        user.Email,
		Name:         user.Name,
		Role:         user.Role,
		TokenVersion: user.TokenVersion,
	})
	if err != nil {
		slog.Error("oidc mfa: sign jwt", "error", err)
		writeError(w, http.StatusInternalServerError, "Login failed")
		return
	}
	clearOIDCMFACookie(w)
	authmw.SetTokenCookie(w, h.cfg, tokenStr)
	AuditLog(r.Context(), h.queries, r, "", AuditActionCreate, "auth_oidc_mfa", user.ID, map[string]any{
		"email": user.Email,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"user": map[string]string{
			"id": user.ID, "email": user.Email, "name": user.Name, "role": user.Role,
		},
	})
}

