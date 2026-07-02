package critique

import (
	"strings"
	"testing"

	"github.com/bright-interaction/slab/internal/agent"
)

func lc(html, css, text string, headings ...string) antiPatternInput {
	// Mirrors antiPatternChecks: html/css/text arrive pre-lowercased.
	return antiPatternInput{htmlLower: html, cssLower: css, textLower: text, headings: headings}
}

func TestAntiPatternDetectors(t *testing.T) {
	cases := []struct {
		name string
		fn   func(antiPatternInput) bool
		in   antiPatternInput
		want bool
	}{
		{"pure black hex", detectPureBlack, lc("", "color:#000000;", ""), true},
		{"pure black short", detectPureBlack, lc("", "color:#000;", ""), true},
		{"off-black ok", detectPureBlack, lc("", "color:#18181b;", ""), false},
		{"four-digit hex not black", detectPureBlack, lc("", "color:#0001;", ""), false},

		{"inter primary heading", detectInterHeading, lc("", "--font-heading: 'inter', sans-serif;", ""), true},
		{"inter fallback ok", detectInterHeading, lc("", "--font-heading: 'space grotesk', inter, sans-serif;", ""), false},
		{"arial heading", detectGenericHeadingFont, lc("", "--font-heading: arial, sans-serif;", ""), true},
		{"space grotesk ok", detectGenericHeadingFont, lc("", "--font-heading: 'space grotesk';", ""), false},

		{"ai purple gradient", detectAIGradient, lc("", "background:linear-gradient(90deg,#6366f1,#a855f7);", ""), true},
		{"teal gradient ok", detectAIGradient, lc("", "background:linear-gradient(90deg,#0e7490,#155e75);", ""), false},

		{"ease-in-out transition", detectBlandTransition, lc("", "transition: transform 0.2s ease-in-out;", ""), true},
		{"premium bezier ok", detectBlandTransition, lc("", "transition: transform 0.2s cubic-bezier(0.16,1,0.3,1);", ""), false},

		{"100vh bug", detectFixedViewportHeight, lc("", "min-height:100vh;", ""), true},
		{"100dvh ok", detectFixedViewportHeight, lc("", "min-height:100dvh;", ""), false},

		{"hero over image", detectHeroOverImage, lc(`<section class="block block--hero"><h1>x</h1><img src="a.jpg"></section>`, "", ""), true},
		{"split hero ok", detectHeroOverImage, lc(`<section class="block block--split_hero"><h1>x</h1><img src="a.jpg"></section>`, "", ""), false},

		{"fake name", detectFakeNames, lc("", "", "quote from john doe, ceo"), true},
		{"fake number", detectFakeNumbers, lc("", "", "only $100.00 per month"), true},
		{"fake company", detectFakeCompanies, lc("", "", "trusted by acme corp"), true},
		{"ai cliche", detectAICliches, lc("", "", "we elevate your seamless workflow"), true},
		{"clean copy ok", detectAICliches, lc("", "", "we audit your servers and migrate your crm"), false},
		{"lorem ipsum", detectLorem, lc("", "", "lorem ipsum dolor sit amet"), true},

		{"exclamation heading", detectExclamationCTAs, lc("", "", "", "Audit booked!"), true},
		{"calm heading ok", detectExclamationCTAs, lc("", "", "", "Audit booked."), false},

		{"title case headings", detectTitleCaseHeadings, lc("", "", "", "How We Build Secure Sites", "Why You Need This Now"), true},
		{"sentence case ok", detectTitleCaseHeadings, lc("", "", "", "How we build secure sites", "Why you need this now"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.fn(tc.in); got != tc.want {
				t.Fatalf("%s = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestAntiPatternChecks_RealPlaybookScoresMany proves the rewrite scores far
// more than the old ~6 needled entries: against the real playbook a clean site
// should yield a healthy number of weighted Pass checks (not zero-weight Info).
func TestAntiPatternChecks_RealPlaybookScoresMany(t *testing.T) {
	pb := agent.DefaultDesignPlaybook()
	if len(pb.AntiPatterns) < 20 {
		t.Fatalf("expected ~25 anti-patterns in playbook, got %d", len(pb.AntiPatterns))
	}
	// Clean site: no banned signals anywhere.
	checks := antiPatternChecks(pb.AntiPatterns,
		`<section class="block block--hero"><h1>Replace your scanner today</h1></section>`,
		"--font-heading:'space grotesk';color:#18181b;transition:transform .2s cubic-bezier(0.16,1,0.3,1);",
		"we audit servers and migrate crm for swedish law firms",
		[]string{"replace your scanner", "why teams switch to us"},
		agent.FidelityBalanced)

	scored, fails := 0, 0
	for _, c := range checks {
		if c.Passed != nil { // weighted Pass/Fail; Info has Passed==nil (no scoring)
			scored++
			if !*c.Passed {
				fails++
			}
		}
	}
	if scored < 12 {
		t.Fatalf("expected >=12 weighted anti-pattern checks after the rewrite, got %d (was ~6 before)", scored)
	}
	if fails != 0 {
		t.Fatalf("clean site should fail no anti-pattern check, got %d fails", fails)
	}
}

// TestAntiPatternChecks_SlopSiteFails confirms a slop-laden site trips multiple
// real Fails (the whole point of the rewrite).
func TestAntiPatternChecks_SlopSiteFails(t *testing.T) {
	pb := agent.DefaultDesignPlaybook()
	checks := antiPatternChecks(pb.AntiPatterns,
		`<section class="block block--hero"><h1>Elevate Your Business Today</h1><img src="bg.jpg"></section>`,
		"--font-heading:'inter';color:#000000;background:linear-gradient(90deg,#8b5cf6,#6366f1);transition:all 0.2s ease-in-out;min-height:100vh;",
		"lorem ipsum dolor. trusted by acme corp. quote from john doe. only $100.00 today!",
		[]string{"Elevate Your Business Today", "Unleash Seamless Growth Now"},
		agent.FidelityBalanced)

	fails := 0
	for _, c := range checks {
		if c.Passed != nil && !*c.Passed {
			fails++
		}
	}
	if fails < 6 {
		t.Fatalf("slop site should trip many anti-pattern fails, got %d", fails)
	}
}

// TestAntiPatternChecks_ShowcaseDemotesTasteKeepsAuthenticity is the
// fidelity-dial contract: under showcase the taste detectors (gradient,
// pure black, Inter, hero-over-image, title case, bland transitions)
// emit advisory Info, while authenticity (slop names/numbers/companies,
// AI cliches, lorem) and correctness (100vh) still FAIL with weight.
func TestAntiPatternChecks_ShowcaseDemotesTasteKeepsAuthenticity(t *testing.T) {
	pb := agent.DesignPlaybookFor(agent.FidelityShowcase)
	checks := antiPatternChecks(pb.AntiPatterns,
		`<section class="block block--hero"><h1>Elevate Your Business Today</h1><img src="bg.jpg"></section>`,
		"--font-heading:'inter';color:#000000;background:linear-gradient(90deg,#8b5cf6,#6366f1);transition:all 0.2s ease-in-out;min-height:100vh;",
		"lorem ipsum dolor. we elevate your seamless workflow. trusted by acme corp. quote from john doe. only $100.00 today!",
		[]string{"Elevate Your Business Today", "Unleash Seamless Growth Now"},
		agent.FidelityShowcase)

	failNames := map[string]bool{}
	for _, c := range checks {
		if c.Passed != nil && !*c.Passed {
			failNames[c.Name] = true
		}
	}
	// Authenticity + correctness must still fail.
	for _, want := range []string{"John Doe", "Acme Corp", "AI copywriting", "Lorem ipsum", "h-screen", "Round fake numbers"} {
		found := false
		for name := range failNames {
			if containsFold(name, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("showcase must still fail authenticity/correctness rule containing %q; fails: %v", want, failNames)
		}
	}
	// Taste must NOT fail (demoted to Info).
	for _, banned := range []string{"AI gradient", "Pure black", "Inter font", "Title Case"} {
		for name := range failNames {
			if containsFold(name, banned) {
				t.Fatalf("showcase must demote taste rule %q to advisory, but it failed", name)
			}
		}
	}
}

func containsFold(haystack, needle string) bool {
	return len(needle) <= len(haystack) &&
		strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}
