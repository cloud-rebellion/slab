package agent

import "strings"

// This file adapts the design playbook + eval playbook to the site's
// design-fidelity dial (design.fidelity setting, see profile.go).
//
// The playbook is the single source of truth for both agent guidance
// AND the critique rubric (critique.Run derives its checks from it),
// so overlaying it per fidelity moves guidance and grading together:
// an agent building in showcase mode is graded by the showcase rubric
// it was taught, never punished by a rubric it couldn't see.
//
// Structure: defaultDesignPlaybook() stays the canonical BALANCED
// playbook (existing sites keep byte-identical guidance and grades).
// Performance and showcase are overlays that swap only the
// taste-sensitive sections. Security, privacy, accessibility,
// self-hosted-fonts-only, and content-authenticity sections are never
// touched by an overlay, by construction: the overlay functions below
// simply do not reference them.

// FidelityInfo tells the agent which design-fidelity dial is active
// and exactly what that changes. Serialized at the top of the
// playbook so it is the first thing an agent reads.
type FidelityInfo struct {
	// Active is the resolved dial value: performance | balanced | showcase.
	Active string `json:"active"`
	// Meaning is the one-paragraph contract of this fidelity.
	Meaning string `json:"meaning"`
	// GradingImpact states how the Inspector grades differently under
	// this fidelity, so the agent knows the scoreboard it plays against.
	GradingImpact []string `json:"grading_impact"`
	// Unlocks lists capabilities this fidelity permits that balanced
	// does not (empty for balanced/performance).
	Unlocks []string `json:"unlocks,omitempty"`
	// StillEnforced lists the invariants no fidelity relaxes. Read this
	// before assuming showcase means anything-goes.
	StillEnforced []string `json:"still_enforced"`
	// HowToChange points at the settings row that owns the dial.
	HowToChange string `json:"how_to_change"`
}

// fidelityInvariants is shared verbatim across all three fidelities.
var fidelityInvariants = []string{
	"Security: sanitization, CSP script-src 'self', no <script> in custom blocks, SRI on cross-origin resources, no mixed content. Identical in every fidelity.",
	"Privacy: consent gating, retention windows, youtube-nocookie, no pre-consent trackers. Identical in every fidelity.",
	"Accessibility: alt text, labels, landmarks, focus indicators, heading hierarchy, contrast >= 4.5:1, prefers-reduced-motion honored on EVERY animation. Identical in every fidelity.",
	"Fonts: self-hosted woff2 only, never a font CDN. Identical in every fidelity.",
	"Content authenticity: no fake names/numbers/companies, no AI-cliche copy, no lorem ipsum. Slop lint blocks writes in every fidelity.",
	"SEO meta honesty: titles, descriptions, canonical, og:image completeness graded identically in every fidelity.",
}

// DesignPlaybookFor returns the playbook adapted to a design fidelity.
// FidelityBalanced returns the canonical playbook (plus the fidelity
// header); performance and showcase overlay the taste sections.
func DesignPlaybookFor(f DesignFidelity) DesignPlaybookInfo {
	pb := defaultDesignPlaybook()
	switch f {
	case FidelityPerformance:
		applyPerformanceFidelity(&pb)
	case FidelityShowcase:
		applyShowcaseFidelity(&pb)
	default:
		pb.Fidelity = FidelityInfo{
			Active:  string(FidelityBalanced),
			Meaning: "Balanced is the default: the full taste rulebook below applies as written, and the Inspector grades with the standard budgets. Pick performance fidelity when the brief demands perfect scores on every category; pick showcase fidelity when the brief demands a jaw-dropping site and the operator accepts a measured speed trade.",
			GradingImpact: []string{
				"Standard budgets: 200KB HTML/page, 50KB inline CSS, motion cap 1 perpetual animation per viewport, canonical design tokens enforced.",
			},
			StillEnforced: fidelityInvariants,
			HowToChange:   "site_settings category=design key=fidelity, one of performance|balanced|showcase. Agent-writable via bulk_upsert_settings; also on the admin Settings -> General page. Change it BEFORE authoring, not after grading.",
		}
	}
	return pb
}

// EvalPlaybookFor adapts the eval playbook's targets to the fidelity so
// the agent converges on the right scoreboard instead of fighting the
// dial.
func EvalPlaybookFor(siteID string, f DesignFidelity) EvalPlaybookInfo {
	ep := defaultEvalPlaybook(siteID)
	switch f {
	case FidelityPerformance:
		ep.Goal = strings.Replace(ep.Goal,
			"Drive the site to 95%+ on agent-writable eval categories.",
			"PERFORMANCE FIDELITY: drive the site to A+ (97%+) on EVERY category. Prefer static hero graphics (monogram, audit-receipt), logo_strip over logo_carousel, no bg=circuit canvas, no perpetual motion, imagery only where it earns its bytes. Perfect scores are the deliverable.",
			1)
	case FidelityShowcase:
		ep.Goal = strings.Replace(ep.Goal,
			"Drive the site to 95%+ on agent-writable eval categories.",
			"SHOWCASE FIDELITY: the operator chose design ambition over raw speed. Targets: security/accessibility/privacy >= A- (non-negotiable), seo >= B, performance >= C+ graded against the showcase budgets (400KB HTML/page, 150KB inline CSS, relaxed lazy ratio, CWV weights halved), design >= B on the showcase rubric. Spend the freed budget on craft: layered motion, scroll-driven reveals, bespoke custom blocks, view transitions.",
			1)
	}
	return ep
}

// --- performance overlay ---------------------------------------------------

func applyPerformanceFidelity(pb *DesignPlaybookInfo) {
	pb.Fidelity = FidelityInfo{
		Active:  string(FidelityPerformance),
		Meaning: "Performance fidelity targets perfect scores: every category at A+. The taste rulebook below applies as written, PLUS the subtractions in grading_impact. The renderer emits the same CSS as balanced; the discipline is in what you choose to author.",
		GradingImpact: []string{
			"Grading budgets are the same as balanced (they already permit A+); what changes is your authoring posture: zero perpetual motion, zero canvas, static hero graphics only.",
			"Prefer monogram or audit-receipt hero graphics (zero animation, best LCP). Do NOT use bg=circuit, mesh, or pulse.",
			"Prefer logo_strip (static) over logo_carousel (perpetual marquee).",
			"Every image through the media pipeline (auto WebP + dimensions), loading=lazy on everything below the fold, no decorative imagery that doesn't carry information.",
			"Aim: measured CWV p75 LCP < 1.5s, CLS < 0.05 once traffic lands.",
		},
		StillEnforced: fidelityInvariants,
		HowToChange:   "site_settings category=design key=fidelity. If the brief later demands more visual ambition, flip to balanced or showcase and re-run the build; the Inspector re-grades against the new budgets.",
	}
	// Motion: subtract the perpetual budget entirely.
	pb.Motion.DontUse = append(pb.Motion.DontUse,
		"PERFORMANCE FIDELITY: perpetual animation of ANY kind, including the logo_carousel marquee and bg=circuit canvas. Use logo_strip and a static hero graphic (monogram, audit-receipt) instead. Load-once entry transitions on :hover/:active remain fine.",
	)
	replaceAuditItem(pb, "Eval grade ≥ B on all categories",
		AuditItem{Check: "Eval grade A+ on every category (performance fidelity). Splash and 404 aside, every agent-fixable item passes.", Why: "Performance fidelity's contract is perfect scores. Below A+ = keep iterating the build loop."})
}

// --- showcase overlay --------------------------------------------------------

func applyShowcaseFidelity(pb *DesignPlaybookInfo) {
	pb.Fidelity = FidelityInfo{
		Active:  string(FidelityShowcase),
		Meaning: "Showcase fidelity is the jaw-dropping dial: the operator accepts a measured speed trade for Google-Stitch-level design ambition. Build with intent: pick ONE vibe archetype, choreograph motion deliberately, and make every bespoke choice cohere. Freedom is not soup; the Inspector still grades cohesion, copy honesty, and accessibility at full strength.",
		GradingImpact: []string{
			"Page budgets grade against showcase limits: 400KB HTML/page, 150KB inline CSS, 8MB total, lazy-load ratio relaxed to 3/5.",
			"Measured CWV weights are halved for LCP/INP/FCP/TTFB (CLS keeps full weight; jank is never a design choice). Google's thresholds are unchanged, so the numbers stay honest.",
			"Motion budget: up to 4 perpetual animations per viewport (Inspector warns at 5, errors at 7). EVERY animation must be covered by prefers-reduced-motion.",
			"Design-token drift (bespoke radii, shadows, beziers, palette width) is advisory instead of scored: bespoke tokens are allowed when they cohere with the chosen vibe.",
			"Gradient, pure-black (OLED), full-bleed image hero, and Title Case detectors are advisory instead of scored.",
			"Sparse-copy landing checks (300+ words, meta-CTA verb, AI-friendly formatting) are advisory instead of scored.",
			"Every evaluation row records fidelity=showcase, so an A here is never confused with an A in performance fidelity.",
		},
		Unlocks: []string{
			"fx utility classes (emitted by the renderer in showcase only, all reduced-motion-gated): .fx-reveal (scroll-driven rise+fade), .fx-parallax-slow / .fx-parallax-fast (scroll-driven drift), .fx-float (ambient bob), .fx-shimmer (specular sweep), .fx-gradient-text (brand gradient headline fill), .fx-aurora-bg (drifting aurora background band), .fx-stagger (entry cascade on children, 90ms steps), .fx-tilt (hover lift+tilt on cards).",
			"Astro view transitions: the layout ships ClientRouter in showcase, so cross-page navigation morphs instead of flashing. Keep page shells consistent for clean morphs.",
			"aurora hero graphic (hero_graphic=aurora): full-bleed drifting aurora gradient behind the hero, dark-vibe ready.",
			"Scroll-driven animation is CSS-only (animation-timeline: view()), Chrome/Edge get the choreography, Firefox/Safari degrade gracefully to the static layout. NEVER JS scroll listeners.",
			"custom + raw_astro blocks are a first-class path (not a last resort): use them for bespoke sections a built-in primitive would flatten. Pair with css_classes for CSS and registered components for interactivity; <script> in block markup stays banned (CSP).",
			"Bespoke materiality: layered/tinted shadows, glass cards (backdrop-blur, prefer fixed/sticky or small surfaces), deliberate glow accents in dark vibes, expressive radii families.",
			"Color-blocking: full-bleed brand or dark bands are allowed when the vibe calls for them; keep text contrast >= 4.5:1 (--color-on-primary) always.",
			"style_json remains inert in the renderer (reserved). Express bespoke looks via css_classes + the fx utilities, not style_json.",
		},
		StillEnforced: fidelityInvariants,
		HowToChange:   "site_settings category=design key=fidelity. Flip back to balanced/performance anytime; the next build re-grades against the new budgets.",
	}

	// Stack: the expressive surface is wider in showcase.
	pb.Stack += " SHOWCASE FIDELITY: additionally, the renderer emits the fx-* utility layer (scroll-driven reveals, parallax, ambient motion, gradient text, aurora bands, stagger, tilt) and Astro view transitions, and custom/raw_astro blocks + css_classes are the sanctioned bespoke path. All fx utilities are prefers-reduced-motion-gated by the renderer."

	// Motion: rebuild the budget around deliberate choreography.
	pb.Motion.DoUse = append(pb.Motion.DoUse,
		"Scroll-driven CSS animations via the fx utilities (.fx-reveal, .fx-parallax-slow/-fast): animation-timeline: view(), zero JS, reduced-motion-gated, Firefox/Safari degrade to static.",
		"Layered ambient motion, deliberately: up to 4 perpetual animations per viewport when they share one visual system (e.g. aurora band + float + shimmer on the same hero).",
		"Entry choreography via .fx-stagger: children cascade in at 90ms steps on first view.",
		"Astro view transitions (ClientRouter ships in the layout for showcase sites): design page shells so shared elements morph cleanly.",
	)
	replaceListEntry(pb.Motion.DontUse,
		"More than ONE perpetual animation per viewport",
		"More than FOUR perpetual animations per viewport (the Inspector warns at 5, errors at 7). Layering is allowed in showcase, soup is not: every ambient signal must belong to the same visual system.")
	replaceListEntry(pb.Motion.DontUse,
		"Parallax. Scroll-tied transforms of any kind",
		"JS scroll listeners of any kind. Scroll choreography is CSS-only via the fx utilities (animation-timeline: view()); window.addEventListener('scroll') stays banned in every fidelity.")
	replaceListEntry(pb.Motion.DontUse,
		"Entry-choreography staggers longer than 80ms",
		"Entry-choreography staggers longer than 120ms between siblings; longer reads as slow, not premium (.fx-stagger ships 90ms).")
	replaceListEntry(pb.Motion.DontUse,
		"GSAP, ThreeJS, Lottie unless registered as a custom component",
		"GSAP, ThreeJS, Lottie loaded from a CDN, never allowed. Self-hosted + registered as a custom component is the sanctioned path when CSS genuinely can't express the concept (CSP + SRI still apply). Try the fx utilities first.")
	replaceListEntry(pb.Motion.Performance,
		"backdrop-blur on FIXED elements only",
		"backdrop-blur on fixed/sticky elements and small glass surfaces (cards, badges). Avoid blurring large scrolling regions; blur cost scales with area.")

	// Materiality: glow + blur become deliberate tools, not bans.
	replaceListEntry(pb.Materiality.DontUse,
		"Neon outer glows",
		"Neon glows as a DEFAULT card treatment. In showcase dark vibes (Ethereal Glass, Cinematic Aurora) a restrained glow accent on the hero focal point or primary CTA is allowed, one glow per viewport, tinted to the accent, never on body text.")
	replaceListEntry(pb.Materiality.DontUse,
		"backdrop-blur on scrolling content",
		"backdrop-blur on LARGE scrolling regions. Glass cards and badges may blur in showcase; keep blurred surfaces small and finite.")

	// Color: budget widens, contrast does not.
	replaceListEntry(pb.Color.UsageRules,
		"Primary colour appears in 4-7 places",
		"Brand-colour budget is relaxed in showcase: color-blocking, gradient fills, and full-bleed brand bands are allowed when the chosen vibe calls for them. Restraint still reads expensive; spend deliberately.")
	replaceListEntry(pb.Color.UsageRules,
		"NEVER use --color-primary as a section background",
		"--color-primary as a section background is allowed for deliberate full-bleed bands (one or two per page). Set --color-on-primary so all text on it clears 4.5:1 contrast.")

	// Spacing: bespoke rhythm through the sanctioned lever.
	replaceListEntry(pb.Spacing.SectionRhythm,
		"NEVER set padding-block via style_json",
		"Bespoke section rhythm (oversized editorial gaps, full-bleed bands, overlap illusions) is authored via custom blocks + css_classes in showcase. style_json remains inert in the renderer, do not use it.")

	// Block selection: custom is first-class, scope widens.
	for i := range pb.BlockSelection {
		switch {
		case strings.HasPrefix(pb.BlockSelection[i].Question, "How many blocks per page?"):
			pb.BlockSelection[i].Choose = "Homepage 8-14. Subpage 4-8. Long-scroll narrative pages can go higher when every section earns its place."
			pb.BlockSelection[i].Why = "Showcase pages may run longer, but fatigue is real: each added section must advance the story or it dilutes the craft."
		case strings.HasPrefix(pb.BlockSelection[i].Question, "When to use the custom block_type?"):
			pb.BlockSelection[i].Choose = "In showcase, custom + raw_astro are first-class: reach for them whenever a built-in primitive would flatten the concept. Pair with css_classes and the fx-* utilities; run lint_block before committing and design_critique after building."
			pb.BlockSelection[i].Why = "Bespoke sections are where jaw-dropping lives. The built-ins remain the fastest path for standard patterns; bespoke is for the moments that define the site."
		}
	}

	// Anti-patterns: reword the taste bans the showcase rubric demotes to
	// advisory. Keep each entry's key phrase intact (the critique registry
	// binds on it) and keep every authenticity entry untouched.
	for i := range pb.AntiPatterns {
		ap := &pb.AntiPatterns[i]
		switch {
		case strings.Contains(ap.Banned, "AI gradient"):
			ap.Preferred = "In showcase this is ADVISORY: considered gradients (including purple-family mesh/aurora in dark vibes) are allowed via .fx-gradient-text / .fx-aurora-bg / hero_graphic=mesh|aurora. The ban is on the UNCONSIDERED default, an indigo-violet gradient slapped on a generic template. If the gradient is your vibe's signature, commit to it coherently."
		case strings.Contains(ap.Banned, "Pure black (#000000)"):
			ap.Preferred = "In showcase this is ADVISORY: true black is legitimate for OLED dark vibes (Ethereal Glass). Everywhere else prefer off-black (#0a0a0a / #18181b), pure black next to saturated colour vibrates."
		case strings.Contains(ap.Banned, "Centered hero with text over an image"):
			ap.Preferred = "In showcase this is ADVISORY: a full-bleed image hero is allowed when text sits on a scrim or gradient overlay that keeps contrast >= 4.5:1, and the image is art-directed (not a stock fallback). split_hero and graphic heroes remain the safer defaults."
		case strings.Contains(ap.Banned, "Title Case On Every Heading"):
			ap.Preferred = "In showcase this is ADVISORY: editorial and luxury voices may run Title Case deliberately. Pick one casing system per site and hold it everywhere."
		case strings.Contains(ap.Banned, "Linear or ease-in-out CSS transitions"):
			ap.Preferred = "In showcase this is ADVISORY for choreography that genuinely wants constant velocity (marquees, scroll-linked drift). For UI state changes, weighted beziers still read premium: cubic-bezier(0.16, 1, 0.3, 1) soft landing, cubic-bezier(0.32, 0.72, 0, 1) overshoot, cubic-bezier(0.34, 1.56, 0.64, 1) spring."
		}
	}

	// Vibes: drop the hedging, add the showcase-native archetype.
	for i := range pb.VibeArchetypes {
		v := &pb.VibeArchetypes[i]
		v.ApplyVia = strings.ReplaceAll(v.ApplyVia, "Use sparingly, dark sites convert worse than light for B2B; only ship this for genuine 'we build futuristic AI' brands.", "In showcase, commit fully when the brand fits: pair with .fx-aurora-bg or hero_graphic=mesh, glass cards, and one glow accent.")
		v.ApplyVia = strings.ReplaceAll(v.ApplyVia, "Use sparingly, neo-brutalism wins attention but loses trust on enterprise B2B.", "In showcase, commit fully when the brand fits: hard offset shadows + mono display type + one saturated accent, held everywhere.")
	}
	pb.VibeArchetypes = append(pb.VibeArchetypes, VibeArchetype{
		Name:         "Cinematic Aurora",
		BestFor:      []string{"Launches and waitlists", "Creative tools", "AI products with warmth", "Portfolio statements", "Showcase-fidelity sites that want motion as identity"},
		Palette:      "Near-black dusk base #0b0b14, aurora accents drifting between two hues picked from the brand (e.g. teal -> violet), warm white text #fafaf9. Aurora carries the colour budget; everything else stays neutral.",
		Typography:   "Oversized display grotesk (Space Grotesk, Geist, or Cabinet Grotesk) at clamp(3rem, 8vw, 6.5rem), tracking -0.03em, weight 600-700. Body in the same family at 1.0625rem. Mono for eyebrows + data.",
		Materiality:  "Full-bleed .fx-aurora-bg band behind the hero, glass cards (backdrop-blur, white/8 hairline, inset top highlight), one restrained glow accent on the primary CTA. Scroll-driven .fx-reveal on every section entry; .fx-parallax-slow on decorative layers only.",
		BgColor:      "#0b0b14",
		PrimaryColor: "#22d3ee",
		TextColor:    "#fafaf9",
		FontHeading:  "Space Grotesk",
		FontBody:     "Space Grotesk",
		ApplyVia:     "SHOWCASE ONLY (needs the fx layer). branding.bg_color=#0b0b14, text_color=#fafaf9, primary_color=<brand accent>. Hero: hero_graphic=aurora or a custom block wrapped in .fx-aurora-bg. Choreograph: hero aurora (perpetual 1) + .fx-float on the hero card (perpetual 2), .fx-reveal on sections, .fx-stagger on grids. That is 2 of the 4-perpetual budget spent; stop there unless a third signal shares the system.",
	})

	// Audit checklist: the grade gate + scope + motion coverage change.
	replaceAuditItem(pb, "Eval grade ≥ B on all categories",
		AuditItem{Check: "Eval: security, accessibility, privacy >= A-. seo >= B. performance >= C+ against showcase budgets. design >= B on the showcase rubric.", Why: "Showcase trades speed for craft, never safety. The Inspector grades against showcase budgets and stamps the fidelity on the evaluation row."})
	replaceAuditItem(pb, "Final page count ≤ 11",
		AuditItem{Check: "Final page count <= 14. More = scope creep unless the operator explicitly asked for a larger site.", Why: "Showcase earns depth per page, not page sprawl."})
	pb.AuditChecklist = append(pb.AuditChecklist,
		AuditItem{Check: "Every perpetual animation (incl. fx utilities and custom @keyframes) is covered by prefers-reduced-motion: reduce.", Why: "A11y invariant. The renderer gates its own fx layer; custom css_classes animations must ship their own gate. The Inspector fails uncovered keyframes in showcase."},
		AuditItem{Check: "One vibe archetype chosen and held: palette, type, materiality, and motion all belong to the same system.", Why: "Freedom without coherence is soup. Cohesion is what separates jaw-dropping from AI-builder demo."},
	)

	// Hero graphics: advertise the showcase-only aurora variant.
	pb.HeroGraphics = append(pb.HeroGraphics, HeroGraphic{
		Name:        "aurora",
		BestFor:     []string{"Cinematic Aurora vibe", "Launches", "Dark showcase heroes", "Sites where motion is the identity"},
		Description: "Full-bleed drifting aurora gradient behind the hero: two brand-derived hue bands slowly sweeping on a near-black base. Renders (animated) in ANY fidelity; it is advertised here because it belongs to the showcase motion budget, and in balanced fidelity it spends the page's single perpetual-motion slot.",
		Materiality: "Pure CSS (layered radial-gradients on the element and ::after, drift keyframes ~26s). Masked fade to bg at the lower edge. prefers-reduced-motion: static gradient.",
		Performance: "~2kb CSS, transform+opacity only. Content paints over it on first frame; LCP unaffected.",
	})

	// Design workflow: stitch-design leads the showcase authoring loop.
	// No case surgery on the original sentence: it starts with the
	// all-caps word BEFORE, which lowercasing garbled into bEFORE.
	pb.DesignWorkflow.WhenToInvoke = "SHOWCASE FIDELITY: FIRST invoke the stitch-design skill to generate the site's DESIGN.md (taste dials: density/variance/motion, calibrated palette, motion philosophy) and treat it as the source of truth for every subsequent authoring decision. After that, the standard rule applies: " + pb.DesignWorkflow.WhenToInvoke
	pb.DesignWorkflow.SkillsByVibe = append([]SkillVibeMapping{{
		Vibe:        "ALL vibes (showcase fidelity)",
		Skill:       "stitch-design",
		HowToInvoke: "Invoke first, before any authoring. Generate a DESIGN.md with variance 7-8, motion 6-7, density to fit the brief; encode the chosen vibe's palette/typography/materiality and the anti-pattern bans. Every custom block and css_class you author afterwards must trace back to it.",
	}}, pb.DesignWorkflow.SkillsByVibe...)
	replaceListEntry(pb.DesignWorkflow.BeforeAuthoring,
		"For inspector-coherent custom blocks: keep hard-coded values inside the canonical token allowlist",
		"Token drift is ADVISORY in showcase: bespoke radii, layered shadows, and custom beziers are allowed when they cohere with the DESIGN.md. The design_critique tool still reports drift so you can check cohesion; it just doesn't cost points.")
	replaceListEntry(pb.DesignWorkflow.BeforeAuthoring,
		"Max ONE perpetual animation per page",
		"Max FOUR perpetual animations per viewport, all belonging to one visual system, all reduced-motion-gated. The Inspector warns at 5 and errors at 7.")
}

// --- helpers -----------------------------------------------------------------

// replaceListEntry swaps the first list entry containing marker for the
// replacement text, in place. No-op when the marker is absent (defensive:
// a reworded balanced playbook must never panic an overlay).
func replaceListEntry(list []string, marker, replacement string) {
	for i := range list {
		if strings.Contains(list[i], marker) {
			list[i] = replacement
			return
		}
	}
}

// replaceAuditItem swaps the first audit item whose Check contains marker.
func replaceAuditItem(pb *DesignPlaybookInfo, marker string, item AuditItem) {
	for i := range pb.AuditChecklist {
		if strings.Contains(pb.AuditChecklist[i].Check, marker) {
			pb.AuditChecklist[i] = item
			return
		}
	}
}
