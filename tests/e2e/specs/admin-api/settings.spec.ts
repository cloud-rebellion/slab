import { test, expect, u } from '../../fixtures/auth';
import { createSite, deleteSite, rand, type Site } from '../../fixtures/data';

test.describe('admin-api: settings CRUD', () => {
	const cleanup: string[] = [];
	let site: Site;

	test.beforeAll(async ({ adminApi }) => {
		site = await createSite(adminApi);
		cleanup.push(site.id);
	});

	test.afterAll(async ({ adminApi }) => {
		for (const id of cleanup) await deleteSite(adminApi, id);
	});

	test('GET lists all settings (seeded with security/server defaults)', async ({ adminApi }) => {
		const res = await adminApi.get(u(`/api/sites/${site.id}/settings`));
		expect(res.ok()).toBe(true);
		const list = await res.json();
		expect(Array.isArray(list)).toBe(true);
		expect(list.length).toBeGreaterThan(0);
		expect(list.find((s: { category: string; key: string }) => s.category === 'security' && s.key === 'csp_enabled')).toBeTruthy();
	});

	test('GET /{category} filters by category', async ({ adminApi }) => {
		const res = await adminApi.get(u(`/api/sites/${site.id}/settings/security`));
		expect(res.ok()).toBe(true);
		const list = await res.json();
		expect(Array.isArray(list)).toBe(true);
		expect(list.every((s: { category: string }) => s.category === 'security')).toBe(true);
	});

	test('PUT upserts a single setting', async ({ adminApi }) => {
		const key = rand('feat');
		const res = await adminApi.put(u(`/api/sites/${site.id}/settings`), {
			data: { category: 'general', key, value: 'on' }
		});
		expect(res.ok()).toBe(true);
		const setting = await res.json();
		expect(setting.key).toBe(key);
		expect(setting.value).toBe('on');

		// Update by re-upserting
		const res2 = await adminApi.put(u(`/api/sites/${site.id}/settings`), {
			data: { category: 'general', key, value: 'off' }
		});
		expect(res2.ok()).toBe(true);
		const updated = await res2.json();
		expect(updated.value).toBe('off');
	});

	test('PUT /bulk upserts multiple settings', async ({ adminApi }) => {
		const a = rand('a');
		const b = rand('b');
		const res = await adminApi.put(u(`/api/sites/${site.id}/settings/bulk`), {
			data: [
				{ category: 'general', key: a, value: '1' },
				{ category: 'general', key: b, value: '2' }
			]
		});
		expect(res.ok()).toBe(true);
		// Response shape matches the agent + MCP bulk paths: the settings
		// list plus the entries the caller was not allowed to write.
		const body = await res.json();
		const list = body.settings as Array<{ key: string }>;
		expect(Array.isArray(list)).toBe(true);
		expect(list.find((s) => s.key === a)).toBeTruthy();
		expect(list.find((s) => s.key === b)).toBeTruthy();
		expect(body.rejected_admin_only).toEqual([]);
	});

	test('DELETE removes a setting', async ({ adminApi }) => {
		const key = rand('rm');
		const create = await adminApi.put(u(`/api/sites/${site.id}/settings`), {
			data: { category: 'general', key, value: 'temp' }
		});
		const setting = await create.json();
		const res = await adminApi.delete(u(`/api/sites/${site.id}/settings/${setting.id}`));
		expect(res.ok()).toBe(true);
		const body = await res.json();
		expect(body.status).toBe('deleted');
	});
});
