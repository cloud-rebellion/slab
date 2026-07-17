// TOTP MFA flow.
//
//	POST /api/auth/totp/setup       (auth required)
//	  Stages a fresh secret on the user row, returns the
//	  base32 secret + otpauth:// URI. The admin SPA renders the
//	  URI as a QR code. Until ConfirmEnrollment fires, the secret
//	  is "staged" (saved on the row) but enrollment isn't locked
//	  in, so a botched scan can be retried by calling Setup again.
//
//	POST /api/auth/totp/verify      (auth required)
//	  Body: { code }. Validates against the staged secret. On
//	  success: writes 8 single-use recovery codes (returned
//	  plaintext ONCE), bumps token_version (every other session
//	  loses its cookie), and locks enrollment in.
//
//	POST /api/auth/totp/disable     (auth required)
//	  Body: { current_password, code? }. Re-confirms password to
//	  stop a stolen-cookie attacker from disabling MFA. Clears
//	  the secret + recovery codes + enrollment timestamp.
//
// Login enforcement: the existing Login handler doesn't yet read
// totp_enrolled_at; this commit adds that gate so an enrolled
// user must supply a valid code (or recovery code) at login.
package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/bright-interaction/slab/internal/atrest"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
	"github.com/bright-interaction/slab/internal/totp"
)

const totpIssuer = "Atomic Site"

// secretCipher builds the at-rest cipher from ATOMICSITE_SHIELD_KEY. A
// nil cfg or unset key yields a passthrough cipher (stores/reads
// plaintext), so deployments without the key are never locked out.
func (h *AuthHandler) secretCipher() *atrest.Cipher {
	if h.cfg == nil {
		return atrest.New(nil)
	}
	return atrest.New([]byte(h.cfg.ShieldKey))
}

// TOTPSetup stages a new secret + returns the otpauth URI for QR
// rendering. Re-callable: each call replaces the staged secret so
// a misclicked enrollment can be redone before Verify locks it in.
//
// POST /api/auth/totp/setup
func (h *AuthHandler) TOTPSetup(w http.ResponseWriter, r *http.Request) {
	user := authmw.GetUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	row, err := h.queries.GetUserByID(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load user")
		return
	}
	// Already enrolled? Refuse to issue a new secret without
	// disabling first; otherwise an attacker who phished a session
	// cookie could rotate the secret silently and lock the real
	// owner out of their own MFA.
	if row.TotpEnrolledAt != "" {
		writeError(w, http.StatusConflict, "MFA is already enrolled. Disable first to re-enroll.")
		return
	}
	secret, err := totp.GenerateSecret()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to generate TOTP secret")
		return
	}
	// Store the seed encrypted at rest; the plaintext below is returned
	// to the user (for the QR / manual entry) but never persisted raw.
	encSecret, err := h.secretCipher().Encrypt(secret)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to protect TOTP secret")
		return
	}
	if err := h.queries.SetUserTOTPSecret(r.Context(), store.SetUserTOTPSecretParams{
		TotpSecret: encSecret,
		ID:         user.ID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to stage TOTP secret")
		return
	}
	uri := totp.GenerateOTPAuthURI(row.Email, totpIssuer, secret)
	writeJSON(w, http.StatusOK, map[string]string{
		"secret":      secret,
		"otpauth_uri": uri,
		"hint":        "Scan the QR / paste the secret into your authenticator app, then call /api/auth/totp/verify with a 6-digit code.",
	})
}

// TOTPVerify locks in enrollment after a successful first code.
// Returns the 8 plaintext recovery codes ONCE. Bumps token_version
// so the caller has to re-login (with the new MFA gate) on the
// next request, which makes the contract obvious.
//
// POST /api/auth/totp/verify  { code }
func (h *AuthHandler) TOTPVerify(w http.ResponseWriter, r *http.Request) {
	user := authmw.GetUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	row, err := h.queries.GetUserByID(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load user")
		return
	}
	if row.TotpSecret == "" {
		writeError(w, http.StatusBadRequest, "No TOTP secret staged. Call /api/auth/totp/setup first.")
		return
	}
	stagedSecret, err := h.secretCipher().Decrypt(row.TotpSecret)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to read TOTP secret")
		return
	}
	enrollStep, ok := totp.ValidateCode(stagedSecret, strings.TrimSpace(req.Code))
	if !ok {
		writeError(w, http.StatusUnauthorized, "Invalid TOTP code")
		return
	}
	plaintext, hashed, err := totp.GenerateRecoveryCodes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to generate recovery codes")
		return
	}
	hashedJSON, _ := json.Marshal(hashed)
	if err := h.queries.ConfirmUserTOTPEnrollment(r.Context(), store.ConfirmUserTOTPEnrollmentParams{
		TotpRecoveryJson: string(hashedJSON),
		ID:               user.ID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to lock in enrollment")
		return
	}
	// Consume the enrollment code so it cannot be replayed for the first login.
	_ = h.queries.UpdateUserTOTPLastStep(r.Context(), store.UpdateUserTOTPLastStepParams{TotpLastStep: enrollStep, ID: user.ID})
	AuditLog(r.Context(), h.queries, r, "", AuditActionUpdate, "user_mfa", user.ID, map[string]any{
		"action": "enrolled",
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "enrolled",
		"recovery_codes": plaintext,
		"hint":           "Save these recovery codes now. Each works once and cannot be retrieved later.",
	})
}

// TOTPDisable clears MFA after re-confirming the user's password.
// Body: { current_password }. The valid-MFA-code requirement is
// already implicit: this endpoint is auth-required and the user's
// session was issued under the MFA-enforced login flow, so a
// session-bearer who triggers this had MFA at login. The password
// re-prompt is the brake against a phished cookie.
//
// POST /api/auth/totp/disable  { current_password }
func (h *AuthHandler) TOTPDisable(w http.ResponseWriter, r *http.Request) {
	user := authmw.GetUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	var req struct {
		CurrentPassword string `json:"current_password"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	row, err := h.queries.GetUserByID(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load user")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(row.PasswordHash), []byte(req.CurrentPassword)); err != nil {
		writeError(w, http.StatusUnauthorized, "Current password is incorrect")
		return
	}
	if err := h.queries.ClearUserTOTP(r.Context(), user.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to disable MFA")
		return
	}
	AuditLog(r.Context(), h.queries, r, "", AuditActionUpdate, "user_mfa", user.ID, map[string]any{
		"action": "disabled",
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "disabled"})
}

// TOTPStatus returns the MFA state for the current user. The admin
// SPA reads this on the security settings page to decide between
// "Enable MFA" (not enrolled) and "Reset / disable" (enrolled).
//
// GET /api/auth/totp/status
func (h *AuthHandler) TOTPStatus(w http.ResponseWriter, r *http.Request) {
	user := authmw.GetUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	row, err := h.queries.GetUserByID(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load user")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enrolled":    row.TotpEnrolledAt != "",
		"enrolled_at": row.TotpEnrolledAt,
		"has_staged":  row.TotpSecret != "" && row.TotpEnrolledAt == "",
	})
}

// validateTOTPForLogin is the helper Login() invokes when an
// enrolled user is signing in. Accepts either a current 6-digit
// code or a single-use recovery code. On a recovery match, the
// matched hash is consumed (rewritten back to the user row) so
// each recovery code works exactly once.
func (h *AuthHandler) validateTOTPForLogin(r *http.Request, user store.User, supplied string) bool {
	supplied = strings.TrimSpace(supplied)
	if supplied == "" {
		return false
	}
	// Prefer fast TOTP path. The stored seed is encrypted at rest.
	if storedSecret, err := h.secretCipher().Decrypt(user.TotpSecret); err == nil {
		if step, ok := totp.ValidateCode(storedSecret, supplied); ok {
			// Reject a replay of an already-accepted code (a captured code stays
			// valid for the ~30-60s drift window otherwise). Persist the new step
			// BEFORE accepting, and fail closed if the persist fails, mirroring the
			// recovery-code single-use contract below.
			if step <= user.TotpLastStep {
				return false
			}
			if err := h.queries.UpdateUserTOTPLastStep(r.Context(), store.UpdateUserTOTPLastStepParams{TotpLastStep: step, ID: user.ID}); err != nil {
				return false
			}
			return true
		}
	}
	// Fall back to recovery code consumption.
	var hashes []string
	if err := json.Unmarshal([]byte(user.TotpRecoveryJson), &hashes); err != nil {
		return false
	}
	remaining, ok := totp.ValidateRecoveryCode(hashes, supplied)
	if !ok {
		return false
	}
	remJSON, _ := json.Marshal(remaining)
	if err := h.queries.UpdateUserTOTPRecovery(r.Context(), store.UpdateUserTOTPRecoveryParams{
		TotpRecoveryJson: string(remJSON),
		ID:               user.ID,
	}); err != nil {
		// Refuse the login to keep the recovery-code single-use
		// contract: if we can't persist the consumption, the
		// next login could re-use the same code.
		return false
	}
	return true
}
