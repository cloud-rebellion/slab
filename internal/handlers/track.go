package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bright-interaction/slab/internal/analytics"
	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/conversions"
	"github.com/bright-interaction/slab/internal/crmsync"
	"github.com/bright-interaction/slab/internal/identify"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/sharedsecret"
	"github.com/bright-interaction/slab/internal/store"
)

// normaliseCFCountry mirrors internal/analytics.normaliseCountry without
// importing it: validates the 2-letter alpha-2 code and drops XX/T1.
func normaliseCFCountry(s string) string {
	if len(s) != 2 {
		return ""
	}
	upper := strings.ToUpper(s)
	for _, r := range upper {
		if r < 'A' || r > 'Z' {
			return ""
		}
	}
	if upper == "XX" || upper == "T1" {
		return ""
	}
	return upper
}

// trackBodyMaxBytes caps the request body for /t/* receivers. Consent payloads
// are tiny (~1 KB); anything bigger is either malicious or a bug.
const trackBodyMaxBytes = 16 * 1024

// TrackHandler serves the public, no-auth analytics receiver mounted at /t/*.
// Two endpoints:
//
//   - POST /t/consent: fired by the CookieProof relay on consent:update or
//     consent:gpc. Upserts a visit_session keyed by (site_id, fingerprint),
//     and if the visitor opted into analytics, lifts the session from
//     anonymous to identified by stamping visitor_id + consent metadata.
//
//   - POST /t/pageview: optional SPA route-change ping. Static Astro builds
//     produce a real navigation per page so this is mostly unused for v1, but
//     the endpoint ships for future client-routed sites.
//
// Both endpoints rely on FingerprintMiddleware having already set the
// atomicsite_fp cookie on the request, so r.Context() carries a 16-hex
// fingerprint. The handlers only validate that the posted siteId actually
// belongs to a real site row; everything else is best-effort to keep the
// receiver fast and tolerant of malformed clients.
//
// Consent model (audit P2-D): analytics consent is gated CLIENT-SIDE by the
// CookieProof widget, which does not emit pageview/event/engagement/cwv until
// the visitor grants the analytics category. The server trusts that gate (it
// has no independent per-request consent state to re-check). /t/identify stores
// an email only when the visitor volunteered it (a form submission or an
// explicit, post-consent identify call), i.e. under a separate lawful basis
// than cookie consent, so it is deliberately NOT gated on the cookie-consent
// flag here. Cross-tenant linkability (audit P2-E): the consent ip_hash is
// salted with site_id (hashIPWithSalt) so the same IP hashes differently per
// tenant; the visitor fingerprint is host-scoped by the browser cookie and
// always stored alongside site_id, so cross-tenant linkage is prevented at the
// storage + query layer (every visit query is site-scoped). The raw fingerprint
// algorithm is intentionally left site-agnostic so the bidirectional CRM
// re-derivation (/t/visitor, /t/inbound) keeps matching existing cookies.
type TrackHandler struct {
	cfg       *config.Config
	queries   *store.Queries
	db        *sql.DB
	crmClient *crmsync.Client
	crmThrot  *crmsync.Throttler
	// inboundVerifier is shared across every /t/inbound request and held as
	// a long-lived pointer so the /admin/reload-secrets handler can hot-swap
	// the primary + secondary HMAC values when Dockyard rotates the shared
	// secret. Callers obtain it via InboundVerifier().
	inboundVerifier *sharedsecret.Verifier
	// consentLimiter throttles /t/consent per visitor IP to keep an attacker
	// from drowning consent_records in fake proofs. Burst 20 + 20/min
	// sustained covers any plausible legitimate flow (a real visitor changes
	// consent at most a handful of times per session).
	consentLimiter *consentRateLimiter
	// pageviewLimiter and engagementLimiter cover the other public /t/*
	// endpoints. Pageview gets a higher ceiling (legitimate SPA route
	// changes can fire dozens per session); engagement is in between
	// (one beacon per visibilitychange / pagehide).
	pageviewLimiter   *consentRateLimiter
	engagementLimiter *consentRateLimiter
	// eventLimiter caps explicit atomic.track() events. Goals fire
	// once per visitor in normal use; the default is wide enough to
	// cover SPAs that emit several events per page without inviting
	// abuse.
	eventLimiter *consentRateLimiter
	// recorder evaluates active conversion goals against /t/pageview
	// and /t/event signals and records matches. nil-safe: a nil
	// recorder no-ops, so unit tests don't have to wire it.
	recorder *conversions.Recorder
	// identifyLimiter caps explicit /t/identify calls. Identify is
	// rare in normal use (a visitor identifies once per browser /
	// device), so the budget can be tight without hurting UX.
	identifyLimiter *consentRateLimiter
	// idRecorder owns the identify flow shared with FormHandler. The
	// dependency is injected (not constructed in NewTrackHandler) so
	// the same instance can also be wired into FormHandler at server
	// boot.
	idRecorder *identify.Recorder
}

// NewTrackHandler builds a TrackHandler. The CRM sync client and throttler
// are constructed from cfg; both are no-ops when BRIGHTCRM_WEBHOOK_URL or
// BRIGHTCRM_WEBHOOK_SECRET are unset.
func NewTrackHandler(cfg *config.Config, queries *store.Queries, db *sql.DB) *TrackHandler {
	crmClient := crmsync.NewClient(cfg.BrightCRMWebhookURL, cfg.BrightCRMWebhookSecret)
	return &TrackHandler{
		cfg:               cfg,
		queries:           queries,
		db:                db,
		crmClient:         crmClient,
		crmThrot:          crmsync.NewThrottler(cfg.CRMSyncMinInterval),
		inboundVerifier:   sharedsecret.NewVerifier(cfg.BrightCRMWebhookSecret, cfg.BrightCRMWebhookSecretPrevious),
		consentLimiter:    newConsentRateLimiter(20, 20.0/60.0, 10*time.Minute),
		pageviewLimiter:   newConsentRateLimiter(100, 60.0/60.0, 10*time.Minute),
		engagementLimiter: newConsentRateLimiter(50, 30.0/60.0, 10*time.Minute),
		eventLimiter:      newConsentRateLimiter(50, 30.0/60.0, 10*time.Minute),
		identifyLimiter:   newConsentRateLimiter(10, 5.0/60.0, 10*time.Minute),
		recorder:          conversions.NewRecorder(queries),
		idRecorder:        identify.NewRecorder(queries, crmClient),
	}
}

// IdentifyRecorder exposes the shared identify recorder so other
// handlers (FormHandler, primarily) can wire the same instance and
// keep identify semantics consolidated.
func (h *TrackHandler) IdentifyRecorder() *identify.Recorder { return h.idRecorder }

// InboundVerifier exposes the verifier so the /admin/reload-secrets handler
// can call its Update method during a Dockyard-driven rotation.
func (h *TrackHandler) InboundVerifier() *sharedsecret.Verifier { return h.inboundVerifier }

// CRMClient exposes the outbound HMAC client so /admin/reload-secrets can
// swap its signing secret in place.
func (h *TrackHandler) CRMClient() *crmsync.Client { return h.crmClient }

// consentRequest is the body the embedded widget posts to /t/consent.
// Domain is the public origin the widget reported (used for the audit row);
// GPC is hoisted out of consent.method so the row can capture both
// (e.g. method=reject-all + gpc=true means GPC happened to also reject all).
type consentRequest struct {
	SiteID    string          `json:"siteId"`
	SessionID string          `json:"sessionId"`
	Domain    string          `json:"domain"`
	Consent   *consentPayload `json:"consent"`
	Page      string          `json:"page"`
	Referrer  string          `json:"referrer"`
	GPC       bool            `json:"gpc"`
}

// consentPayload mirrors the detail.consent shape emitted by CookieProof's
// <cookie-consent> element. We pull only what we store: method + categories +
// version + timestamp. GPC mode is preserved in `method` so downstream
// reporting can split GPC visitors out if needed.
type consentPayload struct {
	Version    int             `json:"version"`
	Timestamp  int64           `json:"timestamp"`
	Method     string          `json:"method"`
	Categories map[string]bool `json:"categories"`
	GPC        bool            `json:"gpc"`
}

// validConsentMethods is the enum CookieProof's widget emits and tests pin
// (see CookieProof/CLAUDE.md). Anything else is a malformed POST.
var validConsentMethods = map[string]bool{
	"accept-all":  true,
	"reject-all":  true,
	"custom":      true,
	"gpc":         true,
	"dns":         true,
	"do-not-sell": true,
}

// Consent receives consent decisions from the embedded widget. Two outcomes:
//
//  1. Always: append a row to consent_records as the GDPR proof-of-consent
//     log entry. Stores method, categories, version, page, hashed IP, GPC
//     flag, and an ISO timestamp. This is the system of record after the
//     CookieProof fold-in; the dashboard reads from this table.
//
//  2. If the consent payload grants analytics, lift the visit_session from
//     anonymous to identified (existing behavior preserved).
//
// Returns 204 on success. Bad payloads return 400 so the widget logs and the
// retry loop on the client doesn't churn forever. Storage failures are
// logged but answered 204 .  the proof is best-effort and the visitor's
// experience must not depend on our database.
func (h *TrackHandler) Consent(w http.ResponseWriter, r *http.Request) {
	// Per-IP rate limit. Public endpoint, no auth, so this is the only
	// thing standing between us and a flood. 429 Too Many Requests with a
	// short Retry-After lets a legitimate retry succeed once tokens refill.
	if h.consentLimiter != nil && !h.consentLimiter.allow(clientIP(r)) {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, "Too many consent posts; slow down")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, trackBodyMaxBytes)
	defer r.Body.Close()

	var req consentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	siteID := strings.TrimSpace(req.SiteID)
	if !isSafeSiteID(siteID) {
		writeError(w, http.StatusBadRequest, "Invalid siteId")
		return
	}

	// Site must exist. Cheap guard against random POSTs.
	site, err := h.queries.GetSiteByID(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Unknown siteId")
		return
	}

	// Domain integrity. The widget echoes its host back to us; if it
	// doesn't match the site's known domain (or the cookieproof_domain
	// override) we reject. Without this gate any third party that
	// happens to know a tenant's siteID could log-poison the proof
	// table from their own origin. Empty request domain is allowed
	// because legitimate same-origin posts can omit it; we fall back
	// to the site's configured domain a few lines down.
	if reqDomain := strings.TrimSpace(req.Domain); reqDomain != "" {
		if !consentDomainMatches(reqDomain, site.Domain, site.CookieproofDomain) {
			writeError(w, http.StatusForbidden, "Domain mismatch")
			return
		}
	}

	// Validate consent.method against the widget's emit enum. Anything else
	// is malformed and gets rejected so a bad client doesn't pollute the
	// proof log with garbage methods.
	if req.Consent == nil {
		writeError(w, http.StatusBadRequest, "Missing consent payload")
		return
	}
	method := strings.TrimSpace(req.Consent.Method)
	if !validConsentMethods[method] {
		writeError(w, http.StatusBadRequest, "Invalid consent method")
		return
	}

	fp := authmw.GetFingerprint(r)
	if fp == "" {
		// Middleware should have set this. If not, something is mis-mounted.
		slog.Warn("track: consent received without fingerprint", "site_id", siteID)
		writeError(w, http.StatusInternalServerError, "Missing fingerprint")
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	sessionID := newID()
	if err := h.queries.UpsertVisitSession(r.Context(), store.UpsertVisitSessionParams{
		ID:          sessionID,
		SiteID:      siteID,
		Fingerprint: fp,
		StartedAt:   now,
		LastSeenAt:  now,
	}); err != nil {
		slog.Error("track: upsert visit_session", "site_id", siteID, "err", err)
		writeError(w, http.StatusInternalServerError, "Failed to record session")
		return
	}

	// Resolve the audit-row inputs. Domain falls back to the site's known
	// domain when the widget didn't echo it. IP is hashed with the daily
	// salt .  never stored raw.
	auditDomain := strings.TrimSpace(req.Domain)
	if auditDomain == "" {
		auditDomain = strings.TrimSpace(site.CookieproofDomain)
	}
	if auditDomain == "" {
		auditDomain = strings.TrimSpace(site.Domain)
	}
	salt, err := getOrCreateConsentSalt(r.Context(), h.queries)
	if err != nil {
		slog.Error("track: get consent salt", "site_id", siteID, "err", err)
		// Still record with empty hash rather than dropping the proof.
		salt = ""
	}
	ipHash := ""
	if salt != "" {
		ipHash = hashIPWithSalt(clientIP(r), salt, siteID)
	}

	// Append the proof. Truncate long strings defensively .  the schema
	// columns are TEXT but we don't want anyone smuggling massive UAs.
	categoriesJSON := encodeCategories(req.Consent.Categories)
	gpcActive := int64(0)
	if req.GPC || method == "gpc" {
		gpcActive = 1
	}
	createdAtMs := time.Now().UTC().UnixMilli()
	// Privacy promise (/analytics/tracked-fields): "Raw UA is never stored."
	// Parse the UA into the coarse browser/os/device triple and persist only
	// that summary so the consent proof keeps useful context without retaining
	// the fingerprintable raw string for the 730d window.
	uaBrowser, uaOS, uaDevice := analytics.ParseUA(r.UserAgent())
	uaSummary := strings.TrimSpace(uaBrowser + " / " + uaOS + " / " + uaDevice)
	if err := h.queries.RecordConsent(r.Context(), store.RecordConsentParams{
		ID:             newID(),
		SiteID:         siteID,
		SessionID:      truncateString(req.SessionID, 64),
		Fingerprint:    fp,
		Domain:         truncateString(auditDomain, 255),
		PageUrl:        truncateString(req.Page, 2000),
		Referrer:       truncateString(req.Referrer, 2000),
		UserAgent:      truncateString(uaSummary, 500),
		IpHash:         ipHash,
		ConsentMethod:  method,
		ConsentVersion: int64(req.Consent.Version),
		CategoriesJson: categoriesJSON,
		GpcActive:      gpcActive,
		CreatedAt:      createdAtMs,
	}); err != nil {
		slog.Error("track: record consent proof", "site_id", siteID, "err", err)
		// Refuse the request so the widget's proof queue retries
		// later. Previous behaviour was to swallow the error and
		// reply 204, leaving the GDPR proof permanently lost on any
		// SQLite blip; the widget already has a localStorage-backed
		// retry queue (see CookieProof/src/core/consent-manager.ts
		// enqueueProof) so 503 is recoverable from the visitor side.
		writeError(w, http.StatusServiceUnavailable, "Could not record consent; please retry")
		return
	}

	// If the consent payload grants analytics, lift the session to identified.
	// "Identified" here means: we have a stable visitor_id we can stitch
	// future visits against. Email arrives later via form submissions; we do
	// not invent one.
	if req.Consent.Categories["analytics"] {
		liftMethod := method
		if req.Consent.GPC {
			liftMethod = "gpc"
		}
		visitorID := deriveVisitorID(siteID, fp)
		if err := h.queries.IdentifyVisitSession(r.Context(), store.IdentifyVisitSessionParams{
			VisitorID:             visitorID,
			Email:                 "",
			ConsentMethod:         liftMethod,
			ConsentCategoriesJson: categoriesJSON,
			IdentifiedAt:          now,
			LastSeenAt:            now,
			SiteID:                siteID,
			Fingerprint:           fp,
		}); err != nil {
			slog.Error("track: identify visit_session", "site_id", siteID, "err", err)
		} else {
			// Forward an "identified" event to BrightCRM. The CRM upserts a
			// contact when an email is present and otherwise drops the event;
			// we let it decide. Fire on a goroutine so the user-facing /t/consent
			// response never blocks on the CRM round-trip.
			h.crmEmitIdentified(siteID, visitorID, liftMethod, req)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// truncateString clamps s to maxLen runes (not bytes .  UTF-8 safe).
func truncateString(s string, maxLen int) string {
	if maxLen <= 0 || s == "" {
		return s
	}
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen])
}

// consentDomainMatches reports whether the domain echoed by the widget
// is one of the site's known domains. We accept an exact match, or a
// match against the site.cookieproof_domain override, or any subdomain
// of either (so `blog.example.com` is accepted when the site domain is
// `example.com`). Apex with or without `www.` is treated as the same
// domain. Comparison is case-insensitive and ignores any trailing dot.
//
// This is a domain-integrity check, not a security boundary on its own —
// the rate limiter still gates volume. Goal: stop a third party who
// happens to know a tenant's internal siteID from log-poisoning the
// proof table from their own origin.
func consentDomainMatches(reqDomain, siteDomain, cookieproofDomain string) bool {
	target := normaliseDomain(reqDomain)
	if target == "" {
		return false
	}
	for _, candidate := range []string{siteDomain, cookieproofDomain} {
		c := normaliseDomain(candidate)
		if c == "" {
			continue
		}
		if target == c {
			return true
		}
		// strip leading www. on either side and compare
		if strings.TrimPrefix(target, "www.") == strings.TrimPrefix(c, "www.") {
			return true
		}
		// subdomain match: target ends with ".<candidate>"
		if strings.HasSuffix(target, "."+strings.TrimPrefix(c, "www.")) {
			return true
		}
	}
	return false
}

// normaliseDomain lowercases the host and strips scheme, path, port, and
// any trailing dot so equality comparison is straightforward.
func normaliseDomain(s string) string {
	v := strings.ToLower(strings.TrimSpace(s))
	if v == "" {
		return ""
	}
	if i := strings.Index(v, "://"); i != -1 {
		v = v[i+3:]
	}
	if i := strings.IndexAny(v, "/?#"); i != -1 {
		v = v[:i]
	}
	if i := strings.LastIndex(v, ":"); i != -1 {
		// strip port unless it's an IPv6 literal (would have ']' before)
		if !strings.Contains(v, "]") {
			v = v[:i]
		}
	}
	return strings.TrimSuffix(v, ".")
}

// clientIP extracts the visitor's IP from the request. Prefers
// CF-Connecting-IP (Cloudflare) over X-Forwarded-For / RemoteAddr because
// the fronting proxy is the only header we know not to be spoofed.
func clientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); v != "" {
		// First entry is the closest client.
		if idx := strings.Index(v, ","); idx != -1 {
			return strings.TrimSpace(v[:idx])
		}
		return v
	}
	// RemoteAddr is "host:port"; strip the port.
	addr := r.RemoteAddr
	if i := strings.LastIndex(addr, ":"); i != -1 {
		return addr[:i]
	}
	return addr
}

// crmEmitIdentified fires an identified event toward BrightCRM. It bypasses
// the throttler so a fresh identification always lands in the activity log.
// All failures are logged; nothing is surfaced to the visitor.
//
// Consent state is included in metadata so the CRM can promote it onto the
// contact record (Phase 18). The full categories map is forwarded verbatim
// so the CRM can split marketing vs analytics consent independently and
// audit the source.
func (h *TrackHandler) crmEmitIdentified(siteID, visitorID, method string, req consentRequest) {
	if h.crmClient == nil || !h.crmClient.Enabled() {
		return
	}
	categories := map[string]bool{}
	if req.Consent != nil {
		for k, v := range req.Consent.Categories {
			categories[k] = v
		}
	}
	event := crmsync.Event{
		Event:      crmsync.EventIdentified,
		SiteID:     siteID,
		VisitorID:  visitorID,
		OccurredAt: time.Now().UTC(),
		Page: crmsync.Page{
			URL:      req.Page,
			Referrer: req.Referrer,
		},
		Metadata: map[string]any{
			"consent_method":     method,
			"consent_categories": categories,
		},
	}
	// Identified events bypass throttling per design.
	h.crmClient.SendAsync(event)
}

// pageViewRequest is the body of POST /t/pageview.
type pageViewRequest struct {
	SiteID   string `json:"siteId"`
	Path     string `json:"path"`
	Referrer string `json:"referrer"`
}

// PageView records a visit_event for SPA route changes. The static Astro
// builds Atomicsite produces don't normally need this (nginx access-logs
// catch every navigation), but client-routed sites need it to show up in
// per-page counts. Always 204 on completion.
func (h *TrackHandler) PageView(w http.ResponseWriter, r *http.Request) {
	if h.pageviewLimiter != nil && !h.pageviewLimiter.allow(clientIP(r)) {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, "Too many pageview pings; slow down")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, trackBodyMaxBytes)
	defer r.Body.Close()

	var req pageViewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	siteID := strings.TrimSpace(req.SiteID)
	if !isSafeSiteID(siteID) {
		writeError(w, http.StatusBadRequest, "Invalid siteId")
		return
	}
	if _, err := h.queries.GetSiteByID(r.Context(), siteID); err != nil {
		writeError(w, http.StatusNotFound, "Unknown siteId")
		return
	}

	fp := authmw.GetFingerprint(r)
	if fp == "" {
		writeError(w, http.StatusInternalServerError, "Missing fingerprint")
		return
	}

	path := strings.TrimSpace(req.Path)
	if path == "" {
		path = "/"
	}
	if len(path) > 1024 {
		path = path[:1024]
	}
	ref := strings.TrimSpace(req.Referrer)
	if len(ref) > 1024 {
		ref = ref[:1024]
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// Look up the existing session (if any) so visit_events.session_id can
	// link the pageview to the same row. Missing-session is fine: we still
	// log the event with empty session_id and the analytics layer handles
	// orphaned events.
	sessionID := ""
	sess, err := h.queries.GetSessionByFingerprint(r.Context(), store.GetSessionByFingerprintParams{
		SiteID:      siteID,
		Fingerprint: fp,
	})
	if err == nil {
		sessionID = sess.ID
	} else if !errors.Is(err, sql.ErrNoRows) {
		slog.Warn("track: get session for pageview", "site_id", siteID, "err", err)
	}

	// Enrich with the same UA/lang/UTM/country fields the nginx-tail parser
	// fills, but computed server-side from the request's own headers since
	// the public receiver doesn't get the nginx log line.
	browser, osName, device := analytics.ParseUA(r.UserAgent())
	lang := analytics.ParsePrimaryLanguage(r.Header.Get("Accept-Language"))
	utmSource, utmMedium, utmCampaign := analytics.ParseUTMFromPath(path)
	country := normaliseCFCountry(r.Header.Get("CF-IPCountry"))

	if err := h.queries.RecordVisitEvent(r.Context(), store.RecordVisitEventParams{
		ID:          newID(),
		SiteID:      siteID,
		Fingerprint: fp,
		SessionID:   sessionID,
		Path:        path,
		Referer:     ref,
		Status:      200,
		Ms:          0,
		Ts:          now,
		Browser:     browser,
		Os:          osName,
		Device:      device,
		Country:     country,
		Lang:        lang,
		UtmSource:   utmSource,
		UtmMedium:   utmMedium,
		UtmCampaign: utmCampaign,
	}); err != nil {
		slog.Error("track: record visit_event", "site_id", siteID, "err", err)
	}

	// Conversion goal evaluation (Phase 31.1). URL-pattern goals
	// fire here so the same /t/pageview ping produces both a visit
	// row and any matching conversion_event rows. Best-effort: a
	// failed match-eval never blocks the 204.
	if h.recorder != nil {
		h.recorder.Evaluate(r.Context(), conversions.Signal{
			SiteID:      siteID,
			Fingerprint: fp,
			SessionID:   sessionID,
			Path:        path,
			MatchType:   conversions.MatchTypeURLPattern,
			MatchValue:  path,
		})
	}

	w.WriteHeader(http.StatusNoContent)
}

// identifyRequest is the body for POST /t/identify, fired by site
// code that wants to attach an email to the current visitor (e.g.
// post-checkout, post-newsletter-confirm, custom SPAs).
type identifyRequest struct {
	SiteID   string `json:"siteId"`
	Email    string `json:"email"`
	Page     string `json:"page"`
	Title    string `json:"title"`
	Referrer string `json:"referrer"`
}

// Identify handles POST /t/identify. Validates the email shape,
// updates visit_sessions for the cookie-derived fingerprint, fires
// a best-effort crmsync event. Returns 204 on success, 404 when no
// session exists for the fingerprint (visitor never landed a
// pageview), 400 on a bad email.
func (h *TrackHandler) Identify(w http.ResponseWriter, r *http.Request) {
	if h.identifyLimiter != nil && !h.identifyLimiter.allow(clientIP(r)) {
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusTooManyRequests, "Too many identify pings; slow down")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, trackBodyMaxBytes)
	defer r.Body.Close()

	var req identifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	siteID := strings.TrimSpace(req.SiteID)
	if !isSafeSiteID(siteID) {
		writeError(w, http.StatusBadRequest, "Invalid siteId")
		return
	}
	if _, err := h.queries.GetSiteByID(r.Context(), siteID); err != nil {
		writeError(w, http.StatusNotFound, "Unknown siteId")
		return
	}
	fp := authmw.GetFingerprint(r)
	if fp == "" {
		writeError(w, http.StatusInternalServerError, "Missing fingerprint")
		return
	}

	outcome, err := h.idRecorder.Identify(r.Context(), siteID, fp, req.Email, identify.Page{
		URL:      req.Page,
		Path:     req.Page,
		Title:    req.Title,
		Referrer: req.Referrer,
	})
	if err != nil {
		if errors.Is(err, identify.ErrInvalidEmail) {
			writeError(w, http.StatusBadRequest, "Invalid email")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to identify visitor")
		return
	}
	if !outcome.Updated {
		writeError(w, http.StatusNotFound, "No session for this fingerprint")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// eventRequest is the body for POST /t/event, fired by the in-page
// beacon when site code calls window.atomic.track(name, props).
type eventRequest struct {
	SiteID     string         `json:"siteId"`
	Path       string         `json:"path"`
	Name       string         `json:"name"`
	ValueCents int64          `json:"valueCents"`
	Properties map[string]any `json:"properties"`
}

// EventTrack handles POST /t/event. It evaluates active event_name
// goals against the posted name and records a conversion_event for
// each match. Always returns 204 unless the request is malformed.
func (h *TrackHandler) EventTrack(w http.ResponseWriter, r *http.Request) {
	if h.eventLimiter != nil && !h.eventLimiter.allow(clientIP(r)) {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, "Too many event pings; slow down")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, trackBodyMaxBytes)
	defer r.Body.Close()

	var req eventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	siteID := strings.TrimSpace(req.SiteID)
	if !isSafeSiteID(siteID) {
		writeError(w, http.StatusBadRequest, "Invalid siteId")
		return
	}
	if _, err := h.queries.GetSiteByID(r.Context(), siteID); err != nil {
		writeError(w, http.StatusNotFound, "Unknown siteId")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 128 {
		writeError(w, http.StatusBadRequest, "Invalid event name")
		return
	}
	fp := authmw.GetFingerprint(r)
	if fp == "" {
		writeError(w, http.StatusInternalServerError, "Missing fingerprint")
		return
	}
	path := strings.TrimSpace(req.Path)
	if len(path) > 1024 {
		path = path[:1024]
	}

	sessionID := ""
	if sess, err := h.queries.GetSessionByFingerprint(r.Context(), store.GetSessionByFingerprintParams{
		SiteID:      siteID,
		Fingerprint: fp,
	}); err == nil {
		sessionID = sess.ID
	} else if !errors.Is(err, sql.ErrNoRows) {
		slog.Warn("track: get session for event", "site_id", siteID, "err", err)
	}

	if h.recorder != nil {
		h.recorder.Evaluate(r.Context(), conversions.Signal{
			SiteID:             siteID,
			Fingerprint:        fp,
			SessionID:          sessionID,
			Path:               path,
			MatchType:          conversions.MatchTypeEventName,
			MatchValue:         name,
			ValueOverrideCents: req.ValueCents,
			Properties:         req.Properties,
		})
	}

	w.WriteHeader(http.StatusNoContent)
}

// engagementRequest is the body the inline browser beacon sends on
// visibilitychange/hidden or pagehide. All fields are optional / best-effort:
// the beacon swallows errors so users with strict browsers (or weird privacy
// extensions) don't see console noise. Server clamps every numeric field to
// reasonable bounds before storage.
type engagementRequest struct {
	SiteID               string `json:"siteId"`
	Path                 string `json:"path"`
	ScreenW              int    `json:"screenW"`
	ScreenH              int    `json:"screenH"`
	ViewportW            int    `json:"viewportW"`
	ViewportH            int    `json:"viewportH"`
	PrefersDark          bool   `json:"prefersDark"`
	PrefersReducedMotion bool   `json:"prefersReducedMotion"`
	TimeOnPageMs         int    `json:"timeOnPageMs"`
	MaxScrollPct         int    `json:"maxScrollPct"`
}

// Engagement records a visit_engagement row with the JS-only metrics that
// server-side log tail can never see (screen / viewport / dark mode pref /
// time on page / max scroll depth). 204 on completion. Always best-effort:
// no error is fatal; the beacon doesn't read the response.
func (h *TrackHandler) Engagement(w http.ResponseWriter, r *http.Request) {
	if h.engagementLimiter != nil && !h.engagementLimiter.allow(clientIP(r)) {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, "Too many engagement beacons; slow down")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, trackBodyMaxBytes)
	defer r.Body.Close()

	var req engagementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	siteID := strings.TrimSpace(req.SiteID)
	if !isSafeSiteID(siteID) {
		writeError(w, http.StatusBadRequest, "Invalid siteId")
		return
	}
	if _, err := h.queries.GetSiteByID(r.Context(), siteID); err != nil {
		writeError(w, http.StatusNotFound, "Unknown siteId")
		return
	}

	fp := authmw.GetFingerprint(r)
	if fp == "" {
		writeError(w, http.StatusInternalServerError, "Missing fingerprint")
		return
	}

	path := strings.TrimSpace(req.Path)
	if path == "" {
		path = "/"
	}
	if len(path) > 1024 {
		path = path[:1024]
	}

	now := time.Now().UTC().Format(time.RFC3339)

	if err := h.queries.RecordVisitEngagement(r.Context(), store.RecordVisitEngagementParams{
		ID:                   newID(),
		SiteID:               siteID,
		Fingerprint:          fp,
		Path:                 path,
		Ts:                   now,
		ScreenW:              clampInt(req.ScreenW, 0, 32_000),
		ScreenH:              clampInt(req.ScreenH, 0, 32_000),
		ViewportW:            clampInt(req.ViewportW, 0, 32_000),
		ViewportH:            clampInt(req.ViewportH, 0, 32_000),
		PrefersDark:          boolToInt(req.PrefersDark),
		PrefersReducedMotion: boolToInt(req.PrefersReducedMotion),
		TimeOnPageMs:         clampInt(req.TimeOnPageMs, 0, 6*60*60*1000), // cap at 6h to ignore browser-tab-zombies
		MaxScrollPct:         clampInt(req.MaxScrollPct, 0, 100),
	}); err != nil {
		slog.Error("track: record engagement", "site_id", siteID, "err", err)
	}

	w.WriteHeader(http.StatusNoContent)
}

func clampInt(n, lo, hi int) int64 {
	if n < lo {
		return int64(lo)
	}
	if n > hi {
		return int64(hi)
	}
	return int64(n)
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// encodeCategories serialises consent.categories to a stable JSON string for
// storage in visit_sessions.consent_categories_json. nil/empty -> "{}".
// Marshal errors are unexpected for map[string]bool but get logged so the
// silent "{}" fallback is observable.
func encodeCategories(c map[string]bool) string {
	if len(c) == 0 {
		return "{}"
	}
	b, err := json.Marshal(c)
	if err != nil {
		slog.Warn("track: encodeCategories marshal failed, falling back to empty", "err", err)
		return "{}"
	}
	return string(b)
}

// deriveVisitorID returns a stable visitor identifier for an identified
// session. Re-using the fingerprint (prefixed with the site ID) keeps things
// deterministic without leaking the raw fingerprint into reports.
func deriveVisitorID(siteID, fingerprint string) string {
	return "v_" + siteID[:6] + "_" + fingerprint
}

// cwvRequest is the body the inline web-vitals measurement script
// sends to /t/cwv per (visitor, page, metric). The script fires one
// beacon per metric observation; the schema's CHECK constraint
// rejects bad metric names so a confused client gets 400 rather than
// silently writing junk. Sprint 5 quick-win (2026-05-24).
type cwvRequest struct {
	SiteID   string  `json:"siteId"`
	PagePath string  `json:"page"`
	Metric   string  `json:"metric"`
	Value    float64 `json:"value"`
	Rating   string  `json:"rating"`
	Device   string  `json:"device"`
}

var validCWVMetrics = map[string]bool{
	"LCP": true, "INP": true, "CLS": true, "FCP": true, "TTFB": true,
}

var validCWVRatings = map[string]bool{
	"":                  true,
	"good":              true,
	"needs-improvement": true,
	"poor":              true,
}

// CWV receives a single Core Web Vitals metric observation from the
// inline web-vitals measurement script. Rate-limited per IP via the
// engagement limiter (CWV beacons share that budget; both fire
// during pageviews + at most a handful per visit). Best-effort
// storage: a failed insert logs but answers 204 so a transient DB
// hiccup doesn't churn the client's retry loop.
func (h *TrackHandler) CWV(w http.ResponseWriter, r *http.Request) {
	if h.engagementLimiter != nil && !h.engagementLimiter.allow(clientIP(r)) {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, "Too many CWV beacons; slow down")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, trackBodyMaxBytes)
	defer r.Body.Close()

	var req cwvRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	siteID := strings.TrimSpace(req.SiteID)
	if !isSafeSiteID(siteID) {
		writeError(w, http.StatusBadRequest, "Invalid siteId")
		return
	}
	if _, err := h.queries.GetSiteByID(r.Context(), siteID); err != nil {
		writeError(w, http.StatusNotFound, "Unknown siteId")
		return
	}
	metric := strings.ToUpper(strings.TrimSpace(req.Metric))
	if !validCWVMetrics[metric] {
		writeError(w, http.StatusBadRequest, "Unsupported metric")
		return
	}
	rating := strings.ToLower(strings.TrimSpace(req.Rating))
	if !validCWVRatings[rating] {
		// Unknown rating = drop to empty rather than 400, so a future
		// web-vitals.js version that introduces new band labels does
		// not break the receiver.
		rating = ""
	}
	device := strings.ToLower(strings.TrimSpace(req.Device))
	if device != "desktop" && device != "mobile" && device != "tablet" {
		device = ""
	}
	path := strings.TrimSpace(req.PagePath)
	if path == "" {
		path = "/"
	}
	if len(path) > 1024 {
		path = path[:1024]
	}
	// Cap value to a sane positive range. LCP / INP in ms can reach
	// many seconds on bad networks; CLS is fractional 0..unbounded
	// but practically <= 5. 60_000 covers all observed bad numbers.
	v := req.Value
	if v < 0 {
		v = 0
	}
	if v > 60000 {
		v = 60000
	}
	if err := h.queries.InsertCWVEvent(r.Context(), store.InsertCWVEventParams{
		ID:       newID(),
		SiteID:   siteID,
		PagePath: path,
		Metric:   metric,
		Value:    v,
		Rating:   rating,
		Device:   device,
	}); err != nil {
		slog.Error("track: cwv insert", "site_id", siteID, "metric", metric, "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}
