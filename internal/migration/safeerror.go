package migration

// Tenant-safe text for a crawl/import failure.
//
// The MCP error-scrubbing pass (scrubError + isInternalMCPError) only runs on a
// handler's error RETURN path. A failure persisted into migrations.error and
// later handed back as a DATA field never touches it, so the raw text reached
// the model and the tenant untouched.
//
// That text is specific enough to matter. The SSRF guard returns "refusing to
// connect to private address 10.x.y.z" with the RESOLVED address, and a plain
// dial failure arrives as "dial tcp <ip>:<port>: connect: connection refused",
// a substring isInternalMCPError already lists as unfit for the model. So a
// tenant could point a hostname they control at an internal address, let the
// guard correctly refuse, then read back exactly what it resolved to: the LLM
// channel as an internal-network mapper, one hostname at a time.
//
// Genericising at the PERSIST site rather than scrubbing at each read is
// deliberate. The same column is read by the MCP tools, the REST admin API and
// the admin UI, and in a multi-tenant deployment a site "admin" is a TENANT,
// not the operator, so every one of those is a tenant-facing surface. The
// operator still gets the full error: every call site logs it via slog before
// calling this.

import (
	"errors"
	"regexp"
	"strings"
)

// ipLikeRE matches IPv4 literals with optional port, and bracketed or bare IPv6
// runs. Deliberately greedy about what counts as an address: a false positive
// costs a slightly vaguer message, a false negative discloses infrastructure.
var ipLikeRE = regexp.MustCompile(
	`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}(?::\d{1,5})?\b` +
		`|\[[0-9a-fA-F:]+\](?::\d{1,5})?` +
		`|\b(?:[0-9a-fA-F]{1,4}:){2,}[0-9a-fA-F]{1,4}\b`)

// safeErrorClasses maps a recognised failure to a message that tells the tenant
// what to do without describing our network. Order matters: first match wins.
var safeErrorClasses = []struct {
	match []string
	text  string
}{
	{
		[]string{"private address", "refusing to connect", "not a public address", "non-public address"},
		"the source URL resolves to an address this server will not fetch; use a publicly reachable hostname",
	},
	{
		[]string{"connection refused", "no such host", "dial tcp", "i/o timeout", "context deadline exceeded", "timeout"},
		"could not reach the source URL; check the hostname is public and responding",
	},
	{
		[]string{"certificate", "tls", "x509"},
		"the source URL's TLS certificate could not be verified",
	},
	{
		[]string{"too many redirects", "redirect"},
		"the source URL redirected too many times",
	},
	{
		[]string{"unsupported scheme", "invalid url", "parse"},
		"the source URL could not be parsed; it must be an absolute http or https URL",
	},
	{
		[]string{"status 4", "status 5", "unexpected status"},
		"the source URL returned an error status",
	},
	{
		[]string{"too large", "exceeds", "limit"},
		"the source response was larger than the import limit",
	},
}

// SafeErrorText returns text safe to persist in a tenant-readable column.
// It fails closed: anything it does not recognise becomes a generic message
// rather than being passed through, because an unrecognised error is exactly
// the one whose contents nobody has audited.
func SafeErrorText(err error) string {
	if err == nil {
		return ""
	}
	low := strings.ToLower(err.Error())
	for _, c := range safeErrorClasses {
		for _, m := range c.match {
			if strings.Contains(low, m) {
				return c.text
			}
		}
	}
	return "the import failed; see the server logs for details"
}

// SafeWarnings genericises crawl warnings, which travel back to the agent in
// the manifest and carry the same disclosure risk as the error field. An IP
// found in an otherwise-harmless warning is redacted in place rather than
// dropping the warning, so the tenant still learns which page had a problem.
func SafeWarnings(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := make([]string, 0, len(in))
	for _, w := range in {
		out = append(out, ipLikeRE.ReplaceAllString(w, "[redacted-address]"))
	}
	return out
}

// SafeErrorTextString is the string form, for the call sites that already hold
// a message rather than an error.
func SafeErrorTextString(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return SafeErrorText(errors.New(s))
}
