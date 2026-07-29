package handlers

// OIDC SSO (#5, 2026-05-10). Authorization Code + PKCE against a
// generic OIDC provider; production stack uses Zitadel at
// auth.example.com. State + PKCE verifier ride a single
// HMAC-signed cookie so we don't need server-side session storage.
// On callback success we mint the same atomicsite_token JWT that the
// password login issues, so the rest of the app sees an SSO user
// identically to a password user. Account provisioning: SSO requires a
// pre-existing user by default; OIDC_ALLOW_DOMAINS allows auto-create
// for emails whose domain matches the allowlist.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bright-interaction/slab/internal/config"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

const (
	oidcStateCookieName = "__oidc_state"
	oidcStateMaxAgeSec  = 300
)

type OIDCHandler struct {
	cfg     *config.Config
	queries *store.Queries
}

func NewOIDCHandler(cfg *config.Config, q *store.Queries) *OIDCHandler {
	return &OIDCHandler{cfg: cfg, queries: q}
}

type oidcWellKnown struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	EndSessionEndpoint    string `json:"end_session_endpoint"`
}

type oidcTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

type oidcUserInfo struct {
	Sub string `json:"sub"`
	// Email is the identity we key the whole session on, so whether the
	// issuer actually verified it matters. A pointer, not a bool: absent
	// and false are different answers. Plenty of issuers omit the claim
	// entirely, and treating that omission as "unverified" would lock out
	// working deployments, but an issuer that explicitly says false is
	// telling us the address is attacker-choosable. Same tri-state the
	// mesh hub uses.
	EmailVerified *bool  `json:"email_verified"`
	Email         string `json:"email"`
	Name          string `json:"name"`
	GivenName     string `json:"given_name"`
	FamilyName    string `json:"family_name"`
	Picture       string `json:"picture"`
}

type oidcStateCookie struct {
	State    string `json:"state"`
	Verifier string `json:"verifier"`
}

// Config exposes OIDC-enabled status to the SPA so the login page can
// conditionally render the "Sign in with SSO" button. No secrets leak.
func (h *OIDCHandler) Config(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"oidc_enabled": h.cfg.OIDCEnabled,
	})
}

func (h *OIDCHandler) Login(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.OIDCEnabled {
		writeError(w, http.StatusNotFound, "SSO is not configured")
		return
	}

	wk, err := h.discover(r.Context())
	if err != nil {
		slog.Error("oidc: discovery failed", "error", err)
		writeError(w, http.StatusInternalServerError, "SSO configuration error")
		return
	}

	verifier, err := oidcRandomString(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	challenge := oidcCodeChallenge(verifier)

	state, err := oidcRandomString(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := h.setStateCookie(w, state, verifier); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	params := url.Values{
		"response_type":         {"code"},
		"client_id":             {h.cfg.OIDCClientID},
		"redirect_uri":          {h.cfg.OIDCRedirectURL},
		"scope":                 {"openid email profile"},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	http.Redirect(w, r, wk.AuthorizationEndpoint+"?"+params.Encode(), http.StatusFound)
}

func (h *OIDCHandler) Callback(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.OIDCEnabled {
		writeError(w, http.StatusNotFound, "SSO is not configured")
		return
	}

	q := r.URL.Query()
	code := q.Get("code")
	state := q.Get("state")
	if errParam := q.Get("error"); errParam != "" {
		slog.Warn("oidc: provider returned error", "error", errParam, "description", q.Get("error_description"))
		http.Redirect(w, r, "/login?error=sso_denied", http.StatusFound)
		return
	}
	if code == "" || state == "" {
		http.Redirect(w, r, "/login?error=sso_invalid", http.StatusFound)
		return
	}

	sc, err := h.getStateCookie(r)
	if err != nil || sc.State == "" || !hmac.Equal([]byte(sc.State), []byte(state)) {
		slog.Warn("oidc: state mismatch or missing cookie", "error", err)
		http.Redirect(w, r, "/login?error=sso_invalid", http.StatusFound)
		return
	}
	clearOIDCStateCookie(w)

	wk, err := h.discover(r.Context())
	if err != nil {
		slog.Error("oidc: discovery failed", "error", err)
		http.Redirect(w, r, "/login?error=sso_error", http.StatusFound)
		return
	}

	tokenResp, err := h.exchangeCode(r.Context(), wk.TokenEndpoint, code, sc.Verifier)
	if err != nil {
		slog.Error("oidc: token exchange failed", "error", err)
		http.Redirect(w, r, "/login?error=sso_error", http.StatusFound)
		return
	}

	info, err := h.fetchUserInfo(r.Context(), wk.UserinfoEndpoint, tokenResp.AccessToken)
	if err != nil {
		slog.Error("oidc: userinfo failed", "error", err)
		http.Redirect(w, r, "/login?error=sso_error", http.StatusFound)
		return
	}
	if info.Email == "" {
		http.Redirect(w, r, "/login?error=sso_no_email", http.StatusFound)
		return
	}
	// An issuer that explicitly reports email_verified=false is telling us
	// the address is attacker-choosable at that IdP. Since email is the
	// only thing we match a local account on, honouring it would let anyone
	// who can set an unverified address to a real user's email take over
	// that account, or auto-provision one on an allowlisted domain they do
	// not control. Refuse before resolveUser, so this covers both the
	// existing-user and the auto-create path.
	if info.EmailVerified != nil && !*info.EmailVerified {
		slog.Warn("oidc: issuer reports email not verified, refusing login", "email", strings.ToLower(info.Email), "sub", info.Sub)
		http.Redirect(w, r, "/login?error=sso_email_unverified", http.StatusFound)
		return
	}

	email := strings.ToLower(info.Email)
	user, err := h.resolveUser(r.Context(), email, info.Name)
	if err != nil {
		slog.Warn("oidc: resolve user", "email", email, "error", err)
		http.Redirect(w, r, "/login?error=sso_no_account", http.StatusFound)
		return
	}

	authUser := &authmw.AuthUser{
		ID:           user.ID,
		Email:        user.Email,
		Name:         user.Name,
		Role:         user.Role,
		TokenVersion: user.TokenVersion,
	}
	tokenStr, err := authmw.SignToken(h.cfg, authUser)
	if err != nil {
		slog.Error("oidc: sign jwt", "error", err)
		http.Redirect(w, r, "/login?error=sso_error", http.StatusFound)
		return
	}
	authmw.SetTokenCookie(w, h.cfg, tokenStr)
	AuditLog(r.Context(), h.queries, r, "", AuditActionCreate, "auth_oidc_login", user.ID, map[string]any{
		"email": email, "sub": info.Sub,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// resolveUser looks up the user by email. When OIDCAllowDomains lists
// the email's domain and no user exists, a passwordless user is
// created (PasswordHash empty so the password flow can't authenticate
// them; only the OIDC path can issue a session). This matches the
// "SSO-only user" pattern used by Zitadel-fronted deployments where
// user provisioning is delegated to the IdP.
//
// The role comes from OIDCDefaultRole and defaults to "editor". It was
// hardcoded to "admin", which is a global superuser here: both
// site_access and workspace_access short-circuit on role == "admin", so
// every auto-provisioned SSO user silently held access to every site and
// workspace in the deployment. Auto-provisioning is a convenience, so it
// gets the least privilege that still makes the account usable, and an
// operator promotes deliberately through the members API.
func (h *OIDCHandler) resolveUser(ctx context.Context, email, name string) (store.User, error) {
	user, err := h.queries.GetUserByEmail(ctx, email)
	if err == nil {
		return user, nil
	}
	if !h.domainAllowedForAutoCreate(email) {
		return store.User{}, fmt.Errorf("user not found and domain not in OIDC_ALLOW_DOMAINS")
	}
	id := newID()
	if name == "" {
		name = email
	}
	role := h.cfg.OIDCDefaultRole
	if role == "" {
		role = "editor"
	}
	if err := h.queries.CreateUser(ctx, store.CreateUserParams{
		ID:           id,
		Email:        email,
		PasswordHash: "",
		Name:         name,
		Role:         role,
	}); err != nil {
		return store.User{}, fmt.Errorf("create sso user: %w", err)
	}
	return h.queries.GetUserByEmail(ctx, email)
}

func (h *OIDCHandler) domainAllowedForAutoCreate(email string) bool {
	if h.cfg.OIDCAllowDomains == "" {
		return false
	}
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return false
	}
	emailDomain := strings.ToLower(email[at+1:])
	for _, d := range strings.Split(h.cfg.OIDCAllowDomains, ",") {
		if strings.EqualFold(strings.TrimSpace(d), emailDomain) {
			return true
		}
	}
	return false
}

func (h *OIDCHandler) discover(ctx context.Context) (*oidcWellKnown, error) {
	discoveryURL := strings.TrimRight(h.cfg.OIDCIssuerURL, "/") + "/.well-known/openid-configuration"
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build discovery request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery returned %d", resp.StatusCode)
	}
	var wk oidcWellKnown
	if err := json.NewDecoder(resp.Body).Decode(&wk); err != nil {
		return nil, fmt.Errorf("decode discovery: %w", err)
	}
	if wk.AuthorizationEndpoint == "" || wk.TokenEndpoint == "" || wk.UserinfoEndpoint == "" {
		return nil, fmt.Errorf("discovery missing endpoints")
	}
	return &wk, nil
}

func (h *OIDCHandler) exchangeCode(ctx context.Context, tokenURL, code, verifier string) (*oidcTokenResponse, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {h.cfg.OIDCRedirectURL},
		"client_id":     {h.cfg.OIDCClientID},
		"client_secret": {h.cfg.OIDCClientSecret},
		"code_verifier": {verifier},
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var tokenResp oidcTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if tokenResp.Error != "" {
		return nil, fmt.Errorf("token error: %s: %s", tokenResp.Error, tokenResp.ErrorDesc)
	}
	if tokenResp.AccessToken == "" {
		return nil, fmt.Errorf("empty access token")
	}
	return &tokenResp, nil
}

func (h *OIDCHandler) fetchUserInfo(ctx context.Context, userinfoURL, accessToken string) (*oidcUserInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userinfoURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build userinfo request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("userinfo request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo returned %d", resp.StatusCode)
	}
	var info oidcUserInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode userinfo: %w", err)
	}
	return &info, nil
}

func (h *OIDCHandler) setStateCookie(w http.ResponseWriter, state, verifier string) error {
	payload, err := json.Marshal(oidcStateCookie{State: state, Verifier: verifier})
	if err != nil {
		return err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	sig := h.hmacSign(encoded)
	http.SetCookie(w, &http.Cookie{
		Name:     oidcStateCookieName,
		Value:    encoded + "." + sig,
		Path:     "/",
		MaxAge:   oidcStateMaxAgeSec,
		HttpOnly: true,
		Secure:   !h.cfg.IsLocalDev(),
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func (h *OIDCHandler) getStateCookie(r *http.Request) (*oidcStateCookie, error) {
	c, err := r.Cookie(oidcStateCookieName)
	if err != nil {
		return nil, fmt.Errorf("no state cookie: %w", err)
	}
	parts := strings.SplitN(c.Value, ".", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("malformed state cookie")
	}
	encoded, sig := parts[0], parts[1]
	if !hmac.Equal([]byte(h.hmacSign(encoded)), []byte(sig)) {
		return nil, fmt.Errorf("state cookie signature mismatch")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode state cookie: %w", err)
	}
	var sc oidcStateCookie
	if err := json.Unmarshal(payload, &sc); err != nil {
		return nil, fmt.Errorf("unmarshal state cookie: %w", err)
	}
	return &sc, nil
}

func clearOIDCStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     oidcStateCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *OIDCHandler) hmacSign(data string) string {
	mac := hmac.New(sha256.New, []byte(h.cfg.JWTSecret))
	mac.Write([]byte(data))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func oidcRandomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func oidcCodeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
