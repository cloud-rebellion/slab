package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/bright-interaction/slab/internal/atrest"
	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/store"
)

// Regression guard for audit finding 8 (2026-08-11): the Dockyard
// control-plane api_key was written to deploy_targets.config_json in
// cleartext (encryption named private_key_pem literally) and toView
// handed the whole blob back to any site member.

const dockyardAPIKey = "dy_live_SUPER_SECRET_DOCKYARD_KEY"

const deployTestPEM = "-----BEGIN OPENSSH PRIVATE KEY-----\nfakekeycontents\n-----END OPENSSH PRIVATE KEY-----\n"

func dockyardConfigBody() map[string]any {
	return map[string]any{
		"dockyard_url":    "https://dockyard.example.com",
		"api_key":         dockyardAPIKey,
		"server_id":       "6f1b2c34-5d6e-4f80-9a1b-2c3d4e5f6071",
		"domain":          "site.example.com",
		"host":            "deploy.example.com",
		"user":            "deployer",
		"port":            22,
		"host_path":       "/srv/atomicsite/site1",
		"private_key_pem": deployTestPEM,
	}
}

// storedConfig reads config_json straight off the row, bypassing every
// handler, so the assertion is about what actually sits in SQLite (and
// therefore in every Litestream replica and restic backup).
func storedConfig(t *testing.T, q *store.Queries, targetID string) (string, map[string]any) {
	t.Helper()
	row, err := q.GetDeployTarget(context.Background(), targetID)
	if err != nil {
		t.Fatalf("get deploy target: %v", err)
	}
	cfg := map[string]any{}
	if err := json.Unmarshal([]byte(row.ConfigJson), &cfg); err != nil {
		t.Fatalf("unmarshal stored config: %v", err)
	}
	return row.ConfigJson, cfg
}

func TestDeploy_DockyardAPIKeyEncryptedAtRestAndRedactedOnRead(t *testing.T) {
	sqlDB, q := setupDeployTestDB(t)
	seedSite(t, q, "site1")
	cfg := &config.Config{ShieldKey: testShieldKey}
	h := NewDeployHandler(cfg, q, sqlDB)
	r := deployRouter(h)

	code, resp := doJSON(t, r, http.MethodPost, "/api/sites/site1/deploy-targets", map[string]any{
		"name":   "dockyard",
		"kind":   "dockyard",
		"config": dockyardConfigBody(),
	})
	if code != http.StatusCreated {
		t.Fatalf("create dockyard target: status=%d resp=%v", code, resp)
	}
	targetID, _ := resp["id"].(string)
	if targetID == "" {
		t.Fatalf("no target id in create response: %v", resp)
	}

	// 1. At rest: neither secret may appear in cleartext, and both must
	// carry the atrest prefix.
	rawJSON, stored := storedConfig(t, q, targetID)
	if strings.Contains(rawJSON, dockyardAPIKey) {
		t.Fatalf("api_key stored in cleartext: %s", rawJSON)
	}
	if strings.Contains(rawJSON, "fakekeycontents") {
		t.Fatalf("private_key_pem stored in cleartext: %s", rawJSON)
	}
	for _, key := range deploySecretConfigKeys {
		v, _ := stored[key].(string)
		if !atrest.IsEncrypted(v) {
			t.Fatalf("stored %s is not atrest ciphertext: %q", key, v)
		}
	}

	// 2. Read surfaces: create, get and list must all redact, never echo
	// the secret or its ciphertext.
	assertRedacted := func(label string, view map[string]any) {
		t.Helper()
		c, ok := view["config"].(map[string]any)
		if !ok {
			t.Fatalf("%s: no config in view: %v", label, view)
		}
		for _, key := range deploySecretConfigKeys {
			got, _ := c[key].(string)
			if got != deploySecretMask {
				t.Fatalf("%s: config[%s]=%q; want the mask %q", label, key, got, deploySecretMask)
			}
		}
		blob, _ := json.Marshal(view)
		if strings.Contains(string(blob), dockyardAPIKey) {
			t.Fatalf("%s: api_key leaked to the API: %s", label, blob)
		}
		if strings.Contains(string(blob), "enc:v1:") {
			t.Fatalf("%s: ciphertext leaked to the API: %s", label, blob)
		}
		// Non-secret keys must still round-trip, or the form breaks.
		if c["server_id"] != "6f1b2c34-5d6e-4f80-9a1b-2c3d4e5f6071" {
			t.Fatalf("%s: non-secret key was redacted: %v", label, c)
		}
	}
	assertRedacted("create", resp)

	code, getResp := doJSON(t, r, http.MethodGet, "/api/sites/site1/deploy-targets/"+targetID, nil)
	if code != http.StatusOK {
		t.Fatalf("get target: status=%d", code)
	}
	assertRedacted("get", getResp)

	code, listResp := doJSON(t, r, http.MethodGet, "/api/sites/site1/deploy-targets", nil)
	if code != http.StatusOK {
		t.Fatalf("list targets: status=%d", code)
	}
	listBlob, _ := json.Marshal(listResp)
	if strings.Contains(string(listBlob), dockyardAPIKey) {
		t.Fatalf("api_key leaked from the list endpoint: %s", listBlob)
	}

	// 3. The deploy path still gets the real credential back.
	_, stored = storedConfig(t, q, targetID)
	decryptDeployConfigSecrets(deployConfigCipher(cfg), stored)
	if stored["api_key"] != dockyardAPIKey {
		t.Fatalf("api_key did not round-trip for deploy: %v", stored["api_key"])
	}
	if stored["private_key_pem"] != deployTestPEM {
		t.Fatalf("private_key_pem did not round-trip for deploy: %v", stored["private_key_pem"])
	}
}

// TestDeploy_MaskedSaveKeepsStoredSecret covers the migration hazard the
// redaction creates: the SPA renders the target from toView, so an
// unedited save echoes the mask back. That must keep the stored secret,
// not overwrite the credential with the literal mask.
func TestDeploy_MaskedSaveKeepsStoredSecret(t *testing.T) {
	sqlDB, q := setupDeployTestDB(t)
	seedSite(t, q, "site1")
	cfg := &config.Config{ShieldKey: testShieldKey}
	h := NewDeployHandler(cfg, q, sqlDB)
	r := deployRouter(h)

	code, resp := doJSON(t, r, http.MethodPost, "/api/sites/site1/deploy-targets", map[string]any{
		"name":   "dockyard",
		"kind":   "dockyard",
		"config": dockyardConfigBody(),
	})
	if code != http.StatusCreated {
		t.Fatalf("create: status=%d resp=%v", code, resp)
	}
	targetID := resp["id"].(string)

	// Echo the redacted view back verbatim, exactly as the form does.
	echoed := resp["config"].(map[string]any)
	echoed["domain"] = "renamed.example.com"
	code, patched := doJSON(t, r, http.MethodPatch, "/api/sites/site1/deploy-targets/"+targetID, map[string]any{
		"config": echoed,
	})
	if code != http.StatusOK {
		t.Fatalf("patch with masked secrets: status=%d resp=%v", code, patched)
	}

	_, stored := storedConfig(t, q, targetID)
	if got, _ := stored["api_key"].(string); got == deploySecretMask {
		t.Fatalf("the mask was stored as the api_key, wiping the credential")
	}
	decryptDeployConfigSecrets(deployConfigCipher(cfg), stored)
	if stored["api_key"] != dockyardAPIKey {
		t.Fatalf("api_key lost across a masked save: %v", stored["api_key"])
	}
	if stored["private_key_pem"] != deployTestPEM {
		t.Fatalf("private_key_pem lost across a masked save: %v", stored["private_key_pem"])
	}
	if stored["domain"] != "renamed.example.com" {
		t.Fatalf("non-secret edit did not apply: %v", stored["domain"])
	}

	// A genuinely new secret still replaces the old one.
	echoed["api_key"] = "dy_live_ROTATED"
	code, _ = doJSON(t, r, http.MethodPatch, "/api/sites/site1/deploy-targets/"+targetID, map[string]any{
		"config": echoed,
	})
	if code != http.StatusOK {
		t.Fatalf("patch with a new api_key: status=%d", code)
	}
	_, stored = storedConfig(t, q, targetID)
	decryptDeployConfigSecrets(deployConfigCipher(cfg), stored)
	if stored["api_key"] != "dy_live_ROTATED" {
		t.Fatalf("rotation did not take: %v", stored["api_key"])
	}
}

// TestDeploy_LegacyPlaintextRowsMigrate covers rows written before the
// fix: their api_key has no enc:v1: prefix. They must stay readable by
// the deploy path, must not leak through the API, and must upgrade to
// ciphertext on the next save.
func TestDeploy_LegacyPlaintextRowsMigrate(t *testing.T) {
	sqlDB, q := setupDeployTestDB(t)
	seedSite(t, q, "site1")
	cfg := &config.Config{ShieldKey: testShieldKey}
	h := NewDeployHandler(cfg, q, sqlDB)
	r := deployRouter(h)

	legacy := dockyardConfigBody()
	legacyJSON, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy config: %v", err)
	}
	targetID := "tgt-legacy-0001"
	if err := q.CreateDeployTarget(context.Background(), store.CreateDeployTargetParams{
		ID: targetID, SiteID: "site1", Name: "legacy", Kind: "dockyard",
		ConfigJson: string(legacyJSON), IsDefault: 1,
	}); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	// Readable: plaintext passes through the decrypt mirror verbatim.
	_, stored := storedConfig(t, q, targetID)
	decryptDeployConfigSecrets(deployConfigCipher(cfg), stored)
	if stored["api_key"] != dockyardAPIKey {
		t.Fatalf("legacy plaintext api_key not readable: %v", stored["api_key"])
	}

	// Not leaked: redaction is on the value, not on the enc:v1: prefix.
	code, got := doJSON(t, r, http.MethodGet, "/api/sites/site1/deploy-targets/"+targetID, nil)
	if code != http.StatusOK {
		t.Fatalf("get legacy target: status=%d", code)
	}
	blob, _ := json.Marshal(got)
	if strings.Contains(string(blob), dockyardAPIKey) {
		t.Fatalf("legacy plaintext api_key leaked to the API: %s", blob)
	}

	// Upgrades: an unedited save re-stores it as ciphertext.
	echoed := got["config"].(map[string]any)
	code, _ = doJSON(t, r, http.MethodPatch, "/api/sites/site1/deploy-targets/"+targetID, map[string]any{
		"config": echoed,
	})
	if code != http.StatusOK {
		t.Fatalf("patch legacy target: status=%d", code)
	}
	rawJSON, stored := storedConfig(t, q, targetID)
	if strings.Contains(rawJSON, dockyardAPIKey) {
		t.Fatalf("legacy api_key still cleartext after a save: %s", rawJSON)
	}
	decryptDeployConfigSecrets(deployConfigCipher(cfg), stored)
	if stored["api_key"] != dockyardAPIKey {
		t.Fatalf("legacy api_key lost during the upgrade save: %v", stored["api_key"])
	}
}
