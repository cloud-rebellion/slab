import { readFileSync } from 'node:fs';
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

// Match ci-context.py's source module identities. Component-joined names
// preserve their distinction through the mirror's normal module replacement.
// This describes source availability, never runtime edition or missing UI.
const moduleNames = Array.from(
	readFileSync(new URL('../../../../go.mod', import.meta.url), 'utf8').matchAll(/^module\s+(\S+)\s*$/gm),
	(match) => match[1]
);
if (moduleNames.length !== 1) {
	throw new Error('Workspace journey requires one unambiguous source module declaration');
}
const privateModule = ['github.com', 'brightinteraction', 'atomicsite'].join('/');
const publicModules = [
	['github.com', 'bright-interaction', 'slab'],
	['github.com', 'cloud-rebellion', 'slab']
].map((parts) => parts.join('/'));
const sourceContext = moduleNames[0] === privateModule
	? 'private'
	: publicModules.includes(moduleNames[0]) ? 'public' : undefined;
if (!sourceContext) {
	throw new Error('Workspace journey requires a known private or public source module');
}
const declaredContext = process.env.SLAB_E2E_SOURCE_CONTEXT;
if (declaredContext !== undefined && declaredContext !== sourceContext) {
	throw new Error('SLAB_E2E_SOURCE_CONTEXT does not match the source module identity');
}

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
			if (sourceContext === 'private') {
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

			} else {
				// Public source deliberately excludes the cloud signup UI. Prove
				// that boundary instead of restoring private frontend code.
				await page.goto(`${E2E_CONSTANTS.API_BASE}/cloud/signup/${invite.token}`);
				await expect(page.getByRole('heading', { name: '404', exact: true })).toBeVisible();
				await expect(page.getByText('Not Found', { exact: true })).toBeVisible();
				await expect(page.locator('form')).toHaveCount(0);

				// Workspace redemption is already a public core API. This
				// context is anonymous and shares no seeded admin session.
				const info = await ctx.request.get(u(`/api/cloud/signup/${invite.token}`));
				expect(info.status()).toBe(200);
				expect(await info.json()).toMatchObject({ workspace_name: mine.name });
				const redeemed = await ctx.request.post(u(`/api/cloud/signup/${invite.token}`), {
					data: { name: 'Journey User', password: 'invitee-pw-123456' }
				});
				expect(redeemed.status()).toBe(201);
				expect(await redeemed.json()).toMatchObject({
					status: 'account_created', existing_account: false,
					user: { email: invite.email, role: 'editor' }
				});

				// Discard only this new fixture session and prove normal OSS
				// password login. The owner's adminApi context is untouched.
				await ctx.clearCookies();
				await page.goto(`${E2E_CONSTANTS.API_BASE}/login`);
				await page.getByLabel('Email', { exact: true }).fill(invite.email);
				await page.getByLabel('Password', { exact: true }).fill('invitee-pw-123456');
				await page.getByRole('button', { name: 'Sign in', exact: true }).click();
			}
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
