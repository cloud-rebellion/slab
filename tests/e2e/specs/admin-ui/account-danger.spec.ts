import { test, expect } from '../../fixtures/auth';
import { createSite, type Site, u } from '../../fixtures/data';

test.describe('Account: Danger zone', () => {
	test('lists every site and gates deletion behind slug confirmation', async ({
		loggedInPage,
		adminApi
	}) => {
		const site: Site = await createSite(adminApi);

		await loggedInPage.goto('/account');
		await expect(
			loggedInPage.getByRole('heading', { name: 'Account settings', exact: true })
		).toBeVisible();

		// Danger zone heading lives at /account, not in /settings/danger.
		await expect(
			loggedInPage.getByRole('heading', { name: 'Danger zone', exact: true })
		).toBeVisible();

		// The newly created site should appear in the danger zone list.
		const row = loggedInPage.locator('li', { hasText: site.name }).first();
		await expect(row).toBeVisible();

		// Clicking Delete site opens the slug-confirm dialog.
		await row.getByRole('button', { name: 'Delete site', exact: true }).click();

		const confirmBtn = loggedInPage.getByRole('button', { name: 'Delete forever', exact: true });
		await expect(confirmBtn).toBeDisabled();

		const slugInput = loggedInPage.getByLabel(`Type "${site.slug}" to confirm`);
		await slugInput.fill(site.slug);
		await expect(confirmBtn).toBeEnabled();
		await confirmBtn.click();

		// Site GET stops returning 200 once the dialog resolves. The site-access
		// middleware refuses a now-deleted site with 403 (it will not leak whether
		// the site ever existed) before the handler runs, so accept 403 or 404.
		await expect(async () => {
			const res = await adminApi.get(u(`/api/sites/${site.id}`));
			expect([403, 404]).toContain(res.status());
		}).toPass({ timeout: 5_000 });
	});

	test('the old per-site /settings/danger route is gone', async ({ loggedInPage, adminApi }) => {
		const site: Site = await createSite(adminApi);
		await loggedInPage.goto(`/sites/${site.id}/settings/danger`, {
			waitUntil: 'domcontentloaded'
		});
		// SvelteKit renders a 404 for missing routes. The Danger zone heading
		// must NOT appear there anymore.
		await expect(
			loggedInPage.getByRole('heading', { name: 'Danger zone', exact: true })
		).toHaveCount(0);

		// The settings landing page surfaces a "Delete this site" pointer to
		// /account so users land in the right place.
		await loggedInPage.goto(`/sites/${site.id}/settings`);
		await expect(loggedInPage.getByRole('link', { name: 'Account settings' })).toBeVisible();

		// Cleanup.
		await adminApi.delete(u(`/api/sites/${site.id}`));
	});
});
