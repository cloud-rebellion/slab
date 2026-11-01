import { test, expect } from '../../fixtures/auth';
import { createPage, createSite, deleteSite, type Site } from '../../fixtures/data';

test.describe('Block editor', () => {
	let site: Site;

	test.afterAll(async ({ adminApi }) => {
		if (site) await deleteSite(adminApi, site.id);
	});

	test('add a text block, save, content persists after reload', async ({
		loggedInPage,
		adminApi
	}) => {
		site = await createSite(adminApi);
		const page = await createPage(adminApi, site.id);

		await loggedInPage.goto(`/sites/${site.id}/pages/${page.id}`);
		await expect(loggedInPage.getByRole('heading', { name: page.title })).toBeVisible();

		// Open block type picker (label "Add block"). The popover renders in a
		// portal - wait for the "Choose block type" header before clicking.
		await loggedInPage.getByRole('button', { name: 'Add block', exact: true }).first().click();
		await expect(loggedInPage.getByText(/choose block type/i)).toBeVisible({ timeout: 5_000 });
		await loggedInPage.getByRole('button', { name: 'Text', exact: true }).click();

		// The block expands; save the page (button "Save changes").
		const savedResponse = loggedInPage.waitForResponse((response) =>
			response.request().method() === 'PUT' &&
			new URL(response.url()).pathname === `/api/sites/${site.id}/pages/${page.id}/blocks/bulk`
		);
		await loggedInPage.getByRole('button', { name: /save changes/i }).click();
		expect((await savedResponse).status()).toBe(200);
		// After save the button label flips back to "Saved".
		await expect(loggedInPage.getByRole('button', { name: 'Saved', exact: true })).toBeVisible();

		// Reload, the text block should still be there (one block in the list).
		await loggedInPage.reload();
		// "1 block(s)" line.
		await expect(loggedInPage.getByText(/1 block\(s\)/i)).toBeVisible({ timeout: 10_000 });
	});
});
