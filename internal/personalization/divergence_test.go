package personalization

import "testing"

// Pins the KNOWN divergences between this package's Eval and the JavaScript
// evaluator that actually ships (internal/builder/personalization.go). The
// package doc claims the two stay in lock-step; for Eval that is false, and
// this test is what stops that claim rotting back into something people trust.
//
// If you make the two agree, these expectations flip, and the right move then is
// to generate both evaluators from one table rather than hand-syncing them.
func TestEvalDivergesFromShippingJS(t *testing.T) {
	tests := []struct {
		expr   string
		vars   map[string]any
		goWant bool
		jsWant bool // what the shipping client script would return
		why    string
	}{
		{"x present", map[string]any{"x": false}, false, true, "Go isEmpty treats false as absent; JS checks !== \"\" && != null"},
		// float64, not int. isEmpty has a float64 case and NO int case, so an
		// int literal falls through to "not empty" and does NOT diverge. The
		// real path unmarshals visitor attributes from JSON, and encoding/json
		// yields float64 for every number, so float64 is the case that actually
		// occurs. Measured both before writing this.
		{"x present", map[string]any{"x": float64(0)}, false, true, "Go isEmpty treats float64(0) as absent; JS does not"},
		{"flag == 0", map[string]any{"flag": false}, false, true, "Go toNumber refuses a bool so it compares \"false\" == \"0\"; JS Number(false) is 0"},
		{"x == 0", map[string]any{"x": ""}, false, true, "Go treats \"\" as non-numeric; JS Number(\"\") is 0"},

		// NOT divergent, verified by measurement, listed so nobody re-adds it:
		//   "x present" with an int 0      -> true in both (isEmpty has no int case)
		//   "x == 0"    with float64(0)    -> true in both

	}
	for _, tt := range tests {
		got, err := Eval(tt.expr, tt.vars)
		if err != nil {
			t.Fatalf("Eval(%q) errored: %v", tt.expr, err)
		}
		if got != tt.goWant {
			t.Errorf("Eval(%q, %v) = %v, want %v (this package's documented behaviour)", tt.expr, tt.vars, got, tt.goWant)
		}
		if tt.goWant == tt.jsWant {
			t.Errorf("Eval(%q) no longer diverges from the shipping JS; if the two were unified, "+
				"delete this case and generate both evaluators from one table. %s", tt.expr, tt.why)
		}
	}
}

// Validate IS shared with production (agent/guardrails.go calls it), so it must
// keep accepting the real DSL and rejecting junk.
func TestValidateStillGuardsTheDSL(t *testing.T) {
	for _, ok := range []string{"plan present", "plan == enterprise", "visits > 3", "utm_source != google"} {
		if err := Validate(ok); err != nil {
			t.Errorf("Validate(%q) rejected a valid condition: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "plan", "plan ==", "== enterprise", "plan >>> 3", "1 == 1 ; DROP TABLE"} {
		if err := Validate(bad); err == nil {
			t.Errorf("Validate(%q) accepted junk", bad)
		}
	}
}
