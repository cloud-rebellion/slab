package builder

import (
	"os"
	"strings"
	"testing"
)

// The build subprocess runs tenant-influenced JavaScript (component
// frontmatter, dependency install scripts), so anything reachable in its
// environment is readable by a tenant. This asserts the allowlist actually
// holds, using the real names that leaked under the old denylist.
func TestBuildEnvExcludesSecrets(t *testing.T) {
	leaked := map[string]string{
		// These two are the reason the denylist was replaced. _ACCESS_KEY is
		// not one of the checked suffixes and _ID is not either, so both sailed
		// through while JWT_SECRET was correctly stripped. They authorise the
		// object-storage replica of the whole multi-tenant database.
		"LITESTREAM_ACCESS_KEY_ID":          "AKIAEXAMPLE",
		"LITESTREAM_SECRET_ACCESS_KEY":      "s3cr3t",
		"BRIGHTCRM_WEBHOOK_SECRET_PREVIOUS": "prev",
		"FLARE_DSN":                         "https://key@flare.example/1",
		"ATOMICSITE_SHIELD_REDIS_URL":       "redis://user:pw@host:6379",
		// Ones the old denylist did catch, which must stay caught.
		"JWT_SECRET":            "SENTINEL-jwt-secret-value",
		"OIDC_CLIENT_SECRET":    "SENTINEL-oidc-client-secret",
		"ADMIN_PASSWORD":        "SENTINEL-admin-password",
		"ATOMICSITE_SHIELD_KEY": "SENTINEL-shield-key",
		"DATABASE_URL":          "SENTINEL-database-url",
		// A secret-shaped variable inside an allowed prefix family.
		"NODE_AUTH_TOKEN": "npm-token",
		// Something simply unknown: an allowlist must drop it by default.
		"SOME_FUTURE_CREDENTIAL_THING": "SENTINEL-future-credential",
	}
	for k, v := range leaked {
		t.Setenv(k, v)
	}
	// Toolchain variables the build legitimately needs.
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("HOME", "/home/build")
	t.Setenv("NODE_OPTIONS", "--max-old-space-size=2048")

	env := buildEnv()
	joined := strings.Join(env, "\n")

	for k, v := range leaked {
		for _, kv := range env {
			if strings.HasPrefix(kv, k+"=") {
				t.Errorf("%s reached the build subprocess; a tenant's astro build can read it", k)
			}
		}
		// Also catch the VALUE appearing under some other name. Values are
		// distinctive sentinels so this cannot false-positive on a substring
		// of an unrelated variable (an earlier version used "a" and "d", which
		// matched inside HOME=/home/build).
		if v != "" && strings.Contains(joined, v) {
			t.Errorf("the value of %s appears in the build environment", k)
		}
	}

	want := map[string]bool{"PATH": false, "HOME": false, "NODE_OPTIONS": false}
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for name, present := range want {
		if !present {
			t.Errorf("%s was stripped but the build needs it", name)
		}
	}
}

// An allowlist is only safe if the default is deny. This asserts the shape
// directly rather than trusting the table.
func TestBuildEnvDefaultsToDeny(t *testing.T) {
	for _, name := range []string{
		"AWS_SECRET_ACCESS_KEY", "GITHUB_TOKEN", "STRIPE_KEY", "MYSTERY_VAR",
		"LITESTREAM_REPLICA_URL", "OPENAI_API_KEY",
	} {
		if buildEnvAllowed(name) {
			t.Errorf("buildEnvAllowed(%q) = true; unknown variables must be denied by default", name)
		}
	}
	for _, name := range []string{"PATH", "HOME", "NODE_OPTIONS", "BUN_INSTALL"} {
		if !buildEnvAllowed(name) {
			t.Errorf("buildEnvAllowed(%q) = false; the build needs it", name)
		}
	}
}

// Guards against a future edit reintroducing os.Environ() passthrough.
func TestBuildEnvIsNotTheWholeEnvironment(t *testing.T) {
	t.Setenv("A_DEFINITELY_UNRELATED_VARIABLE", "1")
	if len(buildEnv()) >= len(os.Environ()) {
		t.Error("buildEnv returned as much as os.Environ(); the filter is not applied")
	}
}
