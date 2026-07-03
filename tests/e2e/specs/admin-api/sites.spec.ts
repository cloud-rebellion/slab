import { test, expect, u } from '../../fixtures/auth';
import {
	createSite,
	deleteSite,
	listSites,
	rand,
	type Site
} from '../../fixtures/data';

test.describe('admin-api: sites CRUD', () => {
	const cleanup: string[] = [];

	test.afterAll(async ({ adminApi }) => {
		for (const id of cleanup) await deleteSite(adminApi, id);
	});

	test('POST /api/sites/seed creates a site transactionally', async ({ adminApi }) => {
		const site = await createSite(adminApi, { siteType: 'b2b', structureType: 'one-pager' });
		cleanup.push(site.id);
		expect(site.id).toBeTruthy();
		expect(site.slug).toBeTruthy();
		expect(site.name).toBeTruthy();
	});

	test('POST /api/sites creates a blank site', async ({ adminApi }) => {
		const site = await createSite(adminApi, { withSeed: false });
		cleanup.push(site.id);
		expect(site.id).toBeTruthy();
	});

	test('GET /api/sites lists all sites for admin', async ({ adminApi }) => {
		const a = await createSite(adminApi);
		cleanup.push(a.id);
		const sites = await listSites(adminApi);
		expect(sites.find((s: Site) => s.id === a.id)).toBeTruthy();
	});

	test('GET /api/sites/{id} returns one site', async ({ adminApi }) => {
		const site = await createSite(adminApi);
		cleanup.push(site.id);
		const res = await adminApi.get(u(`/api/sites/${site.id}`));
		expect(res.ok()).toBe(true);
		const body = await res.json();
		const s = body.site ?? body;
		expect(s.id).toBe(site.id);
	});

	test('PATCH /api/sites/{id} updates name + branding', async ({ adminApi }) => {
		const site = await createSite(adminApi);
		cleanup.push(site.id);
		const newName = `Renamed ${rand()}`;
		const res = await adminApi.patch(u(`/api/sites/${site.id}`), {
			data: { name: newName, primary_color: '#ff0000' }
		});
		expect(res.ok()).toBe(true);
		const get = await adminApi.get(u(`/api/sites/${site.id}`));
		const body = await get.json();
		const s = body.site ?? body;
		expect(s.name).toBe(newName);
	});

	test('DELETE /api/sites/{id} removes the site', async ({ adminApi }) => {
		const site = await createSite(adminApi);
		const res = await adminApi.delete(u(`/api/sites/${site.id}`));
		expect(res.ok()).toBe(true);
		const get = await adminApi.get(u(`/api/sites/${site.id}`));
		// A GET of a just-deleted site is refused by SiteAccessMiddleware with
		// 403 (it will not leak whether the site ever existed) before the
		// handler runs; accept it alongside the legacy 404/401.
		expect([403, 404, 401]).toContain(get.status());
	});

	test('duplicate slug returns 409', async ({ adminApi }) => {
		const slug = rand('dupe');
		const a = await createSite(adminApi, { slug });
		cleanup.push(a.id);
		const res = await adminApi.post(u('/api/sites/seed'), {
			data: {
				site_type: 'b2b',
				info: {
					name: 'Dup',
					slug,
					domain: `${slug}-2.e2e.test`,
					lang: 'en',
					business_name: 'Dup AB',
					contact_email: 'dup@example.test',
					country: 'SE'
				},
				structure: { structure_type: 'one-pager', max_depth: 3 },
				silos: [],
				branding: {
					primary_color: '#000000',
					secondary_color: '#111111',
					bg_color: '#ffffff',
					text_color: '#000000',
					font_heading: 'Inter',
					font_body: 'Inter'
				}
			}
		});
		expect(res.status()).toBe(409);
	});

	test('GET /api/sites/{id}/silos returns silo list', async ({ adminApi }) => {
		const site = await createSite(adminApi, { structureType: 'soft-silo' });
		cleanup.push(site.id);
		const res = await adminApi.get(u(`/api/sites/${site.id}/silos`));
		expect(res.ok()).toBe(true);
		const body = await res.json();
		const list = body.silos ?? body;
		expect(Array.isArray(list)).toBe(true);
	});
});
