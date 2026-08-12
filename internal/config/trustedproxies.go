// Package config - trustedproxies.go.
//
// One parser for ATOMICSITE_TRUSTED_PROXIES, shared by the boot-time
// guard in Validate and by middleware.TrustedProxyRealIP, which is the
// only hop allowed to rewrite r.RemoteAddr from a forwarding header.
//
// It lives in config rather than in middleware because middleware
// already imports config; the reverse would be an import cycle. Having
// one parser is the point: a second copy is how a value that the guard
// accepts and the middleware silently discards (or the reverse) gets
// shipped.
package config

import (
	"net/netip"
	"strings"
)

// TrustedProxiesNone is the explicit "there is no reverse proxy in
// front of me, r.RemoteAddr is already the client" value. It is not the
// same as an empty value: empty means the operator never made the
// decision, which in production is refused by Validate. Opting out has
// to be a positive act, the same rule ATOMICSITE_ENV already follows.
const TrustedProxiesNone = "none"

// ParseTrustedProxies turns the raw comma-separated env value into the
// prefix list the middleware matches the TCP peer against.
//
// Returns the parsed prefixes and the entries that could not be parsed.
// Runtime callers ignore the second return and run with whatever parsed
// (an unparseable entry has never trusted anything, so dropping it is
// the safe direction); Validate uses it to refuse a typo at boot rather
// than let it silently collapse the trust list to nothing.
//
// Bare IPs auto-widen to /32 (v4) or /128 (v6). "none" alone yields no
// prefixes and no bad entries: an explicit, valid, trust-nobody list.
func ParseTrustedProxies(raw string) (prefixes []netip.Prefix, bad []string) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, TrustedProxiesNone) {
		return nil, nil
	}
	prefixes = make([]netip.Prefix, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			prefixes = append(prefixes, p)
			continue
		}
		if addr, err := netip.ParseAddr(part); err == nil {
			bits := 32
			if addr.Is6() {
				bits = 128
			}
			prefixes = append(prefixes, netip.PrefixFrom(addr, bits))
			continue
		}
		bad = append(bad, part)
	}
	return prefixes, bad
}
