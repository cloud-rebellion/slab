package middleware

import "testing"

func TestIsSensitiveLogKey(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		// Sensitive: value must be redacted before shipping to Flare.
		{"password", true},
		{"user.password", true},
		{"agent_token", true},
		{"authorization", true},
		{"Cookie", true},
		{"db_credential", true},
		{"api_key", true},
		{"apiKey", true},
		{"access_key", true},
		{"private_key", true},
		{"vault_key", true},
		{"new_value", true},
		{"jwt", true},
		{"session_id", true},
		{"flare_dsn", true},
		{"bearer", true},
		// Not sensitive: legit keys must pass through untouched.
		{"error", false},
		{"server_id", false},
		{"command_id", false},
		{"count", false},
		{"keyboard", false},
		{"monkey", false},
		{"duration_ms", false},
		{"trace_id", false},
	}
	for _, tt := range tests {
		if got := isSensitiveLogKey(tt.key); got != tt.want {
			t.Errorf("isSensitiveLogKey(%q) = %v, want %v", tt.key, got, tt.want)
		}
	}
}
