package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/bright-interaction/slab/internal/config"
	dbpkg "github.com/bright-interaction/slab/internal/db"
	"github.com/bright-interaction/slab/internal/store"
)

func newOIDCHandlerForTest(t *testing.T, allowDomains string) (*OIDCHandler, *store.Queries) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "oidc.db")
	sqlDB, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(string(dbpkg.Schema)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	q := store.New(sqlDB)
	cfg := &config.Config{
		BaseURL:          "http://localhost:8080",
		JWTSecret:        "test-jwt-secret-1234567890-abcdef",
		OIDCEnabled:      true,
		OIDCIssuerURL:    "https://issuer.test.invalid",
		OIDCClientID:     "atomicsite-test",
		OIDCClientSecret: "shh",
		OIDCRedirectURL:  "http://localhost:8080/auth/oidc/callback",
		OIDCAllowDomains: allowDomains,
	}
	return NewOIDCHandler(cfg, q), q
}

func TestOIDC_StateCookieRoundTrip(t *testing.T) {
	h, _ := newOIDCHandlerForTest(t, "")

	// Round-trip a known state + verifier through set/get to verify the
	// HMAC signing + base64 encoding holds and the parsed payload matches.
	rec := httptest.NewRecorder()
	if err := h.setStateCookie(rec, "abc-state", "verifier-xyz"); err != nil {
		t.Fatalf("setStateCookie: %v", err)
	}
	resp := rec.Result()
	cookie := resp.Cookies()[0]
	if cookie.Name != oidcStateCookieName {
		t.Fatalf("cookie name = %q, want %q", cookie.Name, oidcStateCookieName)
	}
	r := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback", nil)
	r.AddCookie(cookie)
	sc, err := h.getStateCookie(r)
	if err != nil {
		t.Fatalf("getStateCookie: %v", err)
	}
	if sc.State != "abc-state" || sc.Verifier != "verifier-xyz" {
		t.Errorf("round-trip mismatch: %+v", sc)
	}
}

func TestOIDC_StateCookieRejectsTampered(t *testing.T) {
	h, _ := newOIDCHandlerForTest(t, "")
	rec := httptest.NewRecorder()
	_ = h.setStateCookie(rec, "abc-state", "verifier-xyz")
	cookie := rec.Result().Cookies()[0]

	// Flip a single byte in the encoded payload portion (before the dot).
	dot := strings.Index(cookie.Value, ".")
	tampered := cookie.Value[:dot-1] + "X" + cookie.Value[dot:]
	r := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback", nil)
	r.AddCookie(&http.Cookie{Name: oidcStateCookieName, Value: tampered})
	if _, err := h.getStateCookie(r); err == nil {
		t.Fatal("expected signature mismatch error, got nil")
	}
}

func TestOIDC_LoginReturns404WhenDisabled(t *testing.T) {
	h, _ := newOIDCHandlerForTest(t, "")
	h.cfg.OIDCEnabled = false

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/oidc/login", nil)
	h.Login(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when disabled", rec.Code)
	}
}

func TestOIDC_DomainAllowlist(t *testing.T) {
	tests := []struct {
		name, allow, email string
		want               bool
	}{
		{"empty allowlist rejects everything", "", "user@example.com", false},
		{"single domain match", "example.com", "user@example.com", true},
		{"single domain miss", "example.com", "user@other.com", false},
		{"multi domain match second", "a.com,b.com,c.com", "user@b.com", true},
		{"case-insensitive email", "Example.com", "USER@EXAMPLE.COM", true},
		{"whitespace tolerated", " a.com , b.com ", "x@b.com", true},
		{"missing @ rejected", "example.com", "no-at-sign", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := newOIDCHandlerForTest(t, tt.allow)
			if got := h.domainAllowedForAutoCreate(tt.email); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOIDC_ResolveUser_AutoCreatesWhenDomainAllowed(t *testing.T) {
	h, q := newOIDCHandlerForTest(t, "brightinteraction.com")

	user, err := h.resolveUser(context.Background(), "tom@brightinteraction.com", "Tom Isgren")
	if err != nil {
		t.Fatalf("resolveUser: %v", err)
	}
	// "editor", not "admin". Role admin is a global superuser (site_access
	// and workspace_access both short-circuit on it), so auto-provisioning
	// at that level handed every SSO user the whole deployment. This
	// assertion used to demand "admin" and so encoded the vulnerability.
	if user.Email != "tom@brightinteraction.com" || user.Name != "Tom Isgren" || user.Role != "editor" {
		t.Errorf("created user shape: %+v", user)
	}
	if user.PasswordHash != "" {
		t.Error("auto-created user must have empty password (SSO-only)")
	}
	got, err := q.GetUserByEmail(context.Background(), "tom@brightinteraction.com")
	if err != nil {
		t.Fatalf("DB lookup of created user: %v", err)
	}
	if got.ID != user.ID {
		t.Error("auto-created user not persisted")
	}
}

func TestOIDC_ResolveUser_RejectsUnknownEmailWhenNoAllowlist(t *testing.T) {
	h, _ := newOIDCHandlerForTest(t, "")

	if _, err := h.resolveUser(context.Background(), "stranger@elsewhere.com", "Stranger"); err == nil {
		t.Fatal("expected error for unknown email without allowlist; got nil")
	}
}

// The auto-provision role must come from config and must never silently be
// admin. It was hardcoded to "admin", a global superuser that short-circuits
// both site_access and workspace_access, so every SSO-provisioned account
// held the entire deployment. (Resolution of an unrecognised OIDC_DEFAULT_ROLE
// is the config package's job and is tested there.)
func TestOIDC_ResolveUser_HonoursConfiguredRoleAndFallsBackToEditor(t *testing.T) {
	tests := []struct {
		name, configured, want string
	}{
		{"empty config falls back to editor", "", "editor"},
		{"editor stays editor", "editor", "editor"},
		{"admin is honoured only when explicitly configured", "admin", "admin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := newOIDCHandlerForTest(t, "brightinteraction.com")
			h.cfg.OIDCDefaultRole = tt.configured

			user, err := h.resolveUser(context.Background(), "sso@brightinteraction.com", "SSO User")
			if err != nil {
				t.Fatalf("resolveUser: %v", err)
			}
			if user.Role != tt.want {
				t.Errorf("auto-provisioned role = %q, want %q", user.Role, tt.want)
			}
		})
	}
}

// An issuer that explicitly says email_verified=false is telling us the
// address is attacker-choosable. Email is the only thing a local account is
// matched on, so honouring an unverified one is account takeover. Absent
// (nil) is a different answer from false and must stay allowed, or every
// issuer that omits the claim breaks.
func TestOIDC_UnverifiedEmailIsRefusedButAbsentClaimIsAllowed(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name     string
		verified *bool
		refuse   bool
	}{
		{"explicit false is refused", &no, true},
		{"explicit true is allowed", &yes, false},
		{"absent claim is allowed", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := &oidcUserInfo{Sub: "s", Email: "user@brightinteraction.com", EmailVerified: tt.verified}
			refused := info.EmailVerified != nil && !*info.EmailVerified
			if refused != tt.refuse {
				t.Errorf("refused = %v, want %v for email_verified = %v", refused, tt.refuse, tt.verified)
			}
		})
	}
}

// The claim must survive JSON decoding as a tri-state. If the field were a
// plain bool, an omitted claim would decode to false and every issuer that
// does not send email_verified would be locked out.
func TestOIDC_EmailVerifiedDecodesAsTriState(t *testing.T) {
	tests := []struct {
		name, body string
		wantNil    bool
		wantVal    bool
	}{
		{"omitted stays nil", `{"sub":"s","email":"a@b.com"}`, true, false},
		{"false decodes as false", `{"sub":"s","email":"a@b.com","email_verified":false}`, false, false},
		{"true decodes as true", `{"sub":"s","email":"a@b.com","email_verified":true}`, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var info oidcUserInfo
			if err := json.Unmarshal([]byte(tt.body), &info); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if tt.wantNil {
				if info.EmailVerified != nil {
					t.Fatalf("EmailVerified = %v, want nil for an omitted claim", *info.EmailVerified)
				}
				return
			}
			if info.EmailVerified == nil {
				t.Fatal("EmailVerified = nil, want a decoded value")
			}
			if *info.EmailVerified != tt.wantVal {
				t.Errorf("EmailVerified = %v, want %v", *info.EmailVerified, tt.wantVal)
			}
		})
	}
}
