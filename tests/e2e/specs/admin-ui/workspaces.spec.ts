import { test, expect } from '../../fixtures/auth';
import { createWorkspace, deleteWorkspace, rand, type Workspace } from '../../fixtures/data';

// The definition-of-done spec for agency mode: an operator must be able to
// create a workspace and invite someone into it through the UI alone. The
// invite half deliberately makes NO API call for the thing under test: if
// this spec could only pass by curling the endpoint, the feature would not
// be shipped.

test.describe('Workspaces UI', () => {
	const created: Workspace[] = [];

	test.afterAll(async ({ adminApi }) => {
		for (const w of created) await deleteWorkspace(adminApi, w.id);
	});

	test('create a workspace through the UI', async ({ loggedInPage, adminApi }) => {
		const name = `UI WS ${rand()}`;
		await loggedInPage.goto('/workspaces');

		await loggedInPage.getByRole('button', { name: /new workspace/i }).first().click();
		await loggedInPage.getByLabel(/name/i).fill(name);
		await loggedInPage.getByRole('button', { name: /^create workspace$/i }).click();

		// Landing on the detail page is what tells the operator it worked.
		await expect(loggedInPage).toHaveURL(/\/workspaces\/[a-f0-9]{24}/, { timeout: 8_000 });
		await expect(loggedInPage.getByRole('heading', { name })).toBeVisible();

		// Register for cleanup by resolving the id from the API afterwards.
		const list = await adminApi.get(
			(await import('../../fixtures/auth')).u('/api/workspaces')
		);
		const found = ((await list.json()).workspaces as Workspace[]).find((w) => w.name === name);
		if (found) created.push(found);
	});

	test('invite someone to a workspace and see the one-time link', async ({
		loggedInPage,
		adminApi
	}) => {
		const ws = await createWorkspace(adminApi, { name: `Invite WS ${rand()}` });
		created.push(ws);
		const email = `${rand('ui-invitee')}@example.test`;

		await loggedInPage.goto(`/workspaces/${ws.id}`);
		await loggedInPage.getByRole('button', { name: /invite to workspace/i }).click();
		// Exact: the owner settings form on the same page has a "Billing email"
		// field, so a loose /email/i match is ambiguous.
		await loggedInPage.getByRole('textbox', { name: 'Email', exact: true }).fill(email);
		await loggedInPage.getByRole('button', { name: /^create invite$/i }).click();

		// The redeemable link is shown exactly once, here. It is never on the
		// pending list (the API omits the token by design), which is why the
		// panel exists at all.
		await expect(loggedInPage.getByText(/\/cloud\/signup\//)).toBeVisible({ timeout: 8_000 });
		await loggedInPage.getByRole('button', { name: /^done$/i }).click();

		// The invite then shows as pending, addressed to that person.
		await expect(loggedInPage.getByText(email).first()).toBeVisible();
	});

	test('switch workspace from the topbar', async ({ loggedInPage, adminApi }) => {
		const a = await createWorkspace(adminApi, { name: `Switch A ${rand()}` });
		const b = await createWorkspace(adminApi, { name: `Switch B ${rand()}` });
		created.push(a, b);

		await loggedInPage.goto('/workspaces');
		// Select A from the list so it becomes the active workspace.
		await loggedInPage.getByText(a.name).first().click();
		await expect(loggedInPage).toHaveURL(new RegExp(`/workspaces/${a.id}`), { timeout: 8_000 });

		// The topbar switcher is labelled by the active workspace.
		await loggedInPage.getByRole('button', { name: /switch workspace/i }).click();
		await loggedInPage.getByRole('button').filter({ hasText: b.name }).first().click();

		// Switching invalidates the current site, so it lands on the site list.
		await expect(loggedInPage).toHaveURL(/\/sites$/, { timeout: 8_000 });
	});
});
