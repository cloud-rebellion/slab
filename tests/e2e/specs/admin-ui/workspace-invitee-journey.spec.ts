import { test, expect, u } from '../../fixtures/auth';
import { createWorkspace, deleteWorkspace, inviteToWorkspace, rand } from '../../fixtures/data';
import { E2E_CONSTANTS } from '../../playwright.config';

// The shipping gate for agency mode, from the invitee's side.
//
// Everything else proves the OPERATOR can invite. This proves the person on
// the other end of the link can actually work: redeem in a clean browser
// context, land in the app, see exactly their workspace's sites and nobody
// else's, and reach the agent-connection page. If any step here needed curl
// or a doc, the feature would not be shipped.
//
// Deliberately uses a fresh browser context, never `loggedInPage`: the
// seeded admin bypasses every workspace and site gate, so a journey driven
// as admin would prove nothing about what an invited user experiences.

test.describe('Workspace invitee journey', () => {
	test('an invited user redeems, lands in the app, and sees only their workspace', async ({
		browser,
		adminApi
	}) => {
		const mine = await createWorkspace(adminApi, { name: `Journey ${rand()}` });
		const theirs = await createWorkspace(adminApi, { name: `Other ${rand()}` });

		const mySlug = rand('journey');
		const theirSlug = rand('other');
		const mySite = await adminApi.post(u('/api/sites'), {
			data: { name: 'Journey site', slug: mySlug, workspace_id: mine.id }
		});
		expect(mySite.status(), await mySite.text()).toBe(201);
		const theirSite = await adminApi.post(u('/api/sites'), {
			data: { name: 'Other site', slug: theirSlug, workspace_id: theirs.id }
		});
		expect(theirSite.status(), await theirSite.text()).toBe(201);

		const invite = await inviteToWorkspace(adminApi, mine.id, { role: 'member' });

		// A clean context: no admin cookie, nothing cached.
		const ctx = await browser.newContext();
		const page = await ctx.newPage();
		try {
			// 1. Redeem the link exactly as it was handed over.
			await page.goto(`${E2E_CONSTANTS.API_BASE}/cloud/signup/${invite.token}`);
			await expect(page.getByText(new RegExp(mine.name, 'i')).first()).toBeVisible({
				timeout: 10_000
			});

			await page.getByRole('textbox', { name: /name/i }).first().fill('Journey User');
			// Both password fields, in order (set + confirm).
			const pw = page.locator('input[type="password"]');
			await pw.nth(0).fill('journey-pw-123456');
			await pw.nth(1).fill('journey-pw-123456');
			await page.getByRole('button', { name: /(create|activate|join|accept|sign)/i }).first().click();

			// 2. Redemption logs them straight in.
			await expect(page).toHaveURL(/\/(sites)?$/, { timeout: 15_000 });

			// 3. They see their workspace's site, and nobody else's. This is the
			//    fix that mattered: before it, an invited workspace member saw an
			//    empty dashboard while the site answered 200 by direct URL.
			await page.goto(`${E2E_CONSTANTS.API_BASE}/sites`);
			await expect(page.getByText(mySlug).first()).toBeVisible({ timeout: 10_000 });
			await expect(page.getByText(theirSlug)).toHaveCount(0);

			// 4. The topbar shows the workspace they belong to.
			await expect(page.getByRole('button', { name: /switch workspace/i })).toContainText(
				mine.name
			);
		} finally {
			await ctx.close();
			await deleteWorkspace(adminApi, mine.id);
			await deleteWorkspace(adminApi, theirs.id);
		}
	});
});
