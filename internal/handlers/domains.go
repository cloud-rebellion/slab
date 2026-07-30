// Package handlers: site_domains CRUD + verification + reconcile.
//
// site_domains drives the "type a custom hostname into the admin and have
// it answer with TLS, security headers, and the right tenant's content"
// pipeline. The flow:
//
//  1. Admin POSTs the hostname. We mint a verify_token and persist the
//     row at status='pending'.
//  2. Admin points DNS at our edge IP. The /.well-known/atomic-verify/
//     handler below answers with the token; the verification poller
//     fetches that URL via plain HTTP and flips the row to 'verified'
//     when the token matches.
//  3. Reconciler issues a Let's Encrypt cert via certbot HTTP-01,
//     writes a per-domain nginx server block with the A+ headers
//     snippet + per-site CSP override, and reloads nginx.
//  4. Steady-state polling probes the live HTTPS endpoint and keeps
//     the row at 'live' (or flips back to 'error' with last_error
//     surfaced in the admin).
//
// This file owns 1, 2, and the public verify endpoint. Steps 3 and 4
// live in internal/domains (separate package) so the reconcile loop
// doesn't import handlers.
package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/bright-interaction/slab/internal/billing"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"

	"github.com/bright-interaction/slab/internal/domains"
)

// hostnamePattern matches RFC 1123 hostnames for the admin "Add domain"
// flow. We deliberately exclude ports, schemes, and trailing dots; the
// admin types a bare hostname.
var hostnamePattern = regexp.MustCompile(`^([a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}$`)

// reservedHostnameSuffixes are hostnames the operator's own infra owns;
// adding them as a tenant custom domain would wreck the wildcard server
// block. The list is intentionally small: the deployment-suffix env var
// is checked dynamically in the handler, not hardcoded here.
var reservedHostnameSuffixes = []string{
	".localhost",
	".local",
}

// maxDomainsPerSite caps how many custom domains a single site may attach, so a
// tenant cannot flood the shared single-goroutine reconciler with bogus rows.
const maxDomainsPerSite = 10

// DomainHandler manages /api/sites/{siteID}/domains routes.
type DomainHandler struct {
	queries *store.Queries
	// reconcile is fired-and-forgotten when a domain row changes state
	// so the background reconciler can pick it up immediately. nil-safe;
	// when no reconciler is wired the polling loop will still catch up
	// on its own cadence.
	reconcile func()
	// reservedSuffix is the wildcard tenant suffix the deployment
	// already routes (e.g. ".tenants.example.com"). Set via the
	// BUILT_SITE_SUFFIX env var. Custom hostnames ending in this
	// suffix would create a vhost that shadows the wildcard server
	// block, so we reject up front. Empty disables the check
	// (single-tenant deployments).
	reservedSuffix string
	// edgeIP is the public A-record target the admin shows when an
	// operator adds a custom hostname. Read once at boot from
	// ATOMICSITE_EDGE_IP. Empty falls back to "[your edge IP]" placeholder
	// in the UI so single-tenant dev runs without an edge surface stay
	// honest about what's missing.
	edgeIP string
	// cloudflareZones is the list of zone apex hostnames whose DNS we
	// auto-update via the Cloudflare API. Read from
	// ATOMICSITE_CLOUDFLARE_ZONES (comma-separated apex=zone_id list).
	// Empty means "manual A records only", which is the default
	// experience for customer-owned domains.
	cloudflareZones []string
}

// NewDomainHandler builds the handler. reconcile may be nil during boot
// before the reconcile manager is wired; nil-safe. reservedSuffix is
// the wildcard tenant suffix that custom hostnames must NOT end with.
func NewDomainHandler(queries *store.Queries, reconcile func(), reservedSuffix string) *DomainHandler {
	return &DomainHandler{queries: queries, reconcile: reconcile, reservedSuffix: strings.ToLower(strings.TrimSpace(reservedSuffix))}
}

// WithEdgeIP wires the public A-record target. Called once at boot
// from server.New so the admin UI can show the actual IP instead of a
// placeholder.
func (h *DomainHandler) WithEdgeIP(ip string) *DomainHandler {
	h.edgeIP = strings.TrimSpace(ip)
	return h
}

// WithCloudflareZones wires the list of zone apex hostnames whose DNS
// we auto-update. The admin UI uses this to show "auto-creating record"
// vs "add A record manually" hints up front.
func (h *DomainHandler) WithCloudflareZones(zones []string) *DomainHandler {
	h.cloudflareZones = zones
	return h
}

// HelpResponse is the shape returned by GET /api/sites/{siteID}/domains/help.
// Frontend reads it once on the Domains page so the "Add an A record"
// instructions show real values instead of placeholders.
type HelpResponse struct {
	// EdgeIP is the public A-record target. Empty when ATOMICSITE_EDGE_IP
	// is unset on the server (dev / single-tenant).
	EdgeIP string `json:"edge_ip"`
	// CloudflareZones is the list of zone apex hostnames whose DNS we
	// auto-update via the Cloudflare API. Empty when not configured.
	// The frontend uses this to hint "we'll auto-create the record"
	// when the new hostname falls under one of these zones.
	CloudflareZones []string `json:"cloudflare_zones"`
	// VerifyURL is the path the reconciler probes for ownership. The
	// admin can curl this once DNS propagates to confirm wiring before
	// waiting for the next sweep.
	VerifyURLTemplate string `json:"verify_url_template"`
	// DNSProofNameTemplate is the TXT record a hostname inside one of
	// CloudflareZones must carry before we will touch DNS for it. Without
	// it we would be creating the record ourselves and then verifying our
	// own handiwork, which let a tenant claim an operator hostname and get
	// a real certificate for it. A TXT record can only be created by
	// whoever already holds the name, which is the thing being proven.
	//
	// This applies ONLY inside CloudflareZones. For a tenant's own domain
	// there is nothing to prove out of band: pointing the A record at
	// EdgeIP already requires holding the zone.
	DNSProofNameTemplate string `json:"dns_proof_name_template"`
	// DNSProofValue names what goes in that record.
	DNSProofValue string `json:"dns_proof_value"`
}

// Help returns the edge IP + Cloudflare zones the admin needs to know
// to point their DNS provider at us. Public-ish (any authenticated
// member of the site can read) - no secrets, just instructions.
//
// GET /api/sites/{siteID}/domains/help
func (h *DomainHandler) Help(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	if _, err := h.queries.GetSiteByID(r.Context(), siteID); err != nil {
		writeError(w, http.StatusNotFound, "Site not found")
		return
	}
	resp := HelpResponse{
		EdgeIP:               h.edgeIP,
		CloudflareZones:      h.cloudflareZones,
		VerifyURLTemplate:    "http://{hostname}/.well-known/atomic-verify/{token}",
		DNSProofNameTemplate: domains.TXTChallengeName("{hostname}"),
		DNSProofValue:        "the domain's verify_token",
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *DomainHandler) signal() {
	if h.reconcile != nil {
		h.reconcile()
	}
}

// generateVerifyToken returns a 24-byte hex token (192 bits of entropy)
// suitable for /.well-known/atomic-verify lookups.
func generateVerifyToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// validateHostname rejects values that would break nginx routing or
// collide with reserved suffixes. Returns a normalised lowercase form.
func validateHostname(s string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	v = strings.TrimSuffix(v, ".")
	if v == "" {
		return "", errors.New("hostname is required")
	}
	if len(v) > 253 {
		return "", errors.New("hostname too long (max 253 chars)")
	}
	if strings.ContainsAny(v, " \t\r\n/?#") {
		return "", errors.New("hostname contains illegal characters")
	}
	if !hostnamePattern.MatchString(v) {
		return "", errors.New("invalid hostname format (expected example.com or sub.example.com)")
	}
	for _, suf := range reservedHostnameSuffixes {
		if strings.HasSuffix(v, suf) || v == strings.TrimPrefix(suf, ".") {
			return "", errors.New("hostname suffix is reserved")
		}
	}
	return v, nil
}

// List returns every domain row for the site.
//
// GET /api/sites/{siteID}/domains
func (h *DomainHandler) List(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	rows, err := h.queries.ListDomainsBySite(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list domains")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"domains": rows})
}

// Create attaches a hostname to the site. Returns the row plus the
// verify_token + the well-known URL the admin can curl to confirm
// DNS is wired correctly.
//
// POST /api/sites/{siteID}/domains
func (h *DomainHandler) Create(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	site, err := h.queries.GetSiteByID(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Site not found")
		return
	}
	// Plan gate: workspace plan must allow custom domains. The "solo"
	// tier doesn't (atomicsite-suffix subdomain only); studio + agency
	// do. OSS workspaces always pass (Limit returns -1). Cloud builds
	// hit this gate because every workspace lands on a paid plan after
	// signup.
	if site.WorkspaceID != "" {
		if ws, err := h.queries.GetWorkspaceByID(r.Context(), site.WorkspaceID); err == nil {
			if !billing.Allows(ws.Plan, "custom_domain") {
				writeJSON(w, http.StatusPaymentRequired, map[string]any{
					"error":     "plan_quota_exceeded",
					"error_msg": "Custom domains are not available on the current plan; upgrade to add a custom hostname.",
					"kind":      "plan_custom_domain",
					"plan":      ws.Plan,
				})
				return
			}
		}
	}
	// Cap custom domains per site. The reconciler is a single goroutine doing
	// synchronous verify + certbot per row, so a tenant adding many bogus
	// hostnames would monopolize it and delay provisioning for everyone.
	if n, err := h.queries.CountDomainsBySite(r.Context(), siteID); err == nil && n >= maxDomainsPerSite {
		writeError(w, http.StatusBadRequest, "This site has reached its custom-domain limit.")
		return
	}
	var req struct {
		Hostname    string `json:"hostname"`
		IsCanonical bool   `json:"is_canonical"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	host, err := validateHostname(req.Hostname)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Reject hostnames inside the wildcard tenant suffix. Routing for
	// those is already owned by the existing nginx wildcard + Caddy
	// {labels.3} rule, and a per-domain vhost would shadow it.
	if h.reservedSuffix != "" {
		if host == strings.TrimPrefix(h.reservedSuffix, ".") || strings.HasSuffix(host, h.reservedSuffix) {
			writeError(w, http.StatusBadRequest, "This hostname is reserved by the platform's wildcard subdomain. Use a different domain or update the site slug instead.")
			return
		}
	}
	if existing, err := h.queries.GetDomainByHostname(r.Context(), host); err == nil {
		if existing.SiteID == siteID {
			writeError(w, http.StatusConflict, "This hostname is already attached to this site")
			return
		}
		writeError(w, http.StatusConflict, "This hostname is already in use")
		return
	}
	token, err := generateVerifyToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to generate verify token")
		return
	}
	id := newID()
	canonical := int64(0)
	if req.IsCanonical {
		canonical = 1
	}
	if err := h.queries.CreateDomain(r.Context(), store.CreateDomainParams{
		ID:          id,
		SiteID:      siteID,
		Hostname:    host,
		VerifyToken: token,
		IsCanonical: canonical,
	}); err != nil {
		// UNIQUE race: a concurrent request landed the same hostname
		// between the GetDomainByHostname check above and this insert.
		// Surface as 409 (the post-check would have caught it had it
		// run after the racing commit).
		if strings.Contains(err.Error(), "UNIQUE") {
			writeError(w, http.StatusConflict, "This hostname is already in use")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to create domain row")
		return
	}
	if req.IsCanonical {
		_ = h.queries.SetDomainCanonical(r.Context(), store.SetDomainCanonicalParams{
			ID:     id,
			SiteID: siteID,
		})
	}
	row, _ := h.queries.GetDomainByID(r.Context(), id)
	h.signal()
	writeJSON(w, http.StatusCreated, row)
}

// SetCanonical flips the chosen domain to canonical for its site. The
// build pipeline reads the canonical row when site.domain is empty.
//
// POST /api/sites/{siteID}/domains/{domainID}/canonical
func (h *DomainHandler) SetCanonical(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	id := urlParam(r, "domainID")
	row, err := h.queries.GetDomainByID(r.Context(), id)
	if err != nil || row.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Domain not found")
		return
	}
	if err := h.queries.SetDomainCanonical(r.Context(), store.SetDomainCanonicalParams{
		ID:     id,
		SiteID: siteID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to set canonical")
		return
	}
	row, _ = h.queries.GetDomainByID(r.Context(), id)
	writeJSON(w, http.StatusOK, row)
}

// Delete removes a domain. Owner-only because removing the canonical
// hostname could take a customer-facing site offline. The reconciler
// observes the deletion via its periodic sweep (or via the signal()
// hook) and tears the corresponding nginx server block + cert down.
//
// DELETE /api/sites/{siteID}/domains/{domainID}
func (h *DomainHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if !authmw.RequireOwnerOrAdmin(w, r, h.queries) {
		return
	}
	siteID := urlParam(r, "siteID")
	id := urlParam(r, "domainID")
	row, err := h.queries.GetDomainByID(r.Context(), id)
	if err != nil || row.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Domain not found")
		return
	}
	if err := h.queries.DeleteDomain(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete domain")
		return
	}
	h.signal()
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// Refresh re-runs the verification + cert reconciliation for one
// domain right now (instead of waiting for the periodic poller).
// Useful when the admin just updated DNS and wants immediate feedback.
//
// POST /api/sites/{siteID}/domains/{domainID}/refresh
func (h *DomainHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	id := urlParam(r, "domainID")
	row, err := h.queries.GetDomainByID(r.Context(), id)
	if err != nil || row.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Domain not found")
		return
	}
	h.signal()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}
