import { test, expect, u } from '../../fixtures/auth';
import { createBlock, createPage, createSite, deleteSite, type Site } from '../../fixtures/data';

test.describe('Pages editor – editable per-block code', () => {
	let site: Site;

	test.afterAll(async ({ adminApi }) => {
		if (site) await deleteSite(adminApi, site.id);
	});

	test('editing a typed block source converts to raw_astro on save', async ({
		loggedInPage,
		adminApi
	}) => {
		site = await createSite(adminApi);
		const page = await createPage(adminApi, site.id);
		await createBlock(adminApi, site.id, page.id, {
			block_type: 'text',
			data: { content: 'Original copy' }
		});

		await loggedInPage.goto(`/sites/${site.id}/pages/${page.id}`);
		await expect(loggedInPage.getByText(/1 block\(s\)/i)).toBeVisible({ timeout: 10_000 });

		// Open the </> source panel for the block.
		await loggedInPage.getByRole('button', { name: /show source/i }).first().click();

		// Wait for the rendered Astro markup to load into the textarea.
		const textarea = loggedInPage.locator('textarea[spellcheck="false"]').first();
		await expect(textarea).toBeVisible({ timeout: 10_000 });
		const original = (await textarea.inputValue()).trim();
		expect(original.length).toBeGreaterThan(0);

		// Replace contents with custom Astro markup.
		const customMarkup = '<section class="custom-edit"><h2>Hand-edited</h2></section>';
		await textarea.fill(customMarkup);

		// Click Save inside the code panel. Typed-block conversion fires
		// a confirm dialog.
		await loggedInPage.getByRole('button', { name: /^Save$/, exact: true }).first().click();
		await loggedInPage.getByRole('button', { name: /^Convert$/, exact: true }).click();

		// Page-level save.
		const savedResponse = loggedInPage.waitForResponse((response) =>
			response.request().method() === 'PUT' &&
			new URL(response.url()).pathname === `/api/sites/${site.id}/pages/${page.id}/blocks/bulk`
		);
		await loggedInPage.getByRole('button', { name: /save changes/i }).click();
		expect((await savedResponse).status()).toBe(200);
		await expect(loggedInPage.getByRole('button', { name: 'Saved', exact: true })).toBeVisible();

		// Reload, verify the block_type is now raw_astro by checking the chip.
		await loggedInPage.reload();
		await expect(loggedInPage.getByText(/1 block\(s\)/i)).toBeVisible({ timeout: 10_000 });
		await expect(loggedInPage.getByText(/^RAW_ASTRO$/i).first()).toBeVisible();

		// Confirm on the API side too.
		const res = await adminApi.get(u(`/api/sites/${site.id}/pages/${page.id}/blocks`));
		const body = await res.json();
		const block = body.blocks?.[0];
		expect(block.block_type).toBe('raw_astro');
		const data = JSON.parse(block.data_json);
		expect(data.code).toContain('Hand-edited');
	});
});
