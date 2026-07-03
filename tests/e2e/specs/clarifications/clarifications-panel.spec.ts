import { test, expect } from '../../fixtures/auth';
import { createSite, deleteSite, type Site, u } from '../../fixtures/data';

// 2026-05-21: clarifications inbox UI.
//
// Handler validation (resolve gate, 409 on already-terminal, cancel
// permissions) is covered by internal/handlers/clarifications_test.go.
// This spec only verifies the UI renders the empty state and that a
// pending row surfaces the Resolve + Cancel actions.

test.describe('Clarifications panel', () => {
	let site: Site;

	test.beforeAll(async ({ adminApi }) => {
		site = await createSite(adminApi);
	});

	test.afterAll(async ({ adminApi }) => {
		if (site) await deleteSite(adminApi, site.id);
	});

	test('empty inbox renders empty state', async ({ adminApi, loggedInPage }) => {
		await loggedInPage.goto(`/sites/${site.id}/clarifications`);
		await expect(
			loggedInPage.getByText('No pending clarifications')
		).toBeVisible({ timeout: 5_000 });

		const list = await adminApi.get(u(`/api/sites/${site.id}/clarifications`));
		expect(list.status()).toBe(200);
		const body = await list.json();
		expect(Array.isArray(body.clarifications)).toBe(true);
		expect(body.clarifications.length).toBe(0);
		expect(body.count).toBe(0);
	});

	test('pending row surfaces Resolve and Cancel actions', async ({
		adminApi,
		loggedInPage
	}) => {
		const create = await adminApi.post(u(`/api/sites/${site.id}/clarifications`), {
			data: {
				question: 'Formal or casual voice for the hero?',
				context: 'Existing copy reads casual; brand book leans formal.',
				options: ['Formal', 'Casual']
			}
		});
		expect(create.status()).toBe(201);

		await loggedInPage.goto(`/sites/${site.id}/clarifications`);

		await expect(
			loggedInPage.getByText('Formal or casual voice for the hero?')
		).toBeVisible({ timeout: 5_000 });
		await expect(loggedInPage.getByRole('button', { name: 'Resolve' })).toBeVisible();
		await expect(loggedInPage.getByRole('button', { name: 'Cancel' })).toBeVisible();
	});
});
