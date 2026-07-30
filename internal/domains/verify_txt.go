package domains

// Out-of-band ownership proof for hostnames inside a Cloudflare zone WE control.
//
// The HTTP-01-style check in verify.go proves control of a hostname by fetching
// a token over it. That is sound only when the tenant pointed DNS at us
// themselves, which is the case for their own domain: they had to hold the zone
// to do it.
//
// It is circular when the hostname sits in an operator-managed zone, because
// there the reconciler creates the A record ITSELF before verifying
// (reconciler.go: EnsureA then VerifyHostnameOwnership), and the verify handler
// resolves the token without binding it to a hostname. So Slab pointed the name
// at its own edge and then successfully proved to itself that it could serve a
// token on it. A tenant could claim any name in a managed zone that had no
// existing A record (a not-yet-existing name, an AAAA-only host, or one covered
// by a wildcard), get a real Let's Encrypt certificate for it, and serve their
// own content there. Adding a Host check to the token handler does not break
// the loop: the probe legitimately carries Host: <target>, so it would pass.
//
// The break is to require proof the tenant controls the NAME before writing any
// DNS for it. A DNS TXT record can only be created by whoever already holds the
// zone or that label, which is exactly the thing being claimed. An attacker
// naming auth.example.com cannot create it; the operator can.
//
// Scoped deliberately to managed zones. For a tenant's own domain MatchZone
// returns nothing, EnsureA never fires, and this requirement does not apply, so
// the ordinary custom-domain flow is unchanged.

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// txtChallengePrefix is the label the proof record lives under, following the
// underscore-prefixed convention (_acme-challenge, _dmarc) so it cannot collide
// with a real host.
const txtChallengePrefix = "_atomic-verify"

// TXTChallengeName returns the record name the tenant must create.
func TXTChallengeName(hostname string) string {
	return txtChallengePrefix + "." + strings.ToLower(strings.TrimSuffix(strings.TrimSpace(hostname), "."))
}

// txtResolver is swappable so tests do not need real DNS.
var txtResolver = func(ctx context.Context, name string) ([]string, error) {
	r := &net.Resolver{}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return r.LookupTXT(ctx, name)
}

// VerifyDNSTXTOwnership reports whether a TXT record at
// _atomic-verify.<hostname> carries expectedToken. It fails closed: a lookup
// error, an empty token, or no matching record are all refusals.
func VerifyDNSTXTOwnership(ctx context.Context, hostname, expectedToken string) error {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return fmt.Errorf("hostname is empty")
	}
	// An empty expected token would otherwise match a TXT record that is
	// itself empty, which is a trivially forgeable proof.
	if strings.TrimSpace(expectedToken) == "" {
		return fmt.Errorf("verify token is empty")
	}

	name := TXTChallengeName(hostname)
	values, err := txtResolver(ctx, name)
	if err != nil {
		return fmt.Errorf("no TXT proof at %s (%w); create a TXT record there containing the verify token", name, err)
	}
	for _, v := range values {
		if strings.TrimSpace(v) == strings.TrimSpace(expectedToken) {
			return nil
		}
	}
	return fmt.Errorf("TXT record at %s does not contain the verify token", name)
}
