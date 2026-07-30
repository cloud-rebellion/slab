package domains

import (
	"context"
	"fmt"
	"testing"
)

func withTXT(t *testing.T, fn func(ctx context.Context, name string) ([]string, error)) {
	t.Helper()
	orig := txtResolver
	txtResolver = fn
	t.Cleanup(func() { txtResolver = orig })
}

// The proof must fail closed on every branch. Failing open here is the entire
// hostname-takeover vulnerability: the reconciler writes DNS for the name right
// after this returns nil.
func TestVerifyDNSTXTOwnershipFailsClosed(t *testing.T) {
	ctx := context.Background()
	const token = "tok-abc123"

	t.Run("matching record passes", func(t *testing.T) {
		withTXT(t, func(context.Context, string) ([]string, error) {
			return []string{"unrelated", token}, nil
		})
		if err := VerifyDNSTXTOwnership(ctx, "app.example.com", token); err != nil {
			t.Errorf("a correct TXT proof was refused: %v", err)
		}
	})

	t.Run("record with surrounding whitespace passes", func(t *testing.T) {
		withTXT(t, func(context.Context, string) ([]string, error) {
			return []string{"  " + token + "  "}, nil
		})
		if err := VerifyDNSTXTOwnership(ctx, "app.example.com", token); err != nil {
			t.Errorf("whitespace around the token should be tolerated: %v", err)
		}
	})

	t.Run("no record is refused", func(t *testing.T) {
		withTXT(t, func(context.Context, string) ([]string, error) {
			return nil, nil
		})
		if err := VerifyDNSTXTOwnership(ctx, "auth.example.com", token); err == nil {
			t.Error("a hostname with no TXT proof was accepted")
		}
	})

	t.Run("lookup error is refused", func(t *testing.T) {
		withTXT(t, func(context.Context, string) ([]string, error) {
			return nil, fmt.Errorf("NXDOMAIN")
		})
		if err := VerifyDNSTXTOwnership(ctx, "auth.example.com", token); err == nil {
			t.Error("a DNS lookup failure was treated as proof")
		}
	})

	t.Run("wrong token is refused", func(t *testing.T) {
		withTXT(t, func(context.Context, string) ([]string, error) {
			return []string{"some-other-token"}, nil
		})
		if err := VerifyDNSTXTOwnership(ctx, "auth.example.com", token); err == nil {
			t.Error("a non-matching TXT record was accepted")
		}
	})

	// An empty expected token would otherwise be satisfied by an empty TXT
	// record, which anyone can create.
	t.Run("empty expected token is refused even against an empty record", func(t *testing.T) {
		withTXT(t, func(context.Context, string) ([]string, error) {
			return []string{""}, nil
		})
		if err := VerifyDNSTXTOwnership(ctx, "auth.example.com", ""); err == nil {
			t.Error("an empty token matched an empty TXT record")
		}
	})

	t.Run("empty hostname is refused", func(t *testing.T) {
		withTXT(t, func(context.Context, string) ([]string, error) {
			return []string{token}, nil
		})
		if err := VerifyDNSTXTOwnership(ctx, "   ", token); err == nil {
			t.Error("an empty hostname was accepted")
		}
	})
}

// The challenge name must sit under an underscore label so it cannot collide
// with a real host, and must be built from the hostname being claimed.
func TestTXTChallengeName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"app.example.com", "_atomic-verify.app.example.com"},
		{"APP.EXAMPLE.COM", "_atomic-verify.app.example.com"},
		{"app.example.com.", "_atomic-verify.app.example.com"},
		{"  app.example.com  ", "_atomic-verify.app.example.com"},
	}
	for _, tt := range tests {
		if got := TXTChallengeName(tt.in); got != tt.want {
			t.Errorf("TXTChallengeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
