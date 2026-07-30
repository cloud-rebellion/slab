package builder

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bright-interaction/slab/internal/agent"
	"github.com/bright-interaction/slab/internal/store"
)

// RenderLayouts generates Astro layout files from site settings and global blocks.
func RenderLayouts(ctx context.Context, queries *store.Queries, siteID string, wsDir string) error {
	site, err := queries.GetSiteByID(ctx, siteID)
	if err != nil {
		return fmt.Errorf("get site: %w", err)
	}

	globalBlocks, _ := queries.ListActiveGlobalBlocksBySite(ctx, siteID)
	settings, _ := queries.ListSettingsBySite(ctx, siteID)
	profile, _ := queries.GetSiteProfile(ctx, siteID)

	// Build settings map for quick lookup
	settingsMap := make(map[string]string)
	for _, s := range settings {
		settingsMap[s.Category+"."+s.Key] = s.Value
	}

	// Find header and footer global blocks
	var headerHTML, footerHTML string
	for _, gb := range globalBlocks {
		html := extractHTMLFromGlobalBlock(gb)
		switch gb.Slot {
		case "header":
			headerHTML = html
		case "footer":
			footerHTML = html
		}
	}

	// Build the layout
	showcase := agent.FidelityFromSettings(settingsMap) == agent.FidelityShowcase
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("import '../styles/global.css';\n")
	if showcase {
		// Showcase fidelity ships Astro view transitions: cross-page
		// navigation morphs instead of flashing. Astro's ClientRouter
		// respects prefers-reduced-motion out of the box (falls back to
		// an instant swap). Balanced/performance layouts stay JS-free.
		b.WriteString("import { ClientRouter } from 'astro:transitions';\n")
	}
	b.WriteString("\n")
	b.WriteString("interface Props {\n")
	b.WriteString("  title: string;\n")
	b.WriteString("  description?: string;\n")
	b.WriteString("  ogImage?: string;\n")
	b.WriteString("  robots?: string;\n")
	b.WriteString("  hideGlobalBlocks?: boolean;\n")
	b.WriteString("  alternates?: Array<{ lang: string; url: string }>;\n")
	b.WriteString("  lang?: string;\n")
	b.WriteString("}\n\n")
	b.WriteString("const {\n")
	b.WriteString(fmt.Sprintf("  title = '%s',\n", escapeAstroString(site.MetaTitle)))
	b.WriteString(fmt.Sprintf("  description = '%s',\n", escapeAstroString(site.MetaDescription)))
	b.WriteString(fmt.Sprintf("  lang = '%s',\n", escapeAstroString(site.Lang)))
	// Default OG image. Resolution order:
	//   1. seo.og_default_image_id (Settings -> SEO -> Default Open Graph image)
	//   2. sites.og_image_id (legacy column written by the wizard)
	// Either way, we resolve to a full URL like https://example.com/media/<id>/original.jpg
	// so external scrapers (Open Graph + Twitter Card) get a usable absolute href.
	// Per-page <Base ogImage="..."> overrides are passed at render time by pages.go.
	b.WriteString(fmt.Sprintf("  ogImage = '%s',\n", resolveDefaultOgImageURL(ctx, queries, site, settingsMap, domainForOgImage(site))))
	b.WriteString("  robots = 'index, follow, max-snippet:-1, max-image-preview:large, max-video-preview:-1',\n")
	b.WriteString("  hideGlobalBlocks = false,\n")
	b.WriteString("  alternates = [],\n")
	b.WriteString("} = Astro.props;\n")

	// Compute canonical
	domain := site.Domain
	if domain == "" {
		domain = "localhost"
	}
	if !strings.HasPrefix(domain, "http") {
		domain = "https://" + domain
	}
	b.WriteString(fmt.Sprintf("\nconst canonicalURL = new URL(Astro.url.pathname, '%s');\n", domain))
	b.WriteString("const canonicalOrigin = canonicalURL.origin;\n")
	// Theme color: the site's primary brand fill. Used by Android Chrome
	// for the address bar tint. Falls back to a neutral near-black so the
	// meta tag always carries a value.
	primaryHex := strings.TrimSpace(site.PrimaryColor)
	if primaryHex == "" {
		primaryHex = "#111111"
	}
	b.WriteString(fmt.Sprintf("const themeColor = '%s';\n", primaryHex))
	b.WriteString("---\n\n")

	// HTML. Per-page lang prop overrides site default so multi-language
	// sites stamp /en/* with lang="en" and /sv/* with lang="sv". The
	// site default lang is the fallback for pages whose locale can't be
	// derived from the slug. CSS :lang() pseudo + locale-aware nav hide
	// rules rely on this being correct.
	b.WriteString("<!DOCTYPE html>\n<html lang={lang}>\n<head>\n")
	b.WriteString("  <meta charset=\"UTF-8\" />\n")
	b.WriteString("  <meta name=\"viewport\" content=\"width=device-width, initial-scale=1.0\" />\n")
	if showcase {
		b.WriteString("  <ClientRouter />\n")
	}
	b.WriteString("  <title>{title}</title>\n")
	b.WriteString("  {description && <meta name=\"description\" content={description} />}\n")
	b.WriteString("  <meta name=\"robots\" content={robots} />\n")
	b.WriteString("  <link rel=\"canonical\" href={canonicalURL.href} />\n")
	// Hreflang alternates. Per-page array passed in by renderPage; mono-
	// language sites receive [] and the JSX expression renders nothing.
	// Each entry maps to a <link rel="alternate" hreflang="X" href="Y" />.
	// Site Inspector's seo checks (Hreflang Tags + Self-Referencing +
	// x-default) cover this output.
	b.WriteString("  {alternates && alternates.map((a) => (\n")
	b.WriteString("    <link rel=\"alternate\" hreflang={a.lang} href={a.url} />\n")
	b.WriteString("  ))}\n")
	// Favicon + Apple Touch Icon for site-inspector parity. Emitted unconditionally;
	// browsers degrade silently if /favicon.ico or /apple-touch-icon.png is missing.
	// The Media library's `folder=brand` is the intended home for these files.
	b.WriteString("  <link rel=\"icon\" type=\"image/x-icon\" href=\"/favicon.ico\" />\n")
	b.WriteString("  <link rel=\"apple-touch-icon\" href=\"/apple-touch-icon.png\" />\n")
	// Theme color drives the Android Chrome address-bar tint and macOS
	// Safari standalone-mode chrome. Pulls from the site's primary brand
	// color injected as a JSX expression below; falls back to a neutral
	// near-black so the meta tag is always present (Site Inspector's
	// "Theme Color Meta" check passes on any non-empty value).
	b.WriteString("  <meta name=\"theme-color\" content={themeColor} />\n")
	// Resource hint: dns-prefetch + preconnect to the canonical origin.
	// Mostly a no-op for fully same-origin builds (the browser already
	// connected to fetch the HTML), but Site Inspector's "Resource Hints"
	// check expects at least one rel="preconnect" or rel="dns-prefetch"
	// link tag in <head>. Adding self-preconnect is harmless and lets the
	// check pass even on tenants who run zero third-party connections.
	b.WriteString("  <link rel=\"dns-prefetch\" href={canonicalOrigin} />\n")
	b.WriteString("  <link rel=\"preconnect\" href={canonicalOrigin} crossorigin />\n\n")

	// Fonts: self-hosted woff2 only. NEVER Google Fonts (leaks visitor IP
	// to Google, fails Subresource Integrity eval, clashes with the
	// EU-sovereignty positioning many atomicsite tenants ship).
	// Per-site fonts are uploaded via POST /api/sites/{id}/fonts and
	// served from /atomicsite-fonts/{siteID}/{fontID}.woff2; the @font-face
	// block below renders one rule per uploaded font. When no fonts are
	// uploaded the site falls back to system-ui through the --font-heading
	// / --font-body CSS custom properties (still readable, just system-
	// styled).

	// Open Graph
	b.WriteString(fmt.Sprintf("  <meta property=\"og:site_name\" content=\"%s\" />\n", escapeAttr(site.Name)))
	b.WriteString(fmt.Sprintf("  <meta property=\"og:locale\" content=\"%s\" />\n", escapeAttr(ogLocaleFromLang(site.Lang))))
	b.WriteString("  <meta property=\"og:title\" content={title} />\n")
	b.WriteString("  {description && <meta property=\"og:description\" content={description} />}\n")
	b.WriteString("  {ogImage && <meta property=\"og:image\" content={ogImage} />}\n")
	b.WriteString("  {ogImage && <meta property=\"og:image:width\" content=\"1200\" />}\n")
	b.WriteString("  {ogImage && <meta property=\"og:image:height\" content=\"630\" />}\n")
	b.WriteString("  <meta property=\"og:type\" content=\"website\" />\n")
	b.WriteString("  <meta property=\"og:url\" content={canonicalURL.href} />\n")
	// Twitter Card. summary_large_image renders the OG image at full bleed in
	// X / Twitter previews; site-inspector requires twitter:card and
	// twitter:image. We piggyback on the ogImage prop so they stay in sync.
	b.WriteString("  <meta name=\"twitter:card\" content=\"summary_large_image\" />\n")
	b.WriteString("  {ogImage && <meta name=\"twitter:image\" content={ogImage} />}\n")
	b.WriteString("  <meta name=\"twitter:title\" content={title} />\n")
	b.WriteString("  {description && <meta name=\"twitter:description\" content={description} />}\n\n")

	// Security headers as meta (belt-and-suspenders: headers also sent via
	// _headers file and nginx.conf for directives browsers honor only in
	// headers, e.g. frame-ancestors, HSTS, X-Frame-Options).
	if headers, err := BuildSecurityHeaders(ctx, queries, siteID); err == nil {
		if headers.ReferrerPolicy != "" {
			b.WriteString(fmt.Sprintf("  <meta name=\"referrer\" content=\"%s\" />\n", escapeAttr(headers.ReferrerPolicy)))
		}
		// Use meta-safe CSP subset -- frame-ancestors etc. only work in headers.
		if metaCSP := CSPForMeta(headers.CSP); metaCSP != "" {
			b.WriteString(fmt.Sprintf("  <meta http-equiv=\"Content-Security-Policy\" content=\"%s\" />\n", escapeAttr(metaCSP)))
		}
	}

	// JSON-LD Organization schema for AI search / knowledge graph entity anchoring.
	// Emitted whenever the site profile has a business name. sameAs array comes
	// from the seo.same_as setting (newline-separated URLs); logo URL comes from
	// seo.logo_url setting (falls back to OG image if absent so the schema is
	// still present, just without a dedicated logo field).
	if orgJSON := buildOrganizationJSONLD(site, profile, settingsMap); orgJSON != "" {
		b.WriteString("  <script type=\"application/ld+json\">")
		b.WriteString(orgJSON)
		b.WriteString("</script>\n")
	}

	// Analytics. Resolution lives in resolveAnalyticsConfig: the
	// analytics-category settings rows take precedence over the legacy
	// sites-table columns, falling back to those columns when the row is
	// empty (so existing wizard-seeded sites keep working). Both Umami and
	// GA4 emit only when the operator explicitly opts in.
	analytics := resolveAnalyticsConfig(site, settingsMap)
	if analytics.UmamiEnabled && analytics.UmamiURL != "" && analytics.UmamiSiteID != "" {
		// is:inline keeps Astro from rewriting the tag into a module
		// (which would defer it past CookieProof's expectations).
		// escapeAttr on BOTH, like the GA4 sink twelve lines down. These two
		// were the only unescaped values in the head, and `analytics` is an
		// agent-writable settings category, so an agent could store
		// `https://x/a"></script><script>...` as umami_url and land a real
		// inline script in the <head> of every published page.
		b.WriteString(fmt.Sprintf("  <script is:inline defer src=\"%s/script.js\" data-website-id=\"%s\"></script>\n",
			escapeAttr(analytics.UmamiURL), escapeAttr(analytics.UmamiSiteID)))
	}
	if analytics.GA4Enabled && analytics.GA4ID != "" {
		// gtag.js + GA4 config. Emitted as an honest static snippet (no consent
		// gate built in here): when analytics.cookieproof_enabled=1 the
		// CookieProof relay below this block delays gtag config until the
		// visitor consents to analytics. When CookieProof is off, this fires
		// on every visit (legitimate-interest aligned with the always-on log
		// tail). Operators who want hard consent gating should keep
		// CookieProof on; that is the documented setup.
		b.WriteString(fmt.Sprintf("  <script is:inline async src=\"https://www.googletagmanager.com/gtag/js?id=%s\"></script>\n", escapeAttr(analytics.GA4ID)))
		b.WriteString("  <script is:inline>\n")
		b.WriteString("    window.dataLayer = window.dataLayer || [];\n")
		b.WriteString("    function gtag(){dataLayer.push(arguments);}\n")
		b.WriteString("    gtag('js', new Date());\n")
		b.WriteString(fmt.Sprintf("    gtag('config', '%s', { anonymize_ip: true });\n", escapeAttr(analytics.GA4ID)))
		b.WriteString("  </script>\n")
	}

	// CookieProof banner .  vendored, same-origin, inline-configured. Gated
	// on analytics.cookieproof_enabled. The snippet emits a synchronous GCM
	// default-denied stub, an inline window.__CCB__ config blob (branding
	// colors flow in as cssVars, banner copy from settings), and a deferred
	// <script> tag pointing at /_ccb.{hash}.js .  the widget bundle we
	// wrote into the workspace's public/ dir from the embedded widget bytes.
	// No third-party fetch. Proofs POST to same-origin /t/consent and land
	// in atomicsite's consent_records table.
	cookieProofEnabled := boolSetting(settingsMap["analytics.cookieproof_enabled"], false)
	if cookieProofEnabled {
		cpCfg := BuildCookieProofConfig(site, settingsMap)
		snippet := RenderCookieProofSnippet(cpCfg)
		// Indent two spaces so the snippet sits flush with surrounding <head> children.
		for _, line := range strings.Split(strings.TrimRight(snippet, "\n"), "\n") {
			if line == "" {
				b.WriteString("\n")
				continue
			}
			b.WriteString("  " + line + "\n")
		}
		// Drop the same-origin widget bundle into public/. Failure here is
		// fatal .  the snippet references the file, so a missing asset would
		// 404 in production and the banner would never render.
		if err := WriteCookieProofWidgetAsset(wsDir, cpCfg); err != nil {
			return fmt.Errorf("write cookieproof widget asset: %w", err)
		}
		if showcase {
			// Showcase ships Astro's ClientRouter: soft navigations swap
			// the <body>, wiping the widget-injected <cookie-consent>
			// element, and the widget bundle never re-executes (module +
			// run-once guard), so an unconsented visitor would
			// permanently lose the consent UI after the first internal
			// click. Consent is a fidelity invariant: capture the live
			// element before each swap and re-append the SAME instance
			// after (custom-element state and the consent relay survive
			// re-parenting). No-op everywhere the router is absent.
			b.WriteString("  <script>\n")
			b.WriteString("    (function () {\n")
			b.WriteString("      var kept = null;\n")
			b.WriteString("      document.addEventListener('astro:before-swap', function () {\n")
			b.WriteString("        kept = document.querySelector('cookie-consent') || kept;\n")
			b.WriteString("      });\n")
			b.WriteString("      document.addEventListener('astro:after-swap', function () {\n")
			b.WriteString("        if (kept && !kept.isConnected && document.body && !document.querySelector('cookie-consent')) {\n")
			b.WriteString("          document.body.appendChild(kept);\n")
			b.WriteString("        }\n")
			b.WriteString("      });\n")
			b.WriteString("    })();\n")
			b.WriteString("  </script>\n")
		}
	}

	// Bring-your-own consent banner. Whatever HTML/JS the user paste into
	// analytics.cookie_banner_snippet is emitted verbatim into <head>. We do
	// not validate or transform it; this is the "I run Cookiebot/OneTrust/
	// Termly/Iubenda/whatever" escape hatch. Trim whitespace so an empty
	// textarea doesn't add a blank script tag.
	if customBanner := strings.TrimSpace(settingsMap["analytics.cookie_banner_snippet"]); customBanner != "" {
		for _, line := range strings.Split(customBanner, "\n") {
			b.WriteString("  " + line + "\n")
		}
	}

	// @font-face emissions for any site_fonts uploaded via /api/sites/{id}/fonts.
	// Self-hosted woff2 only; served by /atomicsite-fonts/{site}/{id}.woff2.
	// font-display:swap so headers don't render invisibly while the file
	// downloads.
	if fontRows, err := queries.ListSiteFonts(ctx, siteID); err == nil && len(fontRows) > 0 {
		b.WriteString("  <style>\n")
		for _, f := range fontRows {
			src := fmt.Sprintf("/atomicsite-fonts/%s/%s.woff2", siteID, f.ID)
			rangeDecl := UnicodeRangeFor(f.Subset)
			if rangeDecl != "" {
				b.WriteString(fmt.Sprintf(
					"    @font-face { font-family: %q; font-style: %s; font-weight: %d; font-display: swap; src: url(%q) format('woff2'); unicode-range: %s; }\n",
					f.FamilyName, f.Style, f.Weight, src, rangeDecl,
				))
			} else {
				b.WriteString(fmt.Sprintf(
					"    @font-face { font-family: %q; font-style: %s; font-weight: %d; font-display: swap; src: url(%q) format('woff2'); }\n",
					f.FamilyName, f.Style, f.Weight, src,
				))
			}
			b.WriteString(fmt.Sprintf(
				"    /* preload-hint: %q subset=%s */\n",
				f.FamilyName, fontSubsetOrAll(f.Subset),
			))
		}
		b.WriteString("  </style>\n")
		// Emit one <link rel="preload"> per uploaded font so above-the-fold
		// renders don't FOUT. The browser still respects unicode-range:
		// the preload fetches the file but it only renders if the page
		// contains characters in the declared range. For a Latin-only
		// site that's still a perf win because the file lands in cache.
		for _, f := range fontRows {
			src := fmt.Sprintf("/atomicsite-fonts/%s/%s.woff2", siteID, f.ID)
			b.WriteString(fmt.Sprintf(
				"  <link rel=\"preload\" as=\"font\" type=\"font/woff2\" crossorigin=\"anonymous\" href=%q />\n",
				src,
			))
		}
	}

	b.WriteString("</head>\n<body>\n")

	// Skip-to-content link for keyboard users. Site-inspector's accessibility
	// pass wants the FIRST focusable anchor on every page to jump to a
	// landmark. The .sr-only-focusable class hides it offscreen until focus.
	b.WriteString("  <a href=\"#main\" class=\"sr-only-focusable\" style=\"position:absolute;left:-9999px;top:auto;\" onfocus=\"this.style.left='8px';this.style.top='8px';this.style.background='#000';this.style.color='#fff';this.style.padding='8px 12px';this.style.zIndex='10000';\" onblur=\"this.style.left='-9999px';this.style.top='auto';\">Skip to content</a>\n")

	// Header. Gated on `hideGlobalBlocks` so per-page landing routes can
	// drop the site-wide nav for higher conversion. Astro reads
	// `hideGlobalBlocks` as a boolean prop, defaults false.
	if headerHTML != "" {
		b.WriteString("  {!hideGlobalBlocks && (\n")
		b.WriteString("    <header>\n")
		b.WriteString("      " + headerHTML + "\n")
		b.WriteString("    </header>\n")
		b.WriteString("  )}\n")
	}

	b.WriteString("  <main id=\"main\">\n    <slot />\n  </main>\n")

	// Footer. Same per-page suppression as the header.
	if footerHTML != "" {
		b.WriteString("  {!hideGlobalBlocks && (\n")
		b.WriteString("    <footer>\n")
		b.WriteString("      " + footerHTML + "\n")
		b.WriteString("    </footer>\n")
		b.WriteString("  )}\n")
	}

	// Engagement beacon (Phase 12.6). Captures the JS-only metrics server
	// log tail can't see: screen + viewport, prefers-color-scheme, prefers-
	// reduced-motion, time on page, max scroll depth. Consent-gated when
	// CookieProof is wired up (waits for `consent:init` with analytics=true);
	// fires always when there's no consent banner (same legitimate-interest
	// posture as the always-on nginx log tail).
	if boolSetting(settingsMap["analytics.engagement_enabled"], true) {
		trackPath := orDefault(settingsMap["analytics.track_path"], "/t")
		// Write the per-site asset so the script tag below has a file
		// to load. CSP `script-src 'self'` rejects the inline form.
		if err := WriteEngagementBeaconAsset(wsDir, site.ID, trackPath, cookieProofEnabled); err != nil {
			return fmt.Errorf("write engagement beacon asset: %w", err)
		}
		b.WriteString(RenderEngagementBeacon(site.ID, trackPath, cookieProofEnabled))
	}

	// Bidirectional CRM personalization (Phase 18.2). Hydration script
	// fetches /t/visitor cross-origin, evaluates [data-asp-when] DSL,
	// reveals matching blocks. Gated on analytics.personalization_enabled
	// so the cost is opt-in.
	if boolSetting(settingsMap["analytics.personalization_enabled"], false) {
		trackPath := orDefault(settingsMap["analytics.track_path"], "/t")
		adminBase := orDefault(settingsMap["analytics.admin_base_url"], DefaultAdminBaseURL)
		// Same CSP-driven externalization pattern as the engagement
		// beacon: write the per-site script to public/, reference via
		// same-origin <script src>.
		if err := WriteVisitorHydrationAsset(wsDir, site.ID, adminBase, trackPath); err != nil {
			return fmt.Errorf("write visitor hydration asset: %w", err)
		}
		b.WriteString(RenderVisitorHydration(site.ID, adminBase, trackPath))
	}

	// Sprint 2 slice C (2026-05-22): per-site storefront island. Wires
	// add-to-cart, the cart drawer, and the checkout-form submit to the
	// /api/sites/{siteID}/checkout endpoint. Always emitted so any page
	// can carry a [data-cart-add] button without an extra setting flip;
	// the script is inert on pages with no matching attributes.
	if err := WriteStorefrontIslandAsset(wsDir, site.ID); err != nil {
		return fmt.Errorf("write storefront island asset: %w", err)
	}
	b.WriteString(RenderStorefrontIslandTag())

	// Sprint 5 quick-win (2026-05-24): Core Web Vitals beacon. Opt-in
	// via analytics.cwv_enabled so tenants who don't want the field
	// data don't pay the per-pageview beacon cost. CookieProof consent
	// gating mirrors the engagement beacon: when the consent widget is
	// wired up, wait for analytics=true; otherwise report under
	// legitimate interest from first paint.
	if boolSetting(settingsMap["analytics.cwv_enabled"], false) {
		trackPath := orDefault(settingsMap["analytics.track_path"], "/t")
		if err := WriteCWVBeaconAsset(wsDir, site.ID, trackPath, cookieProofEnabled); err != nil {
			return fmt.Errorf("write cwv beacon asset: %w", err)
		}
		b.WriteString(RenderCWVBeacon())
	}

	b.WriteString("</body>\n</html>\n")

	return WriteFile(filepath.Join(wsDir, "src", "layouts", "Base.astro"), b.String())
}

// EngagementBeaconScript returns the JS body that records JS-only
// engagement metrics + posts via navigator.sendBeacon. Pure vanilla
// JS, no deps. Site ID + track path are baked in at build time. The
// content is written to a per-site asset by WriteEngagementBeaconAsset
// so the page can reference it via `<script src>` instead of inline
// (which CSP `script-src 'self'` blocks without a hash).
func EngagementBeaconScript(siteID, trackPath string, consentGated bool) string {
	defaultAllowed := "true"
	if consentGated {
		defaultAllowed = "false"
	}
	return fmt.Sprintf(`(function(){
  var SITE_ID=%q;var TRACK_PATH=%q;var ALLOWED=%s;
  var t0=performance.now();var maxScroll=0;
  function pct(){var d=document.documentElement;var sh=Math.max(d.scrollHeight,d.clientHeight);if(sh<=0)return 0;var v=window.scrollY+d.clientHeight;var p=Math.round(v*100/sh);return p>100?100:(p<0?0:p);}
  function tick(){var p=pct();if(p>maxScroll)maxScroll=p;}
  window.addEventListener("scroll",tick,{passive:true});tick();
  function send(){if(!ALLOWED)return;
    var dark=false,rm=false;try{dark=matchMedia("(prefers-color-scheme: dark)").matches;rm=matchMedia("(prefers-reduced-motion: reduce)").matches;}catch(e){}
    var p={siteId:SITE_ID,path:location.pathname+location.search,screenW:screen.width|0,screenH:screen.height|0,viewportW:innerWidth|0,viewportH:innerHeight|0,prefersDark:dark,prefersReducedMotion:rm,timeOnPageMs:Math.round(performance.now()-t0),maxScrollPct:maxScroll};
    try{var b=new Blob([JSON.stringify(p)],{type:"application/json"});navigator.sendBeacon(TRACK_PATH+"/engagement",b);}catch(e){}
  }
  // window.atomic.track(name, props) records an explicit conversion
  // event. Phase 31.1, 2026-05-06. Same consent gate as the
  // engagement beacon - if the visitor has not granted analytics
  // consent, the call is a quiet no-op so site code can fire it
  // unconditionally without polluting opt-out users.
  function track(name,props){if(!ALLOWED||!name)return;
    var p={siteId:SITE_ID,path:location.pathname+location.search,name:String(name).slice(0,128),valueCents:0,properties:null};
    if(props&&typeof props==="object"){if(typeof props.valueCents==="number"){p.valueCents=props.valueCents|0;}p.properties=props.properties||props;}
    try{var b=new Blob([JSON.stringify(p)],{type:"application/json"});navigator.sendBeacon(TRACK_PATH+"/event",b);}catch(e){}
  }
  window.atomic=window.atomic||{};window.atomic.track=track;
  document.addEventListener("consent:init",function(e){if(e&&e.detail&&e.detail.analytics)ALLOWED=true;});
  document.addEventListener("visibilitychange",function(){if(document.visibilityState==="hidden")send();});
  window.addEventListener("pagehide",send);
})();
`, siteID, trackPath, defaultAllowed)
}

// EngagementBeaconAssetName is the filename written to public/. Stable
// across builds so cache-busting comes from query string when needed
// (the script is small + per-site).
const EngagementBeaconAssetName = "_atomic-engagement.js"

// WriteEngagementBeaconAsset writes the per-site engagement-beacon JS
// file to the workspace's public/ dir. Same-origin loadable, CSP
// `script-src 'self'` allows it, no inline-hash gymnastics.
func WriteEngagementBeaconAsset(workspaceDir, siteID, trackPath string, consentGated bool) error {
	publicDir := filepath.Join(workspaceDir, "public")
	if err := EnsureDir(publicDir); err != nil {
		return fmt.Errorf("ensure public dir: %w", err)
	}
	target := filepath.Join(publicDir, EngagementBeaconAssetName)
	return WriteFile(target, EngagementBeaconScript(siteID, trackPath, consentGated))
}

// RenderEngagementBeacon returns the <script> tag that loads the
// per-site engagement asset. The asset itself is written by
// WriteEngagementBeaconAsset during the layout build pass.
func RenderEngagementBeacon(siteID, trackPath string, consentGated bool) string {
	return fmt.Sprintf(`  <script is:inline defer src="/%s"></script>
`, EngagementBeaconAssetName)
}

// buildOrganizationJSONLD emits a schema.org Organization JSON-LD blob from
// site profile + settings. Returns "" if there's not enough data
// (no business name), callers should skip emission in that case.
//
// Settings consumed:
//
//	seo.same_as   newline-separated URLs (LinkedIn, GitHub, etc.)
//	seo.logo_url  absolute URL to the org logo (falls back to og:image)
func buildOrganizationJSONLD(site store.Site, profile store.SiteProfile, settings map[string]string) string {
	name := strings.TrimSpace(profile.BusinessName)
	if name == "" {
		name = strings.TrimSpace(site.Name)
	}
	if name == "" {
		return ""
	}

	domain := strings.TrimSpace(site.Domain)
	if domain == "" {
		return "" // no canonical URL means no useful schema
	}
	if !strings.HasPrefix(domain, "http") {
		domain = "https://" + domain
	}

	logo := strings.TrimSpace(settings["seo.logo_url"])
	if logo == "" && site.OgImageID != "" {
		logo = strings.TrimRight(domain, "/") + "/media/" + site.OgImageID
	}

	sameAs := []string{}
	for _, line := range strings.Split(settings["seo.same_as"], "\n") {
		if v := strings.TrimSpace(line); v != "" {
			sameAs = append(sameAs, v)
		}
	}

	org := map[string]any{
		"@context": "https://schema.org",
		"@type":    "Organization",
		"@id":      strings.TrimRight(domain, "/") + "/#organization",
		"name":     name,
		"url":      domain,
	}
	if logo != "" {
		org["logo"] = logo
	}
	if len(sameAs) > 0 {
		org["sameAs"] = sameAs
	}

	if profile.AddressLine1 != "" || profile.City != "" || profile.PostalCode != "" || profile.Country != "" {
		addr := map[string]any{"@type": "PostalAddress"}
		if profile.AddressLine1 != "" {
			addr["streetAddress"] = profile.AddressLine1
		}
		if profile.City != "" {
			addr["addressLocality"] = profile.City
		}
		if profile.PostalCode != "" {
			addr["postalCode"] = profile.PostalCode
		}
		if profile.Country != "" {
			addr["addressCountry"] = profile.Country
		}
		org["address"] = addr
	}

	if profile.ContactEmail != "" || profile.ContactPhone != "" {
		cp := map[string]any{
			"@type":       "ContactPoint",
			"contactType": "customer service",
		}
		if profile.ContactEmail != "" {
			cp["email"] = profile.ContactEmail
		}
		if profile.ContactPhone != "" {
			cp["telephone"] = profile.ContactPhone
		}
		org["contactPoint"] = cp
	}

	out, err := json.Marshal(org)
	if err != nil {
		return ""
	}
	return string(out)
}

// ExtractHTMLFromGlobalBlock is the exported wrapper for external callers. around the package-
// internal renderer. Callers outside builder (the admin Preview endpoint,
// the agent) use this to render a single block without going through the
// full layout pipeline. Same logic the builder runs at deploy time so
// preview HTML is byte-for-byte identical to what lands in production.
func ExtractHTMLFromGlobalBlock(gb store.GlobalBlock) string {
	return extractHTMLFromGlobalBlock(gb)
}

func extractHTMLFromGlobalBlock(gb store.GlobalBlock) string {
	var data map[string]any
	if err := json.Unmarshal([]byte(gb.DataJson), &data); err != nil {
		return ""
	}
	// Raw HTML override: if the block stores `data.html`, use that verbatim.
	if html, ok := data["html"].(string); ok && html != "" {
		return html
	}

	// Structured data path. Render the canonical header/footer shapes so the
	// deployed site has a real <header><nav> and <footer> instead of empty
	// strings.
	switch gb.Slot {
	case "header":
		return renderHeaderHTML(data)
	case "footer":
		return renderFooterHTML(data)
	}
	return ""
}

func renderHeaderHTML(data map[string]any) string {
	links := extractNavLinks(data["links"])
	brand, _ := data["brand"].(map[string]any)
	if len(links) == 0 && brand == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<header class="site-header"><div class="container">`)

	// Optional brand mark: image + label, image + label, or just label.
	// When data.brand is set, render it pulled to the left; the nav links
	// flow to the right via the existing CSS justify-content: space-between.
	if brand != nil {
		href, _ := brand["href"].(string)
		if href == "" {
			href = "/"
		}
		label, _ := brand["label"].(string)
		imageID, _ := brand["image_id"].(string)
		badge, _ := brand["badge"].(string)
		wordmark, _ := brand["wordmark"].(string)
		accentFrom, _ := brand["accent_from"].(string)
		b.WriteString(`<a class="brand-mark" href="`)
		b.WriteString(escapeAttr(href))
		b.WriteString(`" aria-label="`)
		if label != "" {
			b.WriteString(escapeAttr(label))
		} else if wordmark != "" {
			b.WriteString(escapeAttr(wordmark))
		} else {
			b.WriteString("Home")
		}
		b.WriteString(`">`)
		// Badge: a short text mark (typically 1-2 letters) shown in a coloured
		// rounded square. Universal brand-mark pattern used by BI, Linear,
		// Vercel, Stripe, etc. Falls back to image_id when no badge is set.
		if badge != "" {
			b.WriteString(`<span class="brand-badge" aria-hidden="true">`)
			b.WriteString(escapeText(badge))
			b.WriteString(`</span>`)
		} else if imageID != "" {
			b.WriteString(`<img src="/media/`)
			b.WriteString(escapeAttr(imageID))
			b.WriteString(`/original.png" alt="`)
			b.WriteString(escapeAttr(label))
			b.WriteString(`" />`)
		}
		// Wordmark: optional text wordmark next to the badge. accent_from
		// flags the substring that should render in --color-primary.
		// Example: wordmark="brightinteraction" + accent_from="interaction"
		// → "bright<span class=accent>interaction</span>".
		if wordmark != "" {
			b.WriteString(`<span class="brand-wordmark">`)
			if accentFrom != "" && strings.Contains(wordmark, accentFrom) {
				idx := strings.Index(wordmark, accentFrom)
				b.WriteString(escapeText(wordmark[:idx]))
				b.WriteString(`<span class="brand-wordmark-accent">`)
				b.WriteString(escapeText(wordmark[idx:]))
				b.WriteString(`</span>`)
			} else {
				b.WriteString(escapeText(wordmark))
			}
			b.WriteString(`</span>`)
		} else if label != "" {
			b.WriteString(`<span class="brand-wordmark">`)
			b.WriteString(escapeText(label))
			b.WriteString(`</span>`)
		}
		b.WriteString(`</a>`)
	}

	// 3-zone header layout (mirrors brightinteraction.com / Linear / Vercel):
	//   [brand-mark]  [primary nav (centred)]  [actions: lang switchers + CTA]
	// Splitting links into primary + actions lets CSS grid place each zone
	// without flex-wrap rescuing overflow on narrow viewports. Locale links
	// (data.lang set) and CTA links (data.cta=true) belong in the right
	// actions cluster; everything else is primary.
	if len(links) > 0 {
		var primary []navLink
		var actions []navLink
		for _, l := range links {
			if l.cta || l.lang != "" {
				actions = append(actions, l)
			} else {
				primary = append(primary, l)
			}
		}
		if len(primary) > 0 {
			b.WriteString(`<nav class="site-nav site-nav-primary" aria-label="Primary"><ul>`)
			for _, l := range primary {
				b.WriteString(`<li><a href="`)
				b.WriteString(escapeAttr(l.href))
				b.WriteString(`">`)
				b.WriteString(escapeText(l.label))
				b.WriteString(`</a></li>`)
			}
			b.WriteString(`</ul></nav>`)
		}
		if len(actions) > 0 {
			b.WriteString(`<div class="site-nav-actions"><ul>`)
			for _, l := range actions {
				liCls := ""
				aCls := ""
				if l.cta {
					liCls = ` class="nav-cta-item"`
					aCls = ` class="nav-cta"`
				}
				liData := ""
				if l.lang != "" {
					liData = fmt.Sprintf(` data-lang="%s"`, escapeAttr(l.lang))
				}
				b.WriteString(`<li` + liCls + liData + `><a href="`)
				b.WriteString(escapeAttr(l.href))
				b.WriteString(`"` + aCls + `>`)
				b.WriteString(escapeText(l.label))
				b.WriteString(`</a></li>`)
			}
			b.WriteString(`</ul></div>`)
		}
	}
	b.WriteString(`</div></header>`)
	return b.String()
}

func renderFooterHTML(data map[string]any) string {
	// Multi-column path: when data.columns is set, render a 4-up footer with
	// optional tagline + social row. Backwards-compatible single-row path
	// triggered by data.links (the legacy shape) below.
	if columnsRaw, ok := data["columns"].([]any); ok && len(columnsRaw) > 0 {
		return renderFooterColumnsHTML(data, columnsRaw)
	}
	links := extractNavLinks(data["links"])
	copyright, _ := data["copyright"].(string)
	if len(links) == 0 && copyright == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<footer class="site-footer"><div class="container">`)
	if len(links) > 0 {
		b.WriteString(`<nav aria-label="Footer"><ul>`)
		for _, l := range links {
			b.WriteString(`<li><a href="`)
			b.WriteString(escapeAttr(l.href))
			b.WriteString(`">`)
			b.WriteString(escapeText(l.label))
			b.WriteString(`</a></li>`)
		}
		b.WriteString(`</ul></nav>`)
	}
	if copyright != "" {
		b.WriteString(`<p class="site-footer-copy">&copy; `)
		b.WriteString(escapeText(copyright))
		b.WriteString(`</p>`)
	}
	b.WriteString(`</div></footer>`)
	return b.String()
}

func renderFooterColumnsHTML(data map[string]any, columnsRaw []any) string {
	var b strings.Builder
	cls := "site-footer"
	if t, _ := data["theme"].(string); t == "dark" {
		cls += " is-dark"
	}
	b.WriteString(`<footer class="`)
	b.WriteString(cls)
	b.WriteString(`"><div class="container">`)
	b.WriteString(`<div class="footer-columns">`)
	for _, c := range columnsRaw {
		col, ok := c.(map[string]any)
		if !ok {
			continue
		}
		title, _ := col["title"].(string)
		b.WriteString(`<div class="footer-column">`)
		if title != "" {
			b.WriteString(`<h3>`)
			b.WriteString(escapeText(title))
			b.WriteString(`</h3>`)
		}
		if linksRaw, ok := col["links"].([]any); ok && len(linksRaw) > 0 {
			b.WriteString(`<ul>`)
			for _, lr := range linksRaw {
				link, ok := lr.(map[string]any)
				if !ok {
					continue
				}
				href, _ := link["href"].(string)
				label, _ := link["label"].(string)
				b.WriteString(`<li><a href="`)
				b.WriteString(escapeAttr(href))
				b.WriteString(`">`)
				b.WriteString(escapeText(label))
				b.WriteString(`</a></li>`)
			}
			b.WriteString(`</ul>`)
		}
		if body, _ := col["body"].(string); body != "" {
			b.WriteString(`<p>`)
			b.WriteString(escapeText(body))
			b.WriteString(`</p>`)
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div>`) // /.footer-columns

	tagline, _ := data["tagline"].(string)
	copyright, _ := data["copyright"].(string)
	socialRaw, hasSocial := data["social"].([]any)
	if tagline != "" || copyright != "" || hasSocial {
		b.WriteString(`<div class="footer-bottom">`)
		if tagline != "" {
			b.WriteString(`<p class="footer-tagline">`)
			b.WriteString(escapeText(tagline))
			b.WriteString(`</p>`)
		}
		if hasSocial && len(socialRaw) > 0 {
			b.WriteString(`<div class="footer-social">`)
			for _, sr := range socialRaw {
				s, ok := sr.(map[string]any)
				if !ok {
					continue
				}
				href, _ := s["href"].(string)
				label, _ := s["label"].(string)
				icon, _ := s["icon"].(string)
				b.WriteString(`<a href="`)
				b.WriteString(escapeAttr(href))
				b.WriteString(`" aria-label="`)
				b.WriteString(escapeAttr(label))
				b.WriteString(`">`)
				if icon != "" {
					if svg := renderLucideIcon(icon); svg != "" {
						b.WriteString(svg)
					} else {
						b.WriteString(escapeText(label))
					}
				} else {
					b.WriteString(escapeText(label))
				}
				b.WriteString(`</a>`)
			}
			b.WriteString(`</div>`)
		}
		if copyright != "" {
			b.WriteString(`<p class="site-footer-copy">&copy; `)
			b.WriteString(escapeText(copyright))
			b.WriteString(`</p>`)
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div></footer>`)
	return b.String()
}

type navLink struct {
	label string
	href  string
	cta   bool
	lang  string
}

func extractNavLinks(raw any) []navLink {
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]navLink, 0, len(arr))
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		label, _ := obj["label"].(string)
		href, _ := obj["href"].(string)
		cta, _ := obj["cta"].(bool)
		lang, _ := obj["lang"].(string)
		label = strings.TrimSpace(label)
		href = strings.TrimSpace(href)
		if label == "" || href == "" {
			continue
		}
		out = append(out, navLink{label: label, href: href, cta: cta, lang: strings.TrimSpace(lang)})
	}
	return out
}

func escapeText(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func escapeAstroString(s string) string {
	s = strings.ReplaceAll(s, "'", "\\'")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// ogLocaleFromLang turns an HTML lang code (en, sv, de) into the underscore-
// separated form Open Graph wants (en_US, sv_SE, de_DE). Site-inspector's OG
// Completeness check requires og:locale; this is a best-effort default that
// can be overridden later via a setting if a user needs a regional variant.
func ogLocaleFromLang(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "en":
		return "en_US"
	case "sv":
		return "sv_SE"
	case "de":
		return "de_DE"
	case "no", "nb":
		return "nb_NO"
	case "da":
		return "da_DK"
	case "fi":
		return "fi_FI"
	case "fr":
		return "fr_FR"
	case "es":
		return "es_ES"
	case "":
		return "en_US"
	default:
		return strings.ToLower(strings.TrimSpace(lang)) + "_" + strings.ToUpper(strings.TrimSpace(lang))
	}
}
