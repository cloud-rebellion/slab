import { test, expect } from '../../fixtures/auth';
import { createSite, deleteSite, type Site, u } from '../../fixtures/data';

// 2026-05-22: version history + restore.
//
// Handler validation (cross-tenant, invalid entity_type, retention)
// is covered by internal/handlers/revisions_test.go. This spec
// verifies the API surface returns the documented JSON shapes and
// that the dashboard route renders the History entry point on the
// pages list.

test.describe('Revisions API + UI', () => {
	let site: Site;

	test.beforeAll(async ({ adminApi }) => {
		site = await createSite(adminApi);
	});

	test.afterAll(async ({ adminApi }) => {
		if (site) await deleteSite(adminApi, site.id);
	});

	test('empty entity returns empty revisions list', async ({ adminApi }) => {
		// Create a page; we have not edited it yet so no revisions exist.
		const create = await adminApi.post(u(`/api/sites/${site.id}/pages`), {
			data: { title: 'Revisions test', slug: 'rev-test', layout: 'default' }
		});
		expect(create.status()).toBe(201);
		const page = await create.json();

		const list = await adminApi.get(u(`/api/sites/${site.id}/revisions/page/${page.id}`));
		expect(list.status()).toBe(200);
		const body = await list.json();
		expect(Array.isArray(body.revisions)).toBe(true);
		expect(body.count).toBe(0);
	});

	test('page edit creates a revision and restore reverts the page', async ({ adminApi }) => {
		// Create page, then PATCH twice to seed history.
		const created = await adminApi.post(u(`/api/sites/${site.id}/pages`), {
			data: { title: 'Original', slug: 'restore-flow', layout: 'default' }
		});
		expect(created.status()).toBe(201);
		const page = await created.json();

		const patch1 = await adminApi.patch(u(`/api/sites/${site.id}/pages/${page.id}`), {
			data: { title: 'Edited Once' }
		});
		expect(patch1.status()).toBe(200);

		// Revision row exists capturing the original (pre-edit) state.
		const list1 = await adminApi.get(u(`/api/sites/${site.id}/revisions/page/${page.id}`));
		const body1 = await list1.json();
		expect(body1.count).toBe(1);
		expect(body1.revisions[0].version_number).toBe(1);
		expect(body1.revisions[0].snapshot_json).toContain('Original');

		// Second edit creates a second revision (the post-first-edit state).
		const patch2 = await adminApi.patch(u(`/api/sites/${site.id}/pages/${page.id}`), {
			data: { title: 'Edited Twice' }
		});
		expect(patch2.status()).toBe(200);

		const list2 = await adminApi.get(u(`/api/sites/${site.id}/revisions/page/${page.id}`));
		const body2 = await list2.json();
		expect(body2.count).toBe(2);

		// Restore v1 (original). The handler snapshots the current state
		// first (so the restore is itself undoable) then applies v1's
		// snapshot via UpdatePage, leaving the page titled "Original".
		const restore = await adminApi.post(
			u(`/api/sites/${site.id}/revisions/page/${page.id}/1/restore`),
			{ data: {} }
		);
		expect(restore.status()).toBe(200);
		const restored = await restore.json();
		expect(restored.entity_type).toBe('page');
		expect(restored.entity.title).toBe('Original');

		// History grew by one (pre-restore snapshot of v2's "Edited Twice").
		const list3 = await adminApi.get(u(`/api/sites/${site.id}/revisions/page/${page.id}`));
		const body3 = await list3.json();
		expect(body3.count).toBe(3);
	});

	test('history button visible on pages list row', async ({ loggedInPage }) => {
		await loggedInPage.goto(`/sites/${site.id}/pages`);
		// The History icon button is rendered with aria-label="Version history".
		const btn = loggedInPage.getByRole('button', { name: 'Version history' }).first();
		await expect(btn).toBeAttached({ timeout: 5_000 });
	});

	test('invalid entity_type returns 400', async ({ adminApi }) => {
		const resp = await adminApi.get(
			u(`/api/sites/${site.id}/revisions/site_branding/anything`)
		);
		expect(resp.status()).toBe(400);
	});
});
