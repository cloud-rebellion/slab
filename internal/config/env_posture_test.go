package config

import (
	"strings"
	"testing"
)

// The environment file a healthy production deploy supplies. Each case
// below removes exactly one line from it, which is the real-world failure
// this whole guard exists for: env_file cannot refuse an absence, so a
// regenerated, restored or hand-edited file that loses one line must be
// caught by the process refusing to start.
func prodEnvFile() map[string]string {
	return map[string]string{
		"ATOMICSITE_ENV":            EnvProduction,
		"BASE_URL":                  "https://app.example.com",
		"JWT_SECRET":                "this-is-a-strong-32-byte-secret-value-here",
		"ANALYTICS_SALT":            "this-is-a-random-32-byte-fingerprint-salt-value",
		"ATOMICSITE_SHIELD_KEY":     "0123456789abcdef0123456789abcdef",
		"ATOMICSITE_PRIMARY_DOMAIN": "example.com",
		"ADMIN_PASSWORD":            "a-long-enough-admin-passphrase",
		// The docker bridge gateway the host reverse proxy connects
		// from. A production deploy has to state its trust boundary;
		// see TestTrustedProxies_ProductionMustDeclareTheBoundary.
		"ATOMICSITE_TRUSTED_PROXIES": "172.18.0.1/32",
	}
}

// applyEnv installs the map, blanking anything the case dropped. Blank is
// how a missing line looks to envOr, which treats "" as absent.
func applyEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, k := range []string{
		"ATOMICSITE_ENV", "BASE_URL", "JWT_SECRET", "ANALYTICS_SALT",
		"ATOMICSITE_SHIELD_KEY", "ATOMICSITE_PRIMARY_DOMAIN", "BUILT_SITE_SUFFIX",
		"ADMIN_PASSWORD", "ATOMICSITE_DEPLOYMENT_MODE", "ATOMICSITE_REQUIRE_MFA",
		"ATOMICSITE_SHIELD_REDIS_URL", "ATOMICSITE_TRUSTED_PROXIES",
	} {
		t.Setenv(k, env[k])
	}
}

// TestEnvPosture_LoadAndValidate walks the config permutations that matter
// for deployment posture, through the REAL Load() so the env-var wiring is
// covered and not just the struct fields.
//
// The headline row is "BASE_URL line missing from a production env file".
// Before the ATOMICSITE_ENV switch that row passed validation: BaseURL fell
// back to http://localhost:8080, IsLocalDev() saw the localhost prefix and
// returned nil above every guard, so the server booted signing sessions
// with the documented default JWT secret and running at-rest crypto as a
// passthrough. It must now refuse.
func TestEnvPosture_LoadAndValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(map[string]string)
		wantErr string // empty means Validate must succeed
	}{
		{
			name:   "complete production env validates",
			mutate: func(env map[string]string) {},
		},
		{
			name:    "BASE_URL line missing refuses",
			mutate:  func(env map[string]string) { delete(env, "BASE_URL") },
			wantErr: "BASE_URL",
		},
		{
			name: "BASE_URL and ATOMICSITE_ENV both missing still refuses",
			mutate: func(env map[string]string) {
				delete(env, "BASE_URL")
				delete(env, "ATOMICSITE_ENV")
			},
			wantErr: "BASE_URL",
		},
		{
			name:    "ATOMICSITE_ENV missing keeps every production guard",
			mutate:  func(env map[string]string) { delete(env, "ATOMICSITE_ENV") },
			wantErr: "",
		},
		{
			name: "ATOMICSITE_ENV missing plus a default secret refuses",
			mutate: func(env map[string]string) {
				delete(env, "ATOMICSITE_ENV")
				env["JWT_SECRET"] = DefaultJWTSecret
			},
			wantErr: "JWT_SECRET",
		},
		{
			name: "shield key line missing refuses",
			mutate: func(env map[string]string) {
				delete(env, "ATOMICSITE_SHIELD_KEY")
			},
			wantErr: "ATOMICSITE_SHIELD_KEY",
		},
		{
			name: "analytics salt line missing refuses",
			mutate: func(env map[string]string) {
				delete(env, "ANALYTICS_SALT")
			},
			wantErr: "ANALYTICS_SALT",
		},
		{
			name: "trusted proxies line missing refuses",
			mutate: func(env map[string]string) {
				delete(env, "ATOMICSITE_TRUSTED_PROXIES")
			},
			wantErr: "ATOMICSITE_TRUSTED_PROXIES",
		},
		{
			name: "trusted proxies set to none is an accepted opt-out",
			mutate: func(env map[string]string) {
				env["ATOMICSITE_TRUSTED_PROXIES"] = TrustedProxiesNone
			},
			wantErr: "",
		},
		{
			name: "domain line missing refuses",
			mutate: func(env map[string]string) {
				delete(env, "ATOMICSITE_PRIMARY_DOMAIN")
			},
			wantErr: "ATOMICSITE_PRIMARY_DOMAIN",
		},
		{
			name: "explicit development with nothing set is permissive",
			mutate: func(env map[string]string) {
				for k := range env {
					delete(env, k)
				}
				env["ATOMICSITE_ENV"] = EnvDevelopment
			},
			wantErr: "",
		},
		{
			name: "development is case and space insensitive",
			mutate: func(env map[string]string) {
				for k := range env {
					delete(env, k)
				}
				env["ATOMICSITE_ENV"] = "  Development  "
			},
			wantErr: "",
		},
		{
			name: "a typo in ATOMICSITE_ENV refuses rather than picking a branch",
			mutate: func(env map[string]string) {
				env["ATOMICSITE_ENV"] = "developement"
			},
			wantErr: "ATOMICSITE_ENV",
		},
		{
			name: "prod-looking words are not development",
			mutate: func(env map[string]string) {
				for k := range env {
					delete(env, k)
				}
				env["ATOMICSITE_ENV"] = "dev"
			},
			wantErr: "ATOMICSITE_ENV",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := prodEnvFile()
			tc.mutate(env)
			applyEnv(t, env)

			cfg := Load()
			err := cfg.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("expected Validate to succeed, got %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected Validate to refuse with %q, got nil (the server would have booted)", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestEnvPosture_AbsentEnvIsProduction is the single most important
// property and is asserted on its own so a future edit cannot weaken it
// inside a table: an absent ATOMICSITE_ENV is production, and a Config
// that nobody configured at all is production too.
func TestEnvPosture_AbsentEnvIsProduction(t *testing.T) {
	t.Setenv("ATOMICSITE_ENV", "")
	if got := Load().IsProduction(); !got {
		t.Error("absent ATOMICSITE_ENV must mean production; a missing line has to fail closed")
	}
	if got := (&Config{}).IsProduction(); !got {
		t.Error("the zero-value Config must be production; the struct default has to fail closed too")
	}
	if got := (&Config{Env: EnvDevelopment}).IsProduction(); got {
		t.Error("an explicit development declaration must opt out of the production guards")
	}
}

// TestEnvPosture_LocalhostBaseURLNoLongerRelaxesGuards is the regression
// guard for the exact defect. A localhost BaseURL is not permission to
// skip anything; only ATOMICSITE_ENV=development is.
func TestEnvPosture_LocalhostBaseURLNoLongerRelaxesGuards(t *testing.T) {
	for _, baseURL := range []string{
		DefaultBaseURL,
		"http://localhost:8080",
		"http://127.0.0.1:8080",
		"http://0.0.0.0:8080",
	} {
		cfg := &Config{
			BaseURL:       baseURL,
			JWTSecret:     DefaultJWTSecret,
			AnalyticsSalt: DefaultAnalyticsSalt,
		}
		if err := cfg.Validate(); err == nil {
			t.Errorf("BaseURL=%q with default secrets and no ATOMICSITE_ENV must refuse; got nil", baseURL)
		}
		if err := cfg.ValidateAdminPassword(DefaultAdminPassword); err == nil {
			t.Errorf("BaseURL=%q with the default admin password and no ATOMICSITE_ENV must refuse; got nil", baseURL)
		}
	}
}
