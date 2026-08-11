package shield

import "strings"

// encodedBlobMinLen is the length at which a single unbroken alphanumeric
// run stops being plausible human text. Every PII shape the regex bank
// hunts is orders of magnitude shorter: the longest is an IBAN at 34
// characters. 512 leaves a very wide margin, so this can never swallow a
// real email, phone number, personnummer, IBAN or org number.
const encodedBlobMinLen = 512

// looksLikeEncodedBlob reports whether a JSON string leaf is a single
// encoded payload (base64, base64url, hex, or a data: URI) rather than
// text a human wrote.
//
// Why this exists: the regex bank's phone pattern matches `+` followed by
// digits, and base64 output contains `+` followed by digits constantly.
// Running the redact pass over a base64 PNG therefore replaces random
// slices of the image with [shield:phone:...] markers and the agent
// receives an undecodable blob. Verified on a 200 KB PNG: 266668 bytes in,
// 266743 out, payload corrupted.
//
// That is not hypothetical and it is not new to the REST surface. The MCP
// `screenshot` tool already routes handlers.ScreenshotResult through
// Session.ShieldJSON, so it has been corrupting images for as long as
// Shield has been enabled. This guard sits in the shared JSON walker so
// both transports get it from one place.
//
// Deliberately conservative. The accepted alphabet excludes space, '.',
// '@' and ':', so an email address, a hostname, a sentence, or an
// address blob can never qualify no matter how long it is. Only a run
// that is ENTIRELY base64/hex alphabet for 512+ characters passes.
func looksLikeEncodedBlob(in string) bool {
	if len(in) < encodedBlobMinLen {
		return false
	}
	body := in
	// data:<mediatype>;base64,<payload> - scan the payload, not the header.
	// A data: URI with no ";base64," marker is not an encoded blob we
	// recognise, so it keeps going through the redact pass.
	if strings.HasPrefix(body, "data:") {
		i := strings.Index(body, ";base64,")
		if i < 0 {
			return false
		}
		body = body[i+len(";base64,"):]
		if len(body) < encodedBlobMinLen {
			return false
		}
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		// base64 ('+', '/', '='), base64url ('-', '_').
		case c == '+', c == '/', c == '=', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
