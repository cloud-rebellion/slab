package starterkits

import (
	"context"
	"fmt"

	"github.com/bright-interaction/slab/internal/store"
)

func init() {
	Default.Register(&portfolioKit{})
}

type portfolioKit struct{}

func (k *portfolioKit) ID() string   { return "portfolio" }
func (k *portfolioKit) Name() string { return "Portfolio (One-Pager)" }
func (k *portfolioKit) Description() string {
	return "Personal portfolio one-pager. Hero, project grid, about block, contact."
}
func (k *portfolioKit) TargetSiteTypes() []string { return []string{"personal", "one-pager"} }

func (k *portfolioKit) Apply(ctx context.Context, q *store.Queries, siteID string) error {
	if err := applyBranding(ctx, q, siteID, BrandMonogram); err != nil {
		return fmt.Errorf("portfolio branding: %w", err)
	}
	if err := k.applyComponents(ctx, q, siteID); err != nil {
		return fmt.Errorf("portfolio components: %w", err)
	}
	if err := k.applyCSS(ctx, q, siteID); err != nil {
		return fmt.Errorf("portfolio css: %w", err)
	}
	if err := k.applyKnowledgebase(ctx, q, siteID); err != nil {
		return fmt.Errorf("portfolio kb: %w", err)
	}
	if err := k.applyGuardrails(ctx, q, siteID); err != nil {
		return fmt.Errorf("portfolio guardrails: %w", err)
	}
	if err := k.applyPages(ctx, q, siteID); err != nil {
		return fmt.Errorf("portfolio pages: %w", err)
	}
	return nil
}

func (k *portfolioKit) applyComponents(ctx context.Context, q *store.Queries, siteID string) error {
	defs := []componentDef{
		{
			Name:     "hero-portrait",
			Category: "hero",
			Template: `---
const { name, role, bio, avatar } = Astro.props;
---
<section class="hero-portrait scroll-snap">
  <div class="container">
    {avatar && <img src={avatar} alt={name} class="avatar" />}
    <h1>{name}</h1>
    <p class="role">{role}</p>
    <p class="bio">{bio}</p>
  </div>
</section>`,
			Props:      `{"type":"object","required":["name","role"],"properties":{"name":{"type":"string"},"role":{"type":"string"},"bio":{"type":"string"},"avatar":{"type":"string"}}}`,
			CSSClasses: `["hero-portrait","scroll-snap"]`,
			Usage:      "Centered portrait hero with avatar, name, role and one-line bio.",
		},
		{
			Name:     "project-grid",
			Category: "feature",
			Template: `---
const { heading, projects = [] } = Astro.props;
---
<section class="projects scroll-snap">
  <div class="container">
    {heading && <h2>{heading}</h2>}
    <ul class="project-grid">
      {projects.map((p) => (
        <li class="project-card">
          {p.image && <img src={p.image} alt={p.title} />}
          <h3>{p.title}</h3>
          <p class="role">{p.role}</p>
          <a href={p.href}>View case</a>
        </li>
      ))}
    </ul>
  </div>
</section>`,
			Props:      `{"type":"object","properties":{"heading":{"type":"string"},"projects":{"type":"array","items":{"type":"object","required":["title","href"],"properties":{"title":{"type":"string"},"role":{"type":"string"},"href":{"type":"string"},"image":{"type":"string"}}}}}}`,
			CSSClasses: `["project-grid","project-card"]`,
			Usage:      "Project grid with image cards, title, role, link.",
		},
		{
			Name:     "about-block",
			Category: "about",
			Template: `---
const { heading, body, skills = [] } = Astro.props;
---
<section class="about scroll-snap">
  <div class="container">
    {heading && <h2>{heading}</h2>}
    <div class="body">{body}</div>
    {skills.length > 0 && (
      <ul class="skills">
        {skills.map((s) => <li>{s}</li>)}
      </ul>
    )}
  </div>
</section>`,
			Props:      `{"type":"object","properties":{"heading":{"type":"string"},"body":{"type":"string"},"skills":{"type":"array","items":{"type":"string"}}}}`,
			CSSClasses: `["about"]`,
			Usage:      "About block with long text and skills list.",
		},
		{
			Name:     "contact-block",
			Category: "contact",
			Template: `---
const { email, socials = [] } = Astro.props;
---
<section class="contact scroll-snap">
  <div class="container">
    <h2>Get in touch</h2>
    {email && <p><a href={"mailto:" + email}>{email}</a></p>}
    <ul class="socials">
      {socials.map((s) => (
        <li><a href={s.href}>{s.label}</a></li>
      ))}
    </ul>
  </div>
</section>`,
			Props:      `{"type":"object","properties":{"email":{"type":"string"},"socials":{"type":"array","items":{"type":"object","required":["label","href"],"properties":{"label":{"type":"string"},"href":{"type":"string"}}}}}}`,
			CSSClasses: `["contact"]`,
			Usage:      "Contact block with email link and social links.",
		},
	}
	return seedComponents(ctx, q, siteID, defs)
}

func (k *portfolioKit) applyCSS(ctx context.Context, q *store.Queries, siteID string) error {
	defs := []cssDef{
		{
			Name:     ".scroll-snap",
			Category: "layout",
			CSS:      ".scroll-snap { scroll-snap-align: start; min-height: 100vh; display: flex; align-items: center; }",
			Usage:    "One-pager scroll-snap section.",
		},
		{
			Name:     ".project-card",
			Category: "card",
			CSS:      ".project-card { display: flex; flex-direction: column; gap: 0.5rem; border-radius: 0.75rem; overflow: hidden; background: var(--bg-soft, #f8fafc); padding: 1rem; transition: transform 0.2s ease; } .project-card:hover { transform: translateY(-2px); } .project-card img { aspect-ratio: 16/9; object-fit: cover; border-radius: 0.5rem; }",
			Usage:    "Portfolio project card with image.",
		},
	}
	return seedCSS(ctx, q, siteID, defs)
}

func (k *portfolioKit) applyKnowledgebase(ctx context.Context, q *store.Queries, siteID string) error {
	entries := []kbDef{
		{Category: "brand", Title: "Brand voice", Content: "Personal, conversational, work-focused. Write in first person. Show the work, talk about the role you played, what shipped and what you learned. Avoid abstract claims."},
		{Category: "brand", Title: "What goes on the page", Content: "Hero with name and role, three to six recent projects, a short about section with skills, and a contact block. Nothing else. The page is a one-pager."},
	}
	return seedKB(ctx, q, siteID, entries, 100)
}

func (k *portfolioKit) applyGuardrails(ctx context.Context, q *store.Queries, siteID string) error {
	rules := []guardrailDef{
		{RuleType: "allow_block_type", Target: "*", Value: "hero,text,image,cta,feature_grid", Severity: "error"},
		{RuleType: "forbid_pattern", Target: "block.data.text", Value: "lorem ipsum", Severity: "error"},
	}
	return seedGuardrails(ctx, q, siteID, rules)
}

func (k *portfolioKit) applyPages(ctx context.Context, q *store.Queries, siteID string) error {
	homeID := newKitID()
	if err := q.CreatePage(ctx, store.CreatePageParams{
		ID:        homeID,
		SiteID:    siteID,
		Title:     "Home",
		Slug:      "/",
		Layout:    "one-pager",
		SortOrder: 0,
		ShowInNav: 1,
	}); err != nil {
		return err
	}
	blocks := []blockDef{
		HeroBlock(HeroPayload{
			Eyebrow:        "INDEPENDENT DESIGN AND ENGINEERING - STOCKHOLM",
			Headline:       "Calm interfaces. Fast pages. Work people actually use.",
			Subheading:     "Ten years shipping product across consumer, fintech, and B2B. I take the messy work end-to-end: research, design, the engineering to ship it, and the analytics to know it worked.",
			CTAText:        "See selected work",
			CTAUrl:         "#work",
			SecondaryLabel: "Email me",
			SecondaryUrl:   "mailto:hello@example.com",
			HeroGraphic:    "monogram",
			MonogramChar:   "N",
		}),
		LogoStripBlock("Clients and collaborators", []LogoItem{
			{Label: "Klarna"}, {Label: "Tibber"}, {Label: "Hemnet"},
			{Label: "Spotify"}, {Label: "Northvolt"}, {Label: "Tink"},
		}),
		StatGridBlock("How I work", []StatItem{
			{Value: "10 yr", Label: "shipping product", Context: "Consumer, fintech, B2B SaaS"},
			{Value: "23", Label: "products in production", Context: "From discovery to active maintenance"},
			{Value: "1", Label: "person on the project", Context: "I do the design AND the engineering"},
		}),
		{BlockType: "feature_grid", Data: `{"component":"project-grid","heading":"Selected work","projects":[{"title":"Reimagined the Tibber onboarding flow","role":"Lead designer + frontend engineer","href":"/work/tibber-onboarding","image":""},{"title":"Klarna merchant analytics dashboard","role":"Design system + dataviz","href":"/work/klarna-merchants","image":""},{"title":"Hemnet mobile search redesign","role":"Research + design + ship","href":"/work/hemnet-search","image":""}]}`},
		{BlockType: "quote", Data: `{"quote":"She came in for two weeks of audit, stayed for six months of execution, and shipped three things that are now load-bearing for the team.","attribution":"Head of Product, Series C fintech"}`},
		{BlockType: "text", Data: `{"component":"about-block","heading":"About","body":"I have spent the last decade shipping product across consumer, fintech, and B2B. I like clear typography, fast pages, and shipping things people actually use. Background in computer science, ten years on the design side, four of those running my own practice. Available for fractional CTO/CDO engagements and project-scope sprints.","skills":["Product design","Frontend engineering","Design systems","Accessibility (WCAG AA)","Research and discovery","Analytics and experimentation"]}`},
		AccordionFAQBlock("Frequently asked", []FAQItem{
			{Question: "Do you do both design AND engineering?", Answer: "Yes. That's the value proposition: one person closes the loop from research to merged PR. For larger engagements I bring in a trusted second designer or engineer; for solo sprints I cover both roles myself."},
			{Question: "What kind of engagements do you take?", Answer: "Three shapes: (1) two-week audits with a written report and recommendations, (2) six-to-twelve-week project sprints with a defined deliverable, (3) fractional CTO/CDO for early-stage teams two days a week. Anything outside that I refer out."},
			{Question: "What's your day rate?", Answer: "I don't sell hours. Audits are fixed-price (typically 60-90k SEK), sprints are scoped per project, and fractional retainers are monthly. Get in touch with the shape of the problem and I'll price it after a 30-minute scope call."},
			{Question: "Where are you based?", Answer: "Stockholm, but I work primarily remote across European timezones. I travel to clients in Europe roughly once a quarter for kickoffs and major reviews. North America possible for the right project."},
			{Question: "Can I see longer case studies?", Answer: "Yes, send me an email and I'll share two or three case studies relevant to your stage and industry. The public site shows the surface; the longer reads are NDA-friendly and walk through the messy parts."},
		}),
		{BlockType: "cta", Data: `{"component":"contact-block","email":"hello@example.com","socials":[{"label":"Read CV","href":"/cv"},{"label":"GitHub","href":"https://github.com/"},{"label":"LinkedIn","href":"https://linkedin.com/"}]}`},
	}
	return seedBlocks(ctx, q, homeID, blocks)
}
