package handlers

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/bright-interaction/slab/internal/atrest"
	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/deploy"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

// DeployHandler handles deploy_target CRUD plus default selection.
type DeployHandler struct {
	cfg     *config.Config
	queries *store.Queries
	db      *sql.DB
}

// NewDeployHandler constructs a DeployHandler.
func NewDeployHandler(cfg *config.Config, queries *store.Queries, db *sql.DB) *DeployHandler {
	return &DeployHandler{cfg: cfg, queries: queries, db: db}
}

// deployConfigCipher builds the at-rest cipher for deploy-target
// secrets (rsync private_key_pem), same key + pattern as the TOTP and
// app-install ciphers. Passthrough when ATOMICSITE_SHIELD_KEY is unset.
func deployConfigCipher(cfg *config.Config) *atrest.Cipher {
	if cfg == nil {
		return atrest.New(nil)
	}
	return atrest.New([]byte(cfg.ShieldKey))
}

// deploySecretConfigKeys is the single source of truth for which
// deploy_targets.config_json keys hold a secret. Encrypt, decrypt and
// redact all iterate this one list so they can never drift apart again:
// encryption used to name private_key_pem literally, so the dockyard
// api_key (a Dockyard control-plane credential) was written to SQLite in
// cleartext next to its encrypted sibling, and toView handed it to every
// site member.
//
// Enumerated against every registered deployer's config parser, not just
// the key that prompted the finding:
//
//	local    (deploy/local.go)    path, public_url
//	rsync    (deploy/rsync.go)    host, user, port, path, public_url,
//	                              host_key, private_key_pem
//	dockyard (deploy/dockyard.go) dockyard_url, api_key, server_id, domain,
//	                              host, user, port, private_key_pem, host_path
//
// Of those only private_key_pem and api_key are secret-valued. host_key
// is an SSH *host public* key (a pin, published by the server), and the
// rest are hostnames, paths and IDs. Adding a secret-valued key to a
// deployer means adding it here, and nowhere else.
var deploySecretConfigKeys = []string{"api_key", "private_key_pem"}

// deploySecretMask stands in for every secret value on read surfaces.
// A client that renders a target from toView and saves the form unedited
// echoes the mask back; the write paths translate that to "keep what is
// stored" (mergeDeployConfigSecrets), so redacting a value never wipes a
// configured tenant's credentials.
const deploySecretMask = "__unchanged__"

// encryptDeployConfigSecrets encrypts secret-valued config keys in
// place before the map is marshalled into targets.config_json. The
// rsync SSH private key used to sit in the DB as plaintext (the
// TODO(secrets-vault) in deploy/rsync.go). Skips values that are
// already ciphertext so a round-tripped UI save never double-encrypts.
func encryptDeployConfigSecrets(cipher *atrest.Cipher, cfg map[string]any) {
	for _, key := range deploySecretConfigKeys {
		raw, _ := cfg[key].(string)
		if raw == "" || atrest.IsEncrypted(raw) {
			continue
		}
		if enc, err := cipher.Encrypt(raw); err == nil {
			cfg[key] = enc
		} else {
			slog.Error("deploy: encrypting config secret failed; storing refused", "key", key, "err", err)
			delete(cfg, key)
		}
	}
}

// decryptDeployConfigSecrets is the read-side mirror, applied wherever
// config_json is unpacked into a deploy.Target. Legacy plaintext rows
// pass through verbatim (atrest contract) and upgrade on next save.
// That is also the migration path for rows written before api_key was
// encrypted: they carry no enc:v1: prefix, so they are handed to the
// deployer as-is and are rewritten as ciphertext the next time the
// target is saved.
func decryptDeployConfigSecrets(cipher *atrest.Cipher, cfg map[string]any) {
	for _, key := range deploySecretConfigKeys {
		raw, _ := cfg[key].(string)
		if raw == "" || !atrest.IsEncrypted(raw) {
			continue
		}
		if plain, err := cipher.Decrypt(raw); err == nil {
			cfg[key] = plain
		} else {
			slog.Error("deploy: decrypting config secret failed (key rotated or removed?)", "key", key, "err", err)
		}
	}
}

// redactDeployConfigSecrets replaces every secret-valued key with
// deploySecretMask so no read surface can hand back a credential,
// whether it is stored as ciphertext or as legacy plaintext. Values that
// are absent, empty or not a string are dropped outright, so the mask
// only ever means "a secret is set here".
func redactDeployConfigSecrets(cfg map[string]any) {
	for _, key := range deploySecretConfigKeys {
		v, present := cfg[key]
		if !present {
			continue
		}
		if raw, ok := v.(string); ok && raw != "" {
			cfg[key] = deploySecretMask
			continue
		}
		delete(cfg, key)
	}
}

// mergeDeployConfigSecrets resolves masked secrets in an inbound config
// against what is already stored. Because reads are redacted, a client
// saving an untouched form sends deploySecretMask back; that has to mean
// "unchanged", never "store the literal mask". Any other value (a new
// secret, or the key omitted entirely) is taken at face value, so
// clearing a secret still works exactly as before.
func mergeDeployConfigSecrets(incoming, stored map[string]any) {
	for _, key := range deploySecretConfigKeys {
		if raw, _ := incoming[key].(string); raw != deploySecretMask {
			continue
		}
		if prev, ok := stored[key]; ok {
			incoming[key] = prev
			continue
		}
		delete(incoming, key)
	}
}

// allowed kinds, lifted from the schema CHECK constraint so a bad request
// fails before it ever hits SQLite.
var allowedDeployKinds = map[string]bool{
	"local":    true,
	"rsync":    true,
	"dockyard": true,
}

// deployTargetView is the wire shape returned by list/get. It mirrors the DB
// row but decodes config_json into a structured field so the frontend doesn't
// have to.
type deployTargetView struct {
	ID        string         `json:"id"`
	SiteID    string         `json:"site_id"`
	Name      string         `json:"name"`
	Kind      string         `json:"kind"`
	Config    map[string]any `json:"config"`
	IsDefault bool           `json:"is_default"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
}

// toView renders a row for the API. Secret-valued config keys are
// redacted here rather than behind a role check on the read handlers, so
// no present or future caller of this function can leak a credential.
func toView(t store.DeployTarget) deployTargetView {
	cfg := map[string]any{}
	if strings.TrimSpace(t.ConfigJson) != "" {
		_ = json.Unmarshal([]byte(t.ConfigJson), &cfg)
	}
	redactDeployConfigSecrets(cfg)
	return deployTargetView{
		ID:        t.ID,
		SiteID:    t.SiteID,
		Name:      t.Name,
		Kind:      t.Kind,
		Config:    cfg,
		IsDefault: t.IsDefault == 1,
		CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt,
	}
}

// ListTargets returns all deploy targets for a site.
//
// Owner-gated like the four mutating handlers below. The view returns the
// target's config blob, which carries deployment credentials (the dockyard
// api_key, rsync host/user/path), so read access is as sensitive as write
// access here.
func (h *DeployHandler) ListTargets(w http.ResponseWriter, r *http.Request) {
	if !authmw.RequireOwnerOrAdmin(w, r, h.queries) {
		return
	}
	siteID := urlParam(r, "siteID")
	rows, err := h.queries.ListDeployTargetsBySite(r.Context(), siteID)
	if err != nil {
		slog.Error("list deploy targets", "site_id", siteID, "err", err)
		writeError(w, http.StatusInternalServerError, "Failed to list deploy targets")
		return
	}
	out := make([]deployTargetView, 0, len(rows))
	for _, t := range rows {
		out = append(out, toView(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"targets": out})
}

// GetTarget returns a single target. Owner-gated for the same reason as
// ListTargets: the response includes the target's credential-bearing config.
func (h *DeployHandler) GetTarget(w http.ResponseWriter, r *http.Request) {
	if !authmw.RequireOwnerOrAdmin(w, r, h.queries) {
		return
	}
	siteID := urlParam(r, "siteID")
	targetID := urlParam(r, "targetID")
	t, err := h.queries.GetDeployTarget(r.Context(), targetID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Deploy target not found")
		return
	}
	if t.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Deploy target not found")
		return
	}
	writeJSON(w, http.StatusOK, toView(t))
}

type createDeployTargetRequest struct {
	Name      string         `json:"name"`
	Kind      string         `json:"kind"`
	Config    map[string]any `json:"config"`
	IsDefault bool           `json:"is_default"`
}

// CreateTarget validates the payload (including a kind-specific Validate)
// and inserts a new deploy_targets row.
func (h *DeployHandler) CreateTarget(w http.ResponseWriter, r *http.Request) {
	// Deploy targets control where the built site is written (including a path
	// on our own host for the `local` kind), so only the site owner (or a
	// platform admin) may create/modify them, not every editor member.
	if !authmw.RequireOwnerOrAdmin(w, r, h.queries) {
		return
	}
	siteID := urlParam(r, "siteID")

	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var req createDeployTargetRequest
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Kind = strings.TrimSpace(req.Kind)

	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "Name is required")
		return
	}
	if !allowedDeployKinds[req.Kind] {
		writeError(w, http.StatusBadRequest, "Kind must be one of: local, rsync, dockyard")
		return
	}
	if req.Config == nil {
		req.Config = map[string]any{}
	}
	// Nothing is stored yet, so a mask echoed from some other target's
	// view resolves to "no secret" and is dropped before Validate.
	mergeDeployConfigSecrets(req.Config, nil)

	deployer, err := deploy.New(req.Kind)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	siteSlug := ""
	if site, err := h.queries.GetSiteByID(r.Context(), siteID); err == nil {
		siteSlug = site.Slug
	}
	if err := deployer.Validate(deploy.Target{
		SiteID: siteID,
		Slug:   siteSlug,
		Name:   req.Name,
		Kind:   req.Kind,
		Config: req.Config,
	}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	encryptDeployConfigSecrets(deployConfigCipher(h.cfg), req.Config)
	configJSON, err := json.Marshal(req.Config)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid config")
		return
	}

	id := newID()
	var isDefault int64
	if req.IsDefault {
		isDefault = 1
	}

	// If marking as default, do it inside a tx so we can flip other defaults
	// off in the same write.
	if req.IsDefault {
		ctx := r.Context()
		tx, err := h.db.BeginTx(ctx, nil)
		if err != nil {
			slog.Error("create deploy target: begin tx", "err", err)
			writeError(w, http.StatusInternalServerError, "Failed to create deploy target")
			return
		}
		defer tx.Rollback()
		qtx := h.queries.WithTx(tx)
		if err := qtx.ClearDefaultDeployTargets(ctx, siteID); err != nil {
			slog.Error("create deploy target: clear defaults", "site_id", siteID, "err", err)
			writeError(w, http.StatusInternalServerError, "Failed to create deploy target")
			return
		}
		if err := qtx.CreateDeployTarget(ctx, store.CreateDeployTargetParams{
			ID:         id,
			SiteID:     siteID,
			Name:       req.Name,
			Kind:       req.Kind,
			ConfigJson: string(configJSON),
			IsDefault:  isDefault,
		}); err != nil {
			handleCreateDeployTargetErr(w, err)
			return
		}
		if err := tx.Commit(); err != nil {
			slog.Error("create deploy target: commit", "err", err)
			writeError(w, http.StatusInternalServerError, "Failed to create deploy target")
			return
		}
	} else {
		if err := h.queries.CreateDeployTarget(r.Context(), store.CreateDeployTargetParams{
			ID:         id,
			SiteID:     siteID,
			Name:       req.Name,
			Kind:       req.Kind,
			ConfigJson: string(configJSON),
			IsDefault:  isDefault,
		}); err != nil {
			handleCreateDeployTargetErr(w, err)
			return
		}
	}

	t, err := h.queries.GetDeployTarget(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load created deploy target")
		return
	}
	writeJSON(w, http.StatusCreated, toView(t))
}

func handleCreateDeployTargetErr(w http.ResponseWriter, err error) {
	if strings.Contains(err.Error(), "UNIQUE") {
		writeError(w, http.StatusConflict, "A deploy target with this name already exists")
		return
	}
	if strings.Contains(err.Error(), "CHECK") {
		writeError(w, http.StatusBadRequest, "Invalid kind")
		return
	}
	slog.Error("create deploy target", "err", err)
	writeError(w, http.StatusInternalServerError, "Failed to create deploy target")
}

type updateDeployTargetRequest struct {
	Name   *string         `json:"name"`
	Kind   *string         `json:"kind"`
	Config *map[string]any `json:"config"`
}

// UpdateTarget patches an existing target row.
func (h *DeployHandler) UpdateTarget(w http.ResponseWriter, r *http.Request) {
	if !authmw.RequireOwnerOrAdmin(w, r, h.queries) {
		return
	}
	siteID := urlParam(r, "siteID")
	targetID := urlParam(r, "targetID")

	existing, err := h.queries.GetDeployTarget(r.Context(), targetID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Deploy target not found")
		return
	}
	if existing.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Deploy target not found")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var req updateDeployTargetRequest
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	name := existing.Name
	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		if trimmed == "" {
			writeError(w, http.StatusBadRequest, "Name must not be empty")
			return
		}
		name = trimmed
	}

	kind := existing.Kind
	if req.Kind != nil {
		k := strings.TrimSpace(*req.Kind)
		if !allowedDeployKinds[k] {
			writeError(w, http.StatusBadRequest, "Kind must be one of: local, rsync, dockyard")
			return
		}
		kind = k
	}

	stored := map[string]any{}
	if strings.TrimSpace(existing.ConfigJson) != "" {
		_ = json.Unmarshal([]byte(existing.ConfigJson), &stored)
	}
	config := stored
	if req.Config != nil {
		if *req.Config == nil {
			config = map[string]any{}
		} else {
			config = *req.Config
		}
		// The client only ever saw deploySecretMask for secret keys, so
		// an echoed mask restores the stored value instead of
		// overwriting the credential with the mask itself.
		mergeDeployConfigSecrets(config, stored)
	}

	deployer, err := deploy.New(kind)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	siteSlug := ""
	if site, err := h.queries.GetSiteByID(r.Context(), siteID); err == nil {
		siteSlug = site.Slug
	}
	// A round-tripped UI save may echo the stored ciphertext back;
	// decrypt first so Validate sees a real PEM, then re-encrypt for
	// persistence.
	decryptDeployConfigSecrets(deployConfigCipher(h.cfg), config)
	if err := deployer.Validate(deploy.Target{
		ID:     existing.ID,
		SiteID: siteID,
		Slug:   siteSlug,
		Name:   name,
		Kind:   kind,
		Config: config,
	}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	encryptDeployConfigSecrets(deployConfigCipher(h.cfg), config)
	configJSON, err := json.Marshal(config)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid config")
		return
	}

	if err := h.queries.UpdateDeployTarget(r.Context(), store.UpdateDeployTargetParams{
		ID:         targetID,
		Name:       name,
		Kind:       kind,
		ConfigJson: string(configJSON),
	}); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeError(w, http.StatusConflict, "A deploy target with this name already exists")
			return
		}
		slog.Error("update deploy target", "id", targetID, "err", err)
		writeError(w, http.StatusInternalServerError, "Failed to update deploy target")
		return
	}

	t, err := h.queries.GetDeployTarget(r.Context(), targetID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load updated deploy target")
		return
	}
	writeJSON(w, http.StatusOK, toView(t))
}

// DeleteTarget removes a deploy_targets row.
func (h *DeployHandler) DeleteTarget(w http.ResponseWriter, r *http.Request) {
	if !authmw.RequireOwnerOrAdmin(w, r, h.queries) {
		return
	}
	siteID := urlParam(r, "siteID")
	targetID := urlParam(r, "targetID")

	existing, err := h.queries.GetDeployTarget(r.Context(), targetID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Deploy target not found")
		return
	}
	if existing.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Deploy target not found")
		return
	}
	if err := h.queries.DeleteDeployTarget(r.Context(), targetID); err != nil {
		slog.Error("delete deploy target", "id", targetID, "err", err)
		writeError(w, http.StatusInternalServerError, "Failed to delete deploy target")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// SetDefault flips the chosen target to is_default=1 and clears the flag on
// every other target for the site, all inside a single transaction.
func (h *DeployHandler) SetDefault(w http.ResponseWriter, r *http.Request) {
	if !authmw.RequireOwnerOrAdmin(w, r, h.queries) {
		return
	}
	siteID := urlParam(r, "siteID")
	targetID := urlParam(r, "targetID")

	existing, err := h.queries.GetDeployTarget(r.Context(), targetID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Deploy target not found")
		return
	}
	if existing.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Deploy target not found")
		return
	}

	ctx := r.Context()
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		slog.Error("set default: begin tx", "err", err)
		writeError(w, http.StatusInternalServerError, "Failed to set default deploy target")
		return
	}
	defer tx.Rollback()

	qtx := h.queries.WithTx(tx)
	if err := qtx.ClearDefaultDeployTargets(ctx, siteID); err != nil {
		slog.Error("set default: clear", "site_id", siteID, "err", err)
		writeError(w, http.StatusInternalServerError, "Failed to set default deploy target")
		return
	}
	if err := qtx.SetDeployTargetDefault(ctx, targetID); err != nil {
		slog.Error("set default: set", "id", targetID, "err", err)
		writeError(w, http.StatusInternalServerError, "Failed to set default deploy target")
		return
	}
	if err := tx.Commit(); err != nil {
		slog.Error("set default: commit", "err", err)
		writeError(w, http.StatusInternalServerError, "Failed to set default deploy target")
		return
	}

	t, err := h.queries.GetDeployTarget(ctx, targetID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load deploy target")
		return
	}
	writeJSON(w, http.StatusOK, toView(t))
}
