package settingspolicy

import "strings"

// This file is the single authority for settings-value SECRECY, the read-side
// counterpart to the write-side policy in settingspolicy.go.
//
// It exists for the same reason that file does: the policy was duplicated and
// the copies drifted. The 2026-08-11 audit found every tenant's live Mollie
// API key on five LLM-facing surfaces because the redactor was wired to
// list_settings alone. The string literal was one problem; the real problem was
// that each surface had to remember to call a masker, and five of them did not.
//
// So the rule is: NOTHING hand-declares that a value is secret, and nothing
// writes its own mask literal. Every surface asks IsSecretValue and substitutes
// RedactedValue, via MaskValue or the row-shaped agent.MaskSettingRows.

// RedactedValue is the single literal substituted for any secret setting value
// on every LLM-facing surface. Shared so no two maskers can drift to different
// strings: an agent that learns to recognise one mask must recognise all.
const RedactedValue = "***redacted***"

// secretValueKeys are the "<category>.<key>" settings whose VALUE is
// bearer-equivalent and must never be returned to an LLM/agent. The agent may
// always read the DESCRIPTOR (label, type, whether it is configured); it may
// never read the value.
var secretValueKeys = map[string]bool{
	"payments.mollie_api_key":      true,
	"analytics.crm_webhook_secret": true,
}

// secretKeySuffixes is the defensive half of the policy. A setting whose key
// ends in one of these is treated as secret even if nobody remembered to add
// it to secretValueKeys, so a newly introduced provider key is masked BY
// DEFAULT and a mistake costs a redundant mask rather than a leaked credential.
var secretKeySuffixes = []string{
	"_secret",
	"_api_key",
	"_apikey",
	"_token",
	"_password",
	"_private_key",
}

// IsSecretValue reports whether the value for category.key is a secret that
// must be masked before it crosses the agent/LLM boundary. Explicit allowlist
// first, then the suffix heuristic.
func IsSecretValue(category, key string) bool {
	if secretValueKeys[category+"."+key] {
		return true
	}
	k := strings.ToLower(key)
	for _, suffix := range secretKeySuffixes {
		if strings.HasSuffix(k, suffix) {
			return true
		}
	}
	return false
}

// MaskValue returns RedactedValue when category.key is secret and value is
// non-empty; otherwise it returns value unchanged.
//
// The empty case is deliberate and load-bearing: an UNSET secret must keep
// reading as empty rather than as redacted, or the agent cannot tell "the
// tenant configured a key I am not allowed to see" from "the tenant never
// configured one" and will nag the human to set a key that already exists.
func MaskValue(category, key, value string) string {
	if value == "" {
		return value
	}
	if IsSecretValue(category, key) {
		return RedactedValue
	}
	return value
}
