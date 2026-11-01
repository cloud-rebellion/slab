import { test, expect, u } from '../../fixtures/auth';
import {
	createBlock,
	createPage,
	createSite,
	deleteSite,
	type Site,
	type Page_
} from '../../fixtures/data';

test.describe('admin-api: block + page preview source', () => {
	const cleanup: string[] = [];
	let site: Site;
	let otherSite: Site;
	let page: Page_;

	test.beforeAll(async ({ adminApi }) => {
		site = await createSite(adminApi);
		cleanup.push(site.id);
		otherSite = await createSite(adminApi);
		cleanup.push(otherSite.id);
		page = await createPage(adminApi, site.id);
	});

	test.afterAll(async ({ adminApi }) => {
		for (const id of cleanup) await deleteSite(adminApi, id);
	});

	test('GET block preview returns rendered Astro source', async ({ adminApi }) => {
		// Phase 24 (2026-04-30) renamed the hero data field from
		// `heading` to `headline` and emits the heading at <h1> level
		// (one h1 per page, hero owns it). Subheading renders as
		// <p class="subheading">.
		const block = await createBlock(adminApi, site.id, page.id, {
			block_type: 'hero',
			data: {
				headline: 'A calm site',
				subheading: 'that earns the click',
				cta_text: 'Get started',
				cta_url: '/start'
			}
		});
		const res = await adminApi.get(
			u(`/api/sites/${site.id}/pages/${page.id}/blocks/${block.id}/preview`)
		);
		expect(res.ok()).toBe(true);
		const body = await res.json();
		expect(body.block_id).toBe(block.id);
		expect(body.block_type).toBe('hero');
		expect(body.page_id).toBe(page.id);
		expect(body.astro).toContain('<h1>A calm site</h1>');
		expect(body.astro).toContain('<p class="subheading">that earns the click</p>');
		expect(body.astro).toContain('href="/start"');
		expect(body.astro).toContain('>Get started</a>');
	});

	test('GET block preview rejects cross-site fetch with 404', async ({ adminApi }) => {
		const block = await createBlock(adminApi, site.id, page.id);
		const res = await adminApi.get(
			u(`/api/sites/${otherSite.id}/pages/${page.id}/blocks/${block.id}/preview`)
		);
		expect(res.status()).toBe(404);
	});

	test('GET block preview returns 404 for unknown block id', async ({ adminApi }) => {
		const res = await adminApi.get(
			u(`/api/sites/${site.id}/pages/${page.id}/blocks/does-not-exist/preview`)
		);
		expect(res.status()).toBe(404);
	});

	test('unsafe script input is rejected before persistence; safe text preview escapes metacharacters', async ({
		adminApi
	}) => {
		// The shared admin/agent/MCP write guard rejects scripts. Legacy stored-data
		// escaping is pinned by builder.TestRenderBlock_HTMLEscape.
		const before = await adminApi.get(u(`/api/sites/${site.id}/pages/${page.id}/blocks`));
		expect(before.ok()).toBe(true);
		const beforeIDs = ((await before.json()).blocks as { id: string }[]).map((b) => b.id).sort();
		const refused = await adminApi.post(u(`/api/sites/${site.id}/pages/${page.id}/blocks`), {
			data: {
				block_type: 'text',
				data: { heading: '<script>alert(1)</script>', text: 'AT&T' }
			}
		});
		expect(refused.status()).toBe(422);
		const rejection = await refused.json();
		expect(rejection.error).toBe('Guardrail violations');
		expect(rejection.violations).toEqual(
			expect.arrayContaining([expect.objectContaining({ rule: 'security', severity: 'error' })])
		);

		const after = await adminApi.get(u(`/api/sites/${site.id}/pages/${page.id}/blocks`));
		expect(after.ok()).toBe(true);
		expect(((await after.json()).blocks as { id: string }[]).map((b) => b.id).sort()).toEqual(beforeIDs);

		const block = await createBlock(adminApi, site.id, page.id, {
			block_type: 'text',
			data: { heading: '<example> & text', text: 'AT&T' }
		});
		const res = await adminApi.get(
			u(`/api/sites/${site.id}/pages/${page.id}/blocks/${block.id}/preview`)
		);
		expect(res.ok()).toBe(true);
		const body = await res.json();
		expect(body.astro).not.toContain('<example>');
		expect(body.astro).toContain('&lt;example&gt; &amp; text');
		expect(body.astro).toContain('AT&amp;T');
	});

	test('unsafe URL input is rejected before persistence; safe CTA preview preserves its URL', async ({
		adminApi
	}) => {
		// Render-time rewriting of legacy data remains pinned by
		// builder.TestRenderBlock_RejectsDangerousScheme; current writes refuse it.
		const before = await adminApi.get(u(`/api/sites/${site.id}/pages/${page.id}/blocks`));
		expect(before.ok()).toBe(true);
		const beforeIDs = ((await before.json()).blocks as { id: string }[]).map((b) => b.id).sort();
		const refused = await adminApi.post(u(`/api/sites/${site.id}/pages/${page.id}/blocks`), {
			data: {
				block_type: 'cta',
				data: {
					label: 'Book a discovery call',
					cta_text: 'Book a discovery call',
					cta_url: 'javascript:alert(1)'
				}
			}
		});
		expect(refused.status()).toBe(422);
		const rejection = await refused.json();
		expect(rejection.error).toBe('Guardrail violations');
		expect(rejection.violations).toEqual([
			expect.objectContaining({ rule: 'security', severity: 'error' })
		]);

		const after = await adminApi.get(u(`/api/sites/${site.id}/pages/${page.id}/blocks`));
		expect(after.ok()).toBe(true);
		expect(((await after.json()).blocks as { id: string }[]).map((b) => b.id).sort()).toEqual(beforeIDs);

		const block = await createBlock(adminApi, site.id, page.id, {
			block_type: 'cta',
			data: {
				label: 'Book a discovery call',
				cta_text: 'Book a discovery call',
				cta_url: 'https://site.e2e.invalid/contact'
			}
		});
		const res = await adminApi.get(
			u(`/api/sites/${site.id}/pages/${page.id}/blocks/${block.id}/preview`)
		);
		expect(res.ok()).toBe(true);
		const body = await res.json();
		expect(body.astro).not.toContain('javascript:');
		expect(body.astro).toContain('href="https://site.e2e.invalid/contact"');
	});

	test('GET page preview returns assembled .astro source', async ({ adminApi }) => {
		const fresh = await createPage(adminApi, site.id, { title: 'About', slug: 'about' });
		// Hero uses `headline` and renders <h1>; text uses `heading` and
		// renders <h2>. The order check below verifies the renderer
		// emits blocks in sort_order regardless of block_type.
		await createBlock(adminApi, site.id, fresh.id, {
			block_type: 'hero',
			data: { headline: 'First section' },
			sort_order: 0
		});
		await createBlock(adminApi, site.id, fresh.id, {
			block_type: 'text',
			data: { heading: 'Second section' },
			sort_order: 1
		});
		const res = await adminApi.get(u(`/api/sites/${site.id}/pages/${fresh.id}/preview`));
		expect(res.ok()).toBe(true);
		const body = await res.json();
		expect(body.page_id).toBe(fresh.id);
		expect(body.slug.replace(/^\//, '')).toBe('about');
		expect(body.astro).toContain("import Base from '../layouts/Base.astro'");
		expect(body.astro).toContain('<Base title=');
		const firstIdx = body.astro.indexOf('<h1>First section</h1>');
		const secondIdx = body.astro.indexOf('<h2>Second section</h2>');
		expect(firstIdx).toBeGreaterThan(-1);
		expect(secondIdx).toBeGreaterThan(firstIdx);
	});

	test('GET page preview rejects cross-site fetch with 404', async ({ adminApi }) => {
		const res = await adminApi.get(u(`/api/sites/${otherSite.id}/pages/${page.id}/preview`));
		expect(res.status()).toBe(404);
	});

	test('GET page preview returns 404 for unknown page id', async ({ adminApi }) => {
		const res = await adminApi.get(u(`/api/sites/${site.id}/pages/missing-page/preview`));
		expect(res.status()).toBe(404);
	});

	test('Page preview reflects PATCH edits without a build', async ({ adminApi }) => {
		const fresh = await createPage(adminApi, site.id);
		// Use a text block (heading -> <h2>) so the test stays
		// invariant under any future hero re-shape.
		const block = await createBlock(adminApi, site.id, fresh.id, {
			block_type: 'text',
			data: { heading: 'Original' }
		});
		let res = await adminApi.get(u(`/api/sites/${site.id}/pages/${fresh.id}/preview`));
		let body = await res.json();
		expect(body.astro).toContain('<h2>Original</h2>');

		const patch = await adminApi.patch(
			u(`/api/sites/${site.id}/pages/${fresh.id}/blocks/${block.id}`),
			{ data: { data: { heading: 'Edited live' } } }
		);
		expect(patch.ok()).toBe(true);

		res = await adminApi.get(u(`/api/sites/${site.id}/pages/${fresh.id}/preview`));
		body = await res.json();
		expect(body.astro).toContain('<h2>Edited live</h2>');
		expect(body.astro).not.toContain('<h2>Original</h2>');
	});

	test('Hidden blocks do not appear in page preview', async ({ adminApi }) => {
		const fresh = await createPage(adminApi, site.id);
		await createBlock(adminApi, site.id, fresh.id, {
			block_type: 'text',
			data: { heading: 'Visible row' }
		});
		const hidden = await createBlock(adminApi, site.id, fresh.id, {
			block_type: 'text',
			data: { heading: 'Hidden row' }
		});
		await adminApi.patch(
			u(`/api/sites/${site.id}/pages/${fresh.id}/blocks/${hidden.id}`),
			{ data: { is_visible: false } }
		);
		const res = await adminApi.get(u(`/api/sites/${site.id}/pages/${fresh.id}/preview`));
		expect(res.ok()).toBe(true);
		const body = await res.json();
		expect(body.astro).toContain('<h2>Visible row</h2>');
		expect(body.astro).not.toContain('<h2>Hidden row</h2>');
	});
});
