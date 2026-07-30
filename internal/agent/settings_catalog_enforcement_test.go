package agent

import (
	"fmt"
	"testing"

	"github.com/bright-interaction/slab/internal/settingspolicy"
)

// The catalog declares ValueType, MinInt, MaxInt and EnumValues, and the admin
// UI and the agent both read those as the contract. Nothing enforced them:
// settingspolicy has a hand-written switch, and seven of the ten constrained
// agent-writable settings were absent from it, so the declared constraint was
// decorative. general.mobile_breakpoint is interpolated into
// `--bp-mobile: <value>px;` inside the :root block of the generated
// stylesheet, so an unenforced value closes the rule and blanks the site with
// its own CSS, which no CSP can stop.
//
// The two live in different packages and cannot be merged without an import
// cycle (agent already imports settingspolicy), so this test is the join:
// it walks the real catalog and asserts the real validator honours every
// constraint the catalog advertises. A new catalog entry with a constraint and
// no validator fails here.
func TestCatalogConstraintsAreEnforced(t *testing.T) {
	catalog := buildSettingsCatalog("site-under-test", map[string]string{})

	checked := 0
	for _, d := range catalog.Items {
		if !d.AgentWritable {
			continue
		}

		// Enum: every listed value must be accepted, and a value that is
		// obviously not in the list must be rejected.
		if len(d.EnumValues) > 0 {
			checked++
			for _, ok := range d.EnumValues {
				if err := settingspolicy.ValidateSetting(d.Category, d.Key, ok); err != nil {
					t.Errorf("%s.%s: catalog lists %q as valid but the validator rejects it: %v",
						d.Category, d.Key, ok, err)
				}
			}
			const bogus = "definitely-not-an-allowed-enum-value"
			if err := settingspolicy.ValidateSetting(d.Category, d.Key, bogus); err == nil {
				t.Errorf("%s.%s: catalog declares EnumValues %v but the validator accepts %q; the constraint is decorative",
					d.Category, d.Key, d.EnumValues, bogus)
			}
		}

		// Int range: the bounds must be accepted and just outside must not.
		if d.MinInt != 0 || d.MaxInt != 0 {
			checked++
			for _, in := range []int64{d.MinInt, d.MaxInt} {
				if err := settingspolicy.ValidateSetting(d.Category, d.Key, fmt.Sprint(in)); err != nil {
					t.Errorf("%s.%s: catalog bound %d is rejected by the validator: %v",
						d.Category, d.Key, in, err)
				}
			}
			for _, out := range []string{
				fmt.Sprint(d.MinInt - 1),
				fmt.Sprint(d.MaxInt + 1),
				"not-a-number",
				// The CSS-injection payload. This is the actual attack on the
				// breakpoints, not a synthetic out-of-range value.
				"0px } html{display:none!important} :root{--x: 0",
			} {
				if err := settingspolicy.ValidateSetting(d.Category, d.Key, out); err == nil {
					t.Errorf("%s.%s: catalog declares range [%d,%d] but the validator accepts %q; the constraint is decorative",
						d.Category, d.Key, d.MinInt, d.MaxInt, out)
				}
			}
		}
	}

	// Guard the guard: if the catalog is refactored so no entry carries a
	// constraint, this test would pass vacuously while proving nothing.
	if checked == 0 {
		t.Fatal("no constrained agent-writable settings were checked; this test is no longer testing anything")
	}
	t.Logf("checked %d constrained agent-writable settings", checked)
}
