import { test, expect } from '../../fixtures/auth';
import { createSite, deleteSite, type Site } from '../../fixtures/data';

test.describe('Cmd+K palette', () => {
	let site: Site;

	test.afterAll(async ({ adminApi }) => {
		if (site) await deleteSite(adminApi, site.id);
	});

	test('ControlOrMeta+K opens palette, typing filters, Enter selects', async ({
		loggedInPage,
		adminApi
	}) => {
		site = await createSite(adminApi);
		await loggedInPage.goto('/');
		// Wait for the (app) layout to fully mount so the CommandPalette window keydown
		// listener is registered before we send the chord.
		await expect(loggedInPage.getByRole('heading', { level: 1 }).first()).toBeVisible({
			timeout: 10_000
		});
		// Click body to ensure focus is on document, not a stale element.
		await loggedInPage.locator('body').click({ position: { x: 5, y: 5 } });

		// CommandPalette listens on window keydown for meta/ctrl + K.
		await loggedInPage.keyboard.press('ControlOrMeta+K');
		// Palette input has aria-label "Command palette search".
		const input = loggedInPage.getByLabel(/command palette search/i);
		await expect(input).toBeVisible({ timeout: 5_000 });

		await input.fill(site.name);
		// Wait for the matching option to render.
		const option = loggedInPage.getByRole('option').filter({ hasText: site.name }).first();
		await expect(option).toBeVisible({ timeout: 5_000 });

		await loggedInPage.keyboard.press('Enter');
		await expect(loggedInPage).toHaveURL(new RegExp(`/sites/${site.id}(?!/)`), {
			timeout: 8_000
		});
	});
});
