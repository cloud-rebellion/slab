import { test, expect } from '../../fixtures/auth';
import { createBlock, createSite, deleteSite, triggerBuildAndWait, u } from '../../fixtures/data';
import { createPublishedPage, distExists, readDist } from './_helpers';

// Sprint 3 slice B (2026-05-24): 3-locale build smoke. Configures
// site_locales for en (default) + sv + de, authors a page with two
// blocks + a locale_switcher, adds page_locale + block_locale
// overlays for sv and de (published), then triggers a real build.
// Asserts:
//   - 3 HTML files emitted (one per locale)
//   - per-locale block content swapped where overlay exists
//   - locale_switcher emits 3 entries with the active locale marked
//   - hreflang <link> tags present on every page pointing at the
//     other two locale counterparts

test.describe('build-pipeline: 3-locale multilingual', () => {
	const cleanup: string[] = [];

	test.afterAll(async ({ adminApi }) => {
		for (const id of cleanup) await deleteSite(adminApi, id);
	});

	test('emits one HTML per locale with overlays + hreflang + switcher links', async ({
		adminApi
	}) => {
		test.setTimeout(240_000);
		const site = await createSite(adminApi);
		cleanup.push(site.id);

		// Configure 3 locales.
		await adminApi.post(u(`/api/sites/${site.id}/locales`), {
			data: { locale: 'en', is_default: true, sort_order: 0 }
		});
		await adminApi.post(u(`/api/sites/${site.id}/locales`), {
			data: { locale: 'sv', is_default: false, sort_order: 1 }
		});
		await adminApi.post(u(`/api/sites/${site.id}/locales`), {
			data: { locale: 'de', is_default: false, sort_order: 2 }
		});

		// Author one page with a text block + a locale_switcher.
		const pg = await createPublishedPage(adminApi, site.id, {
			slug: 'multi',
			title: 'Multi'
		});
		const textBlock = await createBlock(adminApi, site.id, pg.id, {
			block_type: 'text',
			data: { text: 'Hello English' },
			sort_order: 1
		});
		await createBlock(adminApi, site.id, pg.id, {
			block_type: 'locale_switcher',
			data: { label: 'Language', style: 'inline' },
			sort_order: 2
		});

		// Publish sv + de overlays for the page + the text block.
		await adminApi.put(u(`/api/sites/${site.id}/pages/${pg.id}/locales/sv`), {
			data: { title: 'Multi (sv)', status: 'published' }
		});
		await adminApi.put(u(`/api/sites/${site.id}/blocks/${textBlock.id}/locales/sv`), {
			data: { data_json: JSON.stringify({ text: 'Hej svenska' }), is_visible: true }
		});
		await adminApi.put(u(`/api/sites/${site.id}/pages/${pg.id}/locales/de`), {
			data: { title: 'Multi (de)', status: 'published' }
		});
		await adminApi.put(u(`/api/sites/${site.id}/blocks/${textBlock.id}/locales/de`), {
			data: { data_json: JSON.stringify({ text: 'Hallo deutsch' }), is_visible: true }
		});

		const result = await triggerBuildAndWait(adminApi, site.id, 180_000);
		expect(result.status, `build_log:\n${result.build_log}`).toBe('success');

		// One HTML per locale at the expected paths.
		expect(distExists(site.id, 'multi/index.html')).toBe(true);
		expect(distExists(site.id, 'sv/multi/index.html')).toBe(true);
		expect(distExists(site.id, 'de/multi/index.html')).toBe(true);

		const enHtml = readDist(site.id, 'multi/index.html');
		const svHtml = readDist(site.id, 'sv/multi/index.html');
		const deHtml = readDist(site.id, 'de/multi/index.html');

		// Per-locale content swap.
		expect(enHtml).toContain('Hello English');
		expect(enHtml).not.toContain('Hej svenska');
		expect(svHtml).toContain('Hej svenska');
		expect(svHtml).not.toContain('Hello English');
		expect(deHtml).toContain('Hallo deutsch');

		// locale_switcher emits one entry per locale; current is marked.
		// Inline style renders <span aria-current="true"> for the active
		// locale and <a> for the others.
		expect(svHtml).toContain('aria-current="true"');
		expect(svHtml).toContain('hreflang="en"');
		expect(svHtml).toContain('hreflang="de"');

		// Cross-locale hreflang on the <head>.
		expect(enHtml).toMatch(/rel="alternate"[^>]*hreflang="sv"/);
		expect(enHtml).toMatch(/rel="alternate"[^>]*hreflang="de"/);
		expect(svHtml).toMatch(/rel="alternate"[^>]*hreflang="en"/);
	});

});
