package retention

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// retainedTables maps each table holding per-site visitor data, submitted
// content or third-party payloads to the query method purgeSite must call to
// sweep it. Adding a table here without wiring purgeSite fails this test,
// which is the point.
//
// This is an explicit map rather than a name-derivation rule on purpose. The
// first version inferred the method from the table name and immediately
// false-failed on cwv_events, whose method is DeleteCWVBySiteOlderThan. A
// guard that reports a problem where none exists gets muted, and a muted guard
// is worse than none.
//
// It exists because the same omission shipped three times in this one package:
// conversion_events, form_submissions and payment_events were each created
// without a sweep, so their rows outlived every window the tenant configured
// while the sessions and events they reference were deleted on schedule. That
// is the estate's incomplete-migration pattern landing on data retention, and
// per the two-or-more-instances rule it earns a durable guard rather than a
// third spot fix.
//
// A tenant lowering analytics_retention_days is making a compliance statement.
// Every table here has to honour it or the statement is false.
var retainedTables = map[string]string{
	"visit_engagement":  "DeleteVisitEngagementBySiteOlderThan",
	"visit_events":      "DeleteVisitEventsBySiteOlderThan",
	"visit_sessions":    "DeleteVisitSessionsBySiteOlderThan",
	"consent_records":   "DeleteConsentBySiteOlderThan",
	"cwv_events":        "DeleteCWVBySiteOlderThan",
	"conversion_events": "DeleteConversionEventsBySiteOlderThan",
	"form_submissions":  "DeleteFormSubmissionsBySiteOlderThan",
	// The row survives (it is the webhook idempotency key; deleting it would
	// let a replayed delivery re-run the side effects) and only the provider
	// payload is blanked.
	"payment_events": "PurgePaymentEventPayloadsOlderThan",
	// Added with the table itself, which is the rule this guard enforces.
	// payload_json carries an identified visitor's email.
	"crm_outbox": "DeleteCRMOutboxBySiteOlderThan",
}

var purgeSiteCallRE = regexp.MustCompile(`m\.queries\.([A-Za-z0-9_]+)\(ctx`)

func TestPurgeSiteCoversEveryRetainedTable(t *testing.T) {
	src, err := os.ReadFile("retention.go")
	if err != nil {
		t.Fatalf("read retention.go: %v", err)
	}
	body := string(src)

	start := strings.Index(body, "func (m *Manager) purgeSite(")
	if start < 0 {
		t.Fatal("purgeSite not found; this test needs updating")
	}
	end := strings.Index(body[start+10:], "\nfunc ")
	if end < 0 {
		end = len(body) - start - 10
	}
	purge := body[start : start+10+end]

	called := map[string]bool{}
	for _, m := range purgeSiteCallRE.FindAllStringSubmatch(purge, -1) {
		called[m[1]] = true
	}
	if len(called) == 0 {
		t.Fatal("no query calls found inside purgeSite; the matcher is broken and this test proves nothing")
	}

	for table, method := range retainedTables {
		if !called[method] {
			t.Errorf("table %q holds per-site retained data but purgeSite never calls %s; "+
				"the tenant's retention window is a lie for that table",
				table, method)
		}
	}
}
