package builder

import (
	"fmt"
	"strings"
)

// writeShowcaseFX emits the fx utility layer, the showcase-fidelity
// expressive vocabulary. Only emitted when design.fidelity=showcase
// (BuildCSS gates it), so balanced/performance builds stay
// byte-identical.
//
// Design contract (mirrors the showcase playbook + stitch-design DNA):
//   - transform + opacity only, hardware-accelerated, no layout thrash
//   - scroll choreography is CSS scroll-driven animation
//     (animation-timeline: view()) inside @supports, so Chrome/Edge get
//     the motion and Firefox/Safari degrade to the static layout
//   - EVERY effect is gated by prefers-reduced-motion (the trailing
//     reduce block kills all fx motion at once)
//   - weighted spring-feel beziers, never linear/ease-in-out for entries
//   - perpetual effects (.fx-float, .fx-shimmer, .fx-aurora-bg) count
//     against the Inspector's showcase motion budget of 4 per viewport
//
// Class catalog (advertised in design_playbook.fidelity.unlocks):
//
//	.fx-reveal         one-shot rise+fade as the element scrolls into view
//	.fx-reveal-scale   one-shot scale+fade variant for imagery/cards
//	.fx-parallax-slow  gentle scroll-linked drift (decorative layers only)
//	.fx-parallax-fast  stronger drift, use on small ornaments only
//	.fx-float          perpetual ambient bob (6s)
//	.fx-shimmer        perpetual specular sweep across text/borders
//	.fx-gradient-text  brand gradient headline fill (static)
//	.fx-aurora-bg      perpetual drifting aurora band behind a section
//	.fx-stagger        entry cascade on direct children (90ms steps)
//	.fx-tilt           hover lift + tilt on cards (interaction, not perpetual)
func writeShowcaseFX(b *strings.Builder) {
	b.WriteString("/* --- Showcase fx layer (design.fidelity=showcase) --- */\n\n")

	// Spring-feel timing tokens for anything fx touches.
	b.WriteString(":root { --fx-ease-spring: cubic-bezier(0.34, 1.56, 0.64, 1); --fx-ease-soft: cubic-bezier(0.16, 1, 0.3, 1); }\n\n")

	// Scroll-driven one-shot reveals. linear timing is correct here:
	// the scroll position IS the clock, easing lives in the keyframes.
	b.WriteString("@supports (animation-timeline: view()) {\n")
	b.WriteString("  @media (prefers-reduced-motion: no-preference) {\n")
	b.WriteString("    @keyframes fx-rise { from { opacity: 0; transform: translateY(28px); } to { opacity: 1; transform: translateY(0); } }\n")
	b.WriteString("    @keyframes fx-scale-in { from { opacity: 0; transform: scale(0.94); } to { opacity: 1; transform: scale(1); } }\n")
	b.WriteString("    @keyframes fx-drift-slow { from { transform: translateY(4%); } to { transform: translateY(-4%); } }\n")
	b.WriteString("    @keyframes fx-drift-fast { from { transform: translateY(9%); } to { transform: translateY(-9%); } }\n")
	b.WriteString("    .fx-reveal { animation: fx-rise linear both; animation-timeline: view(); animation-range: entry 0% cover 30%; }\n")
	b.WriteString("    .fx-reveal-scale { animation: fx-scale-in linear both; animation-timeline: view(); animation-range: entry 0% cover 35%; }\n")
	b.WriteString("    .fx-parallax-slow { animation: fx-drift-slow linear both; animation-timeline: view(); animation-range: cover 0% cover 100%; }\n")
	b.WriteString("    .fx-parallax-fast { animation: fx-drift-fast linear both; animation-timeline: view(); animation-range: cover 0% cover 100%; }\n")
	b.WriteString("  }\n")
	b.WriteString("}\n\n")

	// Entry cascade: children rise in sequence on first paint. One-shot
	// (not scroll-linked) so it also choreographs above-the-fold content.
	b.WriteString("@keyframes fx-stagger-rise { from { opacity: 0; transform: translateY(18px); } to { opacity: 1; transform: translateY(0); } }\n")
	b.WriteString(".fx-stagger > * { animation: fx-stagger-rise 0.7s var(--fx-ease-soft) both; }\n")
	for i := 1; i <= 8; i++ {
		b.WriteString(fmt.Sprintf(".fx-stagger > *:nth-child(%d) { animation-delay: %dms; }\n", i, (i-1)*90))
	}
	b.WriteString(".fx-stagger > *:nth-child(n+9) { animation-delay: 720ms; }\n\n")

	// Perpetual ambient effects. Each is ONE motion-budget signal.
	b.WriteString("@keyframes fx-float-bob { 0% { transform: translateY(0); } 100% { transform: translateY(-10px); } }\n")
	b.WriteString(".fx-float { animation: fx-float-bob 6s ease-in-out infinite alternate; }\n")
	b.WriteString("@keyframes fx-shimmer-sweep { 0% { background-position: -180% 0; } 100% { background-position: 280% 0; } }\n")
	b.WriteString(".fx-shimmer { background-image: linear-gradient(105deg, transparent 42%, color-mix(in oklab, var(--color-bg) 55%, transparent) 50%, transparent 58%); background-size: 220% 100%; background-repeat: no-repeat; animation: fx-shimmer-sweep 3.4s var(--fx-ease-soft) infinite; }\n")
	b.WriteString(".fx-aurora-bg { position: relative; overflow: hidden; }\n")
	b.WriteString(".fx-aurora-bg::before { content: ''; position: absolute; inset: -12%; pointer-events: none; z-index: 0; background: radial-gradient(80% 60% at 20% 25%, color-mix(in oklab, var(--color-primary) 30%, transparent), transparent 62%), radial-gradient(70% 55% at 80% 60%, color-mix(in oklab, var(--color-primary) 16%, var(--color-text) 5%), transparent 65%); animation: heroAuroraDrift 28s ease-in-out infinite alternate; }\n")
	b.WriteString(".fx-aurora-bg > * { position: relative; z-index: 1; }\n\n")

	// Interaction: card lift + tilt. Fires on hover/focus only, so it
	// does not count against the perpetual budget.
	b.WriteString(".fx-tilt { transition: transform 0.35s var(--fx-ease-spring), box-shadow 0.35s var(--fx-ease-soft); will-change: transform; }\n")
	b.WriteString(".fx-tilt:hover, .fx-tilt:focus-within { transform: translateY(-6px) rotate3d(1, -1, 0, 2deg); box-shadow: 0 24px 64px color-mix(in oklab, var(--color-text) 16%, transparent); }\n\n")

	// Static gradient text: brand-derived two-stop fill. No motion cost.
	b.WriteString(".fx-gradient-text { background: linear-gradient(100deg, var(--color-primary), color-mix(in oklab, var(--color-primary) 45%, var(--color-text))); -webkit-background-clip: text; background-clip: text; color: transparent; }\n\n")

	// The a11y invariant: one gate kills every fx motion. Gradient text
	// and tilt shadows survive (no motion), everything animated stops.
	b.WriteString("@media (prefers-reduced-motion: reduce) {\n")
	b.WriteString("  .fx-reveal, .fx-reveal-scale, .fx-parallax-slow, .fx-parallax-fast, .fx-stagger > *, .fx-float, .fx-shimmer, .fx-aurora-bg::before { animation: none; }\n")
	b.WriteString("  .fx-tilt, .fx-tilt:hover, .fx-tilt:focus-within { transition: none; transform: none; }\n")
	b.WriteString("}\n\n")
}
