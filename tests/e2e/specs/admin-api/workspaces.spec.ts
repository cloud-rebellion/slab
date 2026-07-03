import { test, expect, u } from '../../fixtures/auth';
import { request } from '@playwright/test';

// Phase 30.1: workspace CRUD + cross-workspace access enforcement.
// The OSS bootstrap auto-creates a "default" workspace at boot;
// admin can list/create/update/delete workspaces and add members.
//
// Admin (the seeded user) has user.role='admin' which the
// WorkspaceAccessMiddleware bypasses, so admin can read any
// workspace. The cross-workspace leakage tests use a separate
// non-admin invitee added to one workspace and verify they 403
// when reaching for the other.

test.describe('Phase 30.1: workspaces', () => {
	test('default workspace exists after boot', async ({ adminApi }) => {
		const res = await adminApi.get(u('/api/workspaces'));
		expect(res.ok()).toBeTruthy();
		const body = await res.json();
		const list = body.workspaces as Array<{ slug: string; plan: string }>;
		expect(list.length).toBeGreaterThanOrEqual(1);
		const def = list.find((w) => w.slug === 'default');
		expect(def, 'default workspace should be auto-bootstrapped').toBeTruthy();
		expect(def!.plan).toBe('oss');
	});

	test('create + read + patch + delete workspace flow', async ({ adminApi }) => {
		const name = `Test ws ${Date.now().toString(36)}`;
		const create = await adminApi.post(u('/api/workspaces'), {
			data: { name, plan: 'studio', billing_email: 'billing@example.test' }
		});
		expect(create.status()).toBe(201);
		const created = await create.json();
		expect(created.id).toMatch(/^[a-f0-9]{24}$/);
		expect(created.role).toBe('owner');
		expect(created.plan).toBe('studio');

		const get = await adminApi.get(u(`/api/workspaces/${created.id}`));
		expect(get.ok()).toBeTruthy();
		const got = await get.json();
		expect(got.name).toBe(name);

		const patch = await adminApi.patch(u(`/api/workspaces/${created.id}`), {
			data: { name: name + ' renamed', billing_email: 'new@example.test' }
		});
		expect(patch.ok()).toBeTruthy();
		const patched = await patch.json();
		expect(patched.name).toBe(name + ' renamed');
		expect(patched.billing_email).toBe('new@example.test');

		const del = await adminApi.delete(u(`/api/workspaces/${created.id}`));
		expect(del.ok()).toBeTruthy();
	});

	test('invalid workspaceID returns 400 not 404', async ({ adminApi }) => {
		const res = await adminApi.get(u('/api/workspaces/not-a-valid-id'));
		expect(res.status()).toBe(400);
	});

	test('create workspace rejects bad slug', async ({ adminApi }) => {
		const res = await adminApi.post(u('/api/workspaces'), {
			data: { name: 'X', slug: 'A_B_C' }
		});
		expect(res.status()).toBe(400);
	});

	test('cross-workspace read leakage is blocked for non-admin members', async ({ adminApi }) => {
		// Admin has the platform-admin bypass, so to exercise the membership
		// gate we mint two workspaces, invite a brand-new user into ONE of them
		// via the /cloud/signup redeem flow (which creates an editor + that one
		// workspace membership + a session), then confirm the invitee can read
		// their own workspace (200) but is 403'd on the other.
		const stamp = Date.now().toString(36);
		const wsA = await adminApi.post(u('/api/workspaces'), { data: { name: `WS A ${stamp}` } });
		expect(wsA.status()).toBe(201);
		const wsAID = (await wsA.json()).id as string;
		const wsB = await adminApi.post(u('/api/workspaces'), { data: { name: `WS B ${stamp}` } });
		expect(wsB.status()).toBe(201);
		const wsBID = (await wsB.json()).id as string;

		// Invite a fresh email into WS A (admin creates the invite as its owner).
		const email = `wsmember-${stamp}@example.test`;
		const inv = await adminApi.post(u(`/api/workspaces/${wsAID}/invites`), {
			data: { email, role: 'member' }
		});
		expect(inv.status()).toBe(201);
		const token = (await inv.json()).token as string;
		expect(token).toBeTruthy();

		// Redeem as the invitee in a FRESH context (its own cookie jar). A new
		// email creates the editor account + WS A membership and returns a
		// session cookie stored on this context.
		const invitee = await request.newContext();
		try {
			const redeem = await invitee.post(u(`/api/cloud/signup/${token}`), {
				data: { name: 'WS Member', password: 'invitee-pw-123456' }
			});
			expect(redeem.status(), await redeem.text()).toBe(201);

			// The invitee can read WS A (member) ...
			const readA = await invitee.get(u(`/api/workspaces/${wsAID}`));
			expect(readA.status(), 'invitee should read their own workspace').toBe(200);

			// ... but is refused WS B (not a member, and not a platform admin).
			const readB = await invitee.get(u(`/api/workspaces/${wsBID}`));
			expect(readB.status(), 'invitee must NOT read a workspace they are not in').toBe(403);
		} finally {
			await invitee.dispose();
		}
	});
});
