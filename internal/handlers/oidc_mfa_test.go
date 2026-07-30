package handlers

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/store"
)

func mfaTestConfig() *config.Config {
	return &config.Config{
		BaseURL:     "http://localhost:8080",
		JWTSecret:   "test-jwt-secret-1234567890-abcdef",
		OIDCEnabled: true,
	}
}

// The pending cookie is authentication-bearing, so every malformed or stale
// shape has to be a refusal rather than a best-effort parse.
func TestOIDCMFACookieFailsClosed(t *testing.T) {
	cfg := mfaTestConfig()

	t.Run("round trip returns the user", func(t *testing.T) {
		rec := httptest.NewRecorder()
		if err := setOIDCMFACookie(rec, cfg, "user-123"); err != nil {
			t.Fatalf("set: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		for _, c := range rec.Result().Cookies() {
			req.AddCookie(c)
		}
		got, err := readOIDCMFACookie(req, cfg)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if got != "user-123" {
			t.Errorf("user = %q, want user-123", got)
		}
	})

	t.Run("absent cookie is refused", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		if _, err := readOIDCMFACookie(req, cfg); err == nil {
			t.Error("no cookie was accepted")
		}
	})

	t.Run("tampered payload is refused", func(t *testing.T) {
		rec := httptest.NewRecorder()
		if err := setOIDCMFACookie(rec, cfg, "user-123"); err != nil {
			t.Fatalf("set: %v", err)
		}
		orig := rec.Result().Cookies()[0].Value
		_, sig, _ := strings.Cut(orig, ".")
		forged := base64.RawURLEncoding.EncodeToString([]byte(`{"uid":"attacker","exp":99999999999}`))
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.AddCookie(&http.Cookie{Name: oidcMFACookieName, Value: forged + "." + sig})
		if _, err := readOIDCMFACookie(req, cfg); err == nil {
			t.Error("a re-pointed payload under the original signature was accepted")
		}
	})

	t.Run("wrong signing key is refused", func(t *testing.T) {
		rec := httptest.NewRecorder()
		if err := setOIDCMFACookie(rec, cfg, "user-123"); err != nil {
			t.Fatalf("set: %v", err)
		}
		other := mfaTestConfig()
		other.JWTSecret = "a-completely-different-secret-value"
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		for _, c := range rec.Result().Cookies() {
			req.AddCookie(c)
		}
		if _, err := readOIDCMFACookie(req, other); err == nil {
			t.Error("a cookie signed with another key was accepted")
		}
	})

	t.Run("expired cookie is refused", func(t *testing.T) {
		payload, _ := json.Marshal(oidcMFACookie{UserID: "user-123", Expiry: time.Now().Add(-time.Second).Unix()})
		encoded := base64.RawURLEncoding.EncodeToString(payload)
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.AddCookie(&http.Cookie{
			Name:  oidcMFACookieName,
			Value: encoded + "." + hmacSignPurpose(cfg, oidcMFAPurpose, encoded),
		})
		if _, err := readOIDCMFACookie(req, cfg); err == nil {
			t.Error("an expired cookie was accepted")
		}
	})

	t.Run("empty user id is refused", func(t *testing.T) {
		payload, _ := json.Marshal(oidcMFACookie{UserID: "", Expiry: time.Now().Add(time.Minute).Unix()})
		encoded := base64.RawURLEncoding.EncodeToString(payload)
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.AddCookie(&http.Cookie{
			Name:  oidcMFACookieName,
			Value: encoded + "." + hmacSignPurpose(cfg, oidcMFAPurpose, encoded),
		})
		if _, err := readOIDCMFACookie(req, cfg); err == nil {
			t.Error("a cookie naming no user was accepted")
		}
	})
}

// The OIDC state cookie and the MFA-pending cookie are both HMACs over the
// same JWT secret, but only one of them authenticates anybody. Without the
// purpose string in the MAC input they would be interchangeable, so a state
// cookie obtained from a normal login start could be replayed as a pending
// MFA cookie. This asserts the domain separation, not just that signing works.
func TestOIDCMFAPurposeIsDomainSeparated(t *testing.T) {
	cfg := mfaTestConfig()
	const payload = "some-shared-payload"

	mfaSig := hmacSignPurpose(cfg, oidcMFAPurpose, payload)
	otherSig := hmacSignPurpose(cfg, "some_other_purpose", payload)
	if mfaSig == otherSig {
		t.Error("two purposes produced the same signature: the purpose is not in the MAC input")
	}

	// The OIDC handler's own signer must not produce the MFA signature either.
	h := NewOIDCHandler(cfg, nil)
	if h.hmacSign(payload) == mfaSig {
		t.Error("the state-cookie signer and the MFA signer agree: a state cookie could be replayed as an MFA cookie")
	}
}

// A pending cookie is not a session. Reject the request when SSO is off,
// regardless of what the cookie says.
func TestCompleteOIDCMFA404sWhenSSODisabled(t *testing.T) {
	cfg := mfaTestConfig()
	cfg.OIDCEnabled = false
	h := &AuthHandler{cfg: cfg}

	rec := httptest.NewRecorder()
	setRec := httptest.NewRecorder()
	if err := setOIDCMFACookie(setRec, cfg, "user-123"); err != nil {
		t.Fatalf("set: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/oidc/mfa", strings.NewReader(`{"totp_code":"123456"}`))
	for _, c := range setRec.Result().Cookies() {
		req.AddCookie(c)
	}
	h.CompleteOIDCMFA(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when SSO is disabled", rec.Code)
	}
}

// fakeIssuer serves the three endpoints the callback walks: discovery, token
// exchange and userinfo. emailVerified is emitted only when non-nil, so a test
// can distinguish an issuer that omits the claim from one that sends false.
func fakeIssuer(t *testing.T, email string, emailVerified *bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
			"userinfo_endpoint":      srv.URL + "/userinfo",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": "fake-access-token", "token_type": "Bearer", "expires_in": 3600,
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{"sub": "fake-sub", "email": email, "name": "Fake User"}
		if emailVerified != nil {
			body["email_verified"] = *emailVerified
		}
		writeJSON(w, http.StatusOK, body)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// callbackWithIssuer drives a full callback against a fake issuer, with a
// valid state cookie already in place.
func callbackWithIssuer(t *testing.T, h *OIDCHandler, srv *httptest.Server) *httptest.ResponseRecorder {
	t.Helper()
	h.cfg.OIDCIssuerURL = srv.URL

	stateRec := httptest.NewRecorder()
	if err := h.setStateCookie(stateRec, "the-state", "the-verifier"); err != nil {
		t.Fatalf("set state cookie: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?code=abc&state=the-state", nil)
	for _, c := range stateRec.Result().Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.Callback(rec, req)
	return rec
}

func cookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name && c.Value != "" {
			return c
		}
	}
	return nil
}

// THE security property, end to end. A user with TOTP enrolled must not come
// out of the SSO callback holding a session. Before the gate existed the
// callback signed the same atomicsite_token the password path issues, so SSO
// was a way around the second factor the user had deliberately turned on.
func TestOIDCCallbackWithheldsSessionUntilTOTPWhenEnrolled(t *testing.T) {
	h, q := newOIDCHandlerForTest(t, "brightinteraction.com")
	ctx := t.Context()

	// A user who exists AND has TOTP enrolled.
	if err := q.CreateUser(ctx, store.CreateUserParams{
		ID: "u-mfa", Email: "mfa@brightinteraction.com", PasswordHash: "x", Name: "MFA User", Role: "editor",
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := q.ConfirmUserTOTPEnrollment(ctx, store.ConfirmUserTOTPEnrollmentParams{
		TotpRecoveryJson: "[]",
		ID:               "u-mfa",
	}); err != nil {
		t.Fatalf("enrol totp: %v", err)
	}

	verified := true
	rec := callbackWithIssuer(t, h, fakeIssuer(t, "mfa@brightinteraction.com", &verified))

	if got := cookieNamed(rec, "atomicsite_token"); got != nil {
		t.Fatal("callback issued a full session cookie for a TOTP-enrolled user: SSO bypasses MFA")
	}
	pending := cookieNamed(rec, oidcMFACookieName)
	if pending == nil {
		t.Fatal("callback set no MFA-pending cookie, so the user has no way to finish")
	}
	if loc := rec.Header().Get("Location"); loc != "/login?mfa=oidc" {
		t.Errorf("redirect = %q, want /login?mfa=oidc", loc)
	}
}

// The same flow for a user WITHOUT TOTP must still work, or the gate has
// broken ordinary SSO for everyone.
func TestOIDCCallbackStillIssuesSessionWithoutTOTP(t *testing.T) {
	h, q := newOIDCHandlerForTest(t, "brightinteraction.com")
	if err := q.CreateUser(t.Context(), store.CreateUserParams{
		ID: "u-plain", Email: "plain@brightinteraction.com", PasswordHash: "x", Name: "Plain", Role: "editor",
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	verified := true
	rec := callbackWithIssuer(t, h, fakeIssuer(t, "plain@brightinteraction.com", &verified))

	if cookieNamed(rec, "atomicsite_token") == nil {
		t.Error("no session cookie for a user without TOTP: the MFA gate broke normal SSO")
	}
	if cookieNamed(rec, oidcMFACookieName) != nil {
		t.Error("an MFA-pending cookie was set for a user with no TOTP enrolled")
	}
}

// email_verified=false must be refused end to end, before any account is
// resolved or created, and must not produce a session or a pending cookie.
func TestOIDCCallbackRefusesUnverifiedEmailEndToEnd(t *testing.T) {
	h, _ := newOIDCHandlerForTest(t, "brightinteraction.com")

	notVerified := false
	rec := callbackWithIssuer(t, h, fakeIssuer(t, "attacker@brightinteraction.com", &notVerified))

	if cookieNamed(rec, "atomicsite_token") != nil {
		t.Error("an issuer-unverified email got a session")
	}
	if cookieNamed(rec, oidcMFACookieName) != nil {
		t.Error("an issuer-unverified email got an MFA-pending cookie")
	}
	if loc := rec.Header().Get("Location"); loc != "/login?error=sso_email_unverified" {
		t.Errorf("redirect = %q, want /login?error=sso_email_unverified", loc)
	}
	// And it must not have auto-provisioned the account on the way past.
	if _, err := h.queries.GetUserByEmail(t.Context(), "attacker@brightinteraction.com"); err == nil {
		t.Error("an issuer-unverified email auto-provisioned an account")
	}
}

// An issuer that omits email_verified entirely must keep working, or the fix
// locks out every deployment whose IdP does not send the claim.
func TestOIDCCallbackAllowsAbsentEmailVerifiedClaim(t *testing.T) {
	h, _ := newOIDCHandlerForTest(t, "brightinteraction.com")

	rec := callbackWithIssuer(t, h, fakeIssuer(t, "new@brightinteraction.com", nil))

	if cookieNamed(rec, "atomicsite_token") == nil {
		t.Errorf("an issuer that omits email_verified was refused; redirect = %q", rec.Header().Get("Location"))
	}
}
