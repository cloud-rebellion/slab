package starterkits

import (
	"context"
	"fmt"

	"github.com/bright-interaction/slab/internal/store"
)

func init() {
	Default.Register(&saasLandingKit{})
}

type saasLandingKit struct{}

func (k *saasLandingKit) ID() string   { return "saas-landing" }
func (k *saasLandingKit) Name() string { return "SaaS Landing Page" }
func (k *saasLandingKit) Description() string {
	return "Modern English SaaS marketing site. Hero, features, pricing, testimonials, FAQ, CTA, docs hub."
}
func (k *saasLandingKit) TargetSiteTypes() []string { return []string{"b2b", "b2c"} }

func (k *saasLandingKit) Apply(ctx context.Context, q *store.Queries, siteID string) error {
	if err := applyBranding(ctx, q, siteID, BrandMesh); err != nil {
		return fmt.Errorf("saas-landing branding: %w", err)
	}
	if err := k.applyComponents(ctx, q, siteID); err != nil {
		return fmt.Errorf("saas-landing components: %w", err)
	}
	if err := k.applyCSS(ctx, q, siteID); err != nil {
		return fmt.Errorf("saas-landing css: %w", err)
	}
	if err := k.applyKnowledgebase(ctx, q, siteID); err != nil {
		return fmt.Errorf("saas-landing kb: %w", err)
	}
	if err := k.applyGuardrails(ctx, q, siteID); err != nil {
		return fmt.Errorf("saas-landing guardrails: %w", err)
	}
	if err := k.applyPages(ctx, q, siteID); err != nil {
		return fmt.Errorf("saas-landing pages: %w", err)
	}
	return nil
}

func (k *saasLandingKit) applyComponents(ctx context.Context, q *store.Queries, siteID string) error {
	defs := []componentDef{
		{
			Name:     "hero-saas",
			Category: "hero",
			Template: `---
const { eyebrow, heading, subheading, ctaLabel, ctaHref, secondaryLabel, secondaryHref } = Astro.props;
---
<section class="hero-saas">
  <div class="container">
    {eyebrow && <p class="eyebrow">{eyebrow}</p>}
    <h1>{heading}</h1>
    <p class="subheading">{subheading}</p>
    <p class="actions">
      <a class="btn gradient-cta" href={ctaHref}>{ctaLabel}</a>
      {secondaryHref && <a class="btn btn-ghost" href={secondaryHref}>{secondaryLabel}</a>}
    </p>
  </div>
</section>`,
			Props:      `{"type":"object","required":["heading","subheading","ctaLabel","ctaHref"],"properties":{"eyebrow":{"type":"string"},"heading":{"type":"string"},"subheading":{"type":"string"},"ctaLabel":{"type":"string"},"ctaHref":{"type":"string"},"secondaryLabel":{"type":"string"},"secondaryHref":{"type":"string"}}}`,
			CSSClasses: `["hero-saas","gradient-cta"]`,
			Usage:      "SaaS hero with eyebrow, heading, subheading, primary and secondary CTA.",
		},
		{
			Name:     "feature-grid",
			Category: "feature",
			Template: `---
const { heading, features = [] } = Astro.props;
---
<section class="features">
  <div class="container">
    {heading && <h2>{heading}</h2>}
    <ul class="feature-grid">
      {features.map((f) => (
        <li class="feature-tile">
          {f.icon && <span class="icon">{f.icon}</span>}
          <h3>{f.title}</h3>
          <p>{f.body}</p>
        </li>
      ))}
    </ul>
  </div>
</section>`,
			Props:      `{"type":"object","properties":{"heading":{"type":"string"},"features":{"type":"array","items":{"type":"object","required":["title","body"],"properties":{"title":{"type":"string"},"body":{"type":"string"},"icon":{"type":"string"}}}}}}`,
			CSSClasses: `["feature-tile"]`,
			Usage:      "Feature grid with icon, title, body.",
		},
		{
			Name:     "pricing-table",
			Category: "pricing",
			Template: `---
const { heading, tiers = [] } = Astro.props;
---
<section class="pricing">
  <div class="container">
    {heading && <h2>{heading}</h2>}
    <ul class="pricing-table">
      {tiers.map((t) => (
        <li class={t.featured ? "pricing-tier featured" : "pricing-tier"}>
          <h3>{t.name}</h3>
          <p class="price">{t.price}</p>
          <ul class="features">
            {(t.features || []).map((f) => <li>{f}</li>)}
          </ul>
          <a class="btn gradient-cta" href={t.ctaHref}>{t.ctaLabel}</a>
        </li>
      ))}
    </ul>
  </div>
</section>`,
			Props:      `{"type":"object","properties":{"heading":{"type":"string"},"tiers":{"type":"array","items":{"type":"object","required":["name","price","ctaLabel","ctaHref"],"properties":{"name":{"type":"string"},"price":{"type":"string"},"features":{"type":"array","items":{"type":"string"}},"ctaLabel":{"type":"string"},"ctaHref":{"type":"string"},"featured":{"type":"boolean"}}}}}}`,
			CSSClasses: `["pricing-tier"]`,
			Usage:      "Three-tier pricing table with optional featured tier.",
		},
		{
			Name:     "testimonial-carousel",
			Category: "social_proof",
			Template: `---
const { items = [] } = Astro.props;
---
<section class="testimonials">
  <div class="container">
    <ul class="testimonial-carousel">
      {items.map((t) => (
        <li class="testimonial">
          <blockquote>{t.quote}</blockquote>
          <p class="cite">{t.author}, {t.role}</p>
        </li>
      ))}
    </ul>
  </div>
</section>`,
			Props:      `{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","required":["quote","author"],"properties":{"quote":{"type":"string"},"author":{"type":"string"},"role":{"type":"string"}}}}}}`,
			CSSClasses: `["testimonial-carousel"]`,
			Usage:      "Customer testimonial carousel with quote, author and role.",
		},
		{
			Name:     "faq-accordion",
			Category: "faq",
			Template: `---
const { heading, items = [] } = Astro.props;
---
<section class="faq">
  <div class="container">
    {heading && <h2>{heading}</h2>}
    {items.map((it) => (
      <details class="faq-item">
        <summary>{it.q}</summary>
        <p>{it.a}</p>
      </details>
    ))}
  </div>
</section>`,
			Props:      `{"type":"object","properties":{"heading":{"type":"string"},"items":{"type":"array","items":{"type":"object","required":["q","a"],"properties":{"q":{"type":"string"},"a":{"type":"string"}}}}}}`,
			CSSClasses: `["faq-item"]`,
			Usage:      "Native disclosure FAQ accordion.",
		},
		{
			Name:     "metric-card",
			Category: "metric",
			Template: `---
const { value, label, delta } = Astro.props;
---
<div class="metric-card">
  <p class="value">{value}</p>
  <p class="label">{label}</p>
  {delta && <p class="delta">{delta}</p>}
</div>`,
			Props:      `{"type":"object","required":["value","label"],"properties":{"value":{"type":"string"},"label":{"type":"string"},"delta":{"type":"string"}}}`,
			CSSClasses: `["metric-card"]`,
			Usage:      "Single metric card for hero or features section.",
		},
	}
	return seedComponents(ctx, q, siteID, defs)
}

func (k *saasLandingKit) applyCSS(ctx context.Context, q *store.Queries, siteID string) error {
	defs := []cssDef{
		{
			Name:     ".gradient-cta",
			Category: "button",
			CSS:      ".gradient-cta { background: linear-gradient(90deg, var(--accent, #6366f1), var(--accent-2, #8b5cf6)); color: #fff; padding: 0.75rem 1.5rem; border-radius: 0.5rem; font-weight: 600; }",
			Usage:    "Gradient primary CTA button.",
		},
		{
			Name:     ".metric-card",
			Category: "card",
			CSS:      ".metric-card { display: flex; flex-direction: column; gap: 0.25rem; padding: 1rem; border-radius: 0.75rem; background: var(--bg-soft, #f8fafc); } .metric-card .value { font-size: 2.25rem; font-weight: 700; line-height: 1; } .metric-card .label { color: var(--muted, #64748b); }",
			Usage:    "Metric card with value, label, optional delta.",
		},
		{
			Name:     ".feature-tile",
			Category: "card",
			CSS:      ".feature-tile { padding: 1.5rem; border: 1px solid var(--border, #e2e8f0); border-radius: 0.75rem; } .feature-tile .icon { display: inline-flex; width: 2.5rem; height: 2.5rem; align-items: center; justify-content: center; border-radius: 0.5rem; background: var(--accent-soft, #eef2ff); margin-bottom: 0.75rem; }",
			Usage:    "Feature tile with icon, title, body.",
		},
	}
	return seedCSS(ctx, q, siteID, defs)
}

func (k *saasLandingKit) applyKnowledgebase(ctx context.Context, q *store.Queries, siteID string) error {
	entries := []kbDef{
		{Category: "brand", Title: "Brand voice", Content: "Concise, technical, benefit-led. Lead with the outcome, follow with the mechanism. Avoid filler. No marketing jargon. No 'revolutionary'."},
		{Category: "brand", Title: "Target audience", Content: "Developers and small product teams shipping fast. They want proof, not promises. Code examples and pricing transparency win."},
		{Category: "technical", Title: "Pricing page rules", Content: "Always show three tiers. Always include a free or starter tier. The middle tier is the recommended one and gets the featured visual treatment. Show the price prominently."},
		{Category: "technical", Title: "Docs structure", Content: "Docs live under /docs as a silo hub with intro, quickstart, reference and guides. Every doc page has a working code example near the top."},
	}
	return seedKB(ctx, q, siteID, entries, 100)
}

func (k *saasLandingKit) applyGuardrails(ctx context.Context, q *store.Queries, siteID string) error {
	rules := []guardrailDef{
		{RuleType: "allow_block_type", Target: "*", Value: "hero,text,image,cta,feature_grid,pricing_table,testimonial,faq,metric_card", Severity: "error"},
		{RuleType: "forbid_pattern", Target: "block.data.text", Value: "lorem ipsum", Severity: "error"},
		{RuleType: "require_block", Target: "page.layout=landing", Value: "cta", Severity: "warning"},
	}
	return seedGuardrails(ctx, q, siteID, rules)
}

func (k *saasLandingKit) applyPages(ctx context.Context, q *store.Queries, siteID string) error {
	// Home page
	homeID := newKitID()
	if err := q.CreatePage(ctx, store.CreatePageParams{
		ID:        homeID,
		SiteID:    siteID,
		Title:     "Home",
		Slug:      "/",
		Layout:    "landing",
		SortOrder: 0,
		ShowInNav: 1,
	}); err != nil {
		return err
	}
	homeBlocks := []blockDef{
		HeroBlock(HeroPayload{
			Eyebrow:        "BUILT FOR ENGINEERS, NOT PROCUREMENT",
			Headline:       "Ship the API your customers thought you already had.",
			HeadlineAccent: "Typed clients, predictable pricing, zero meetings.",
			Subheading:     "Drop in. Get a TypeScript or Go client with IntelliSense. Pay per seat, never per event. Audit the OpenAPI 3.1 schema before you sign anything.",
			CTAText:        "Start free",
			CTAUrl:         "/pricing",
			SecondaryLabel: "Read the docs",
			SecondaryUrl:   "/docs",
			HeroGraphic:    "mesh",
		}),
		LogoStripBlock("In production at engineering teams who hate vendor demos", []LogoItem{
			{Label: "Lattice"}, {Label: "Northwind"}, {Label: "Pylon"},
			{Label: "Mux"}, {Label: "Spinnerly"}, {Label: "Korex"},
		}),
		StatGridBlock("Results from the first 90 days", []StatItem{
			{Value: "47ms", Label: "p99 API latency", Context: "Frankfurt and Iowa regions, last 30 days"},
			{Value: "11 min", Label: "Median time-to-first-request", Context: "From signup, no demo"},
			{Value: "3 SDKs", Label: "Typed clients shipped", Context: "TypeScript, Go, Python; Rust next"},
		}),
		{BlockType: "feature_grid", Data: `{"component":"feature-grid","heading":"Three things every backend you ever bought failed at","features":[{"title":"Typed SDKs that match the spec","body":"First-class TypeScript and Go clients. The schema is the source of truth, not a Notion doc.","icon":"code"},{"title":"Pricing you can predict on a Friday","body":"Per-seat, never per-event. The bill on the 1st matches the bill on the 15th.","icon":"dollar-sign"},{"title":"OpenAPI 3.1 published, signed, dated","body":"Audit before you sign. Generate your own client. Mirror it to your private registry.","icon":"file-check"}]}`},
		{BlockType: "quote", Data: `{"quote":"We replaced three vendors with this in a week. The bit that sold us was not the demo, it was the OpenAPI spec being public.","attribution":"Sara P, CTO at Lattice"}`},
		{BlockType: "pricing", Data: `{"heading":"Predictable pricing","subheading":"No volume cliffs, no usage tax, no surprises.","tiers":[{"name":"Starter","price":"$0","price_period":"forever","description":"One project, community support, the same API as paid plans.","features":[{"":"1 active project"},{"":"50k requests / month"},{"":"Community Discord"},{"":"OpenAPI export"}],"cta_text":"Start free","cta_url":"/signup"},{"name":"Team","price":"$29","price_period":"per seat / month","description":"For teams that want SSO and a humans-in-the-loop support line.","features":[{"":"Unlimited projects"},{"":"500k requests / month / seat"},{"":"SSO + audit log"},{"":"Slack support"},{"":"Staging environment"}],"cta_text":"Start 14-day trial","cta_url":"/signup?plan=team","featured":true},{"name":"Enterprise","price":"Custom","description":"For when procurement gets involved.","features":[{"":"SLA + 24/7 oncall"},{"":"SOC 2 Type II report"},{"":"Dedicated CSM"},{"":"Private cloud option"},{"":"DPA + signed sub-processor list"}],"cta_text":"Contact sales","cta_url":"/contact"}]}`},
		AccordionFAQBlock("Questions we get on every sales call", []FAQItem{
			{Question: "Is there really a free tier or is it a 14-day trial?", Answer: "Free tier is forever. One project, 50,000 requests per month, full OpenAPI export. We make money from teams that need SSO, audit logs, and the staging environment, not from squeezing solo developers."},
			{Question: "Where is the data stored?", Answer: "EU by default (Frankfurt). US (Iowa) is opt-in per project. Both regions run the same code and the same SLA. We publish the sub-processor list and update it within 7 days of any change."},
			{Question: "Can I self-host?", Answer: "Enterprise plan only. The image is on Harbor, the Helm chart is versioned, and we sign every release with cosign. Self-host customers get the same OpenAPI spec the hosted version exposes."},
			{Question: "Do you have a SOC 2 report?", Answer: "Type II, refreshed annually. We send it under NDA before the call so you can pre-read it. Penetration test summary is part of the same packet."},
			{Question: "Will my pricing change as I grow?", Answer: "Only if you add seats. The per-seat price has not changed since launch. There is no per-event tax, no volume cliff. Per-seat is per-seat."},
		}),
		{BlockType: "cta", Data: `{"heading":"Start in eleven minutes","text":"No credit card. No demo call. Auth, billing, and SDKs are working in your terminal before the next standup.","cta_text":"Start free","cta_url":"/signup","variant":"primary"}`},
	}
	if err := seedBlocks(ctx, q, homeID, homeBlocks); err != nil {
		return err
	}

	// Other pages
	others := []struct {
		title  string
		slug   string
		layout string
		nav    int64
		hero   string
		body   string
	}{
		{"Docs", "/docs", "default", 1, "Documentation", "Quickstart, reference, and guides."},
		{"Pricing", "/pricing", "default", 1, "Pricing", "Simple, predictable pricing."},
		{"About", "/about", "default", 1, "About us", "Why we built this and where we are headed."},
	}
	for i, p := range others {
		pageID := newKitID()
		if err := q.CreatePage(ctx, store.CreatePageParams{
			ID:        pageID,
			SiteID:    siteID,
			Title:     p.title,
			Slug:      p.slug,
			Layout:    p.layout,
			SortOrder: int64(10 + i),
			ShowInNav: p.nav,
		}); err != nil {
			return err
		}
		blocks := []blockDef{
			{BlockType: "hero", Data: fmt.Sprintf(`{"component":"hero-saas","heading":%q,"subheading":%q,"ctaLabel":"Get started","ctaHref":"/signup"}`, p.hero, p.body)},
		}
		if p.slug == "/pricing" {
			blocks = append(blocks, blockDef{BlockType: "feature_grid", Data: `{"component":"pricing-table","tiers":[{"name":"Starter","price":"$0","features":["1 project","Community support"],"ctaLabel":"Start","ctaHref":"/signup"},{"name":"Team","price":"$29 / seat","features":["Unlimited projects","Slack support","SSO"],"ctaLabel":"Start trial","ctaHref":"/signup?plan=team","featured":true},{"name":"Enterprise","price":"Talk to us","features":["SLA","SOC 2 report","Dedicated CSM"],"ctaLabel":"Contact sales","ctaHref":"/contact"}]}`})
		}
		if err := seedBlocks(ctx, q, pageID, blocks); err != nil {
			return err
		}
	}

	return nil
}
