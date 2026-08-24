import { test, expect, u } from '../../fixtures/auth';
import {
	createWorkspace,
	inviteToWorkspace,
	rand,
	redeemAsInvitee,
	type Workspace
} from '../../fixtures/data';

// Agency mode (2026-08-24): the workspace layer became reachable from the
// UI, so the gates under it had to hold for a NON-admin. Every negative
// assertion here runs as an invitee, never as `adminApi`: the seeded admin
// bypasses WorkspaceAccessMiddleware and RequireWorkspaceRole outright, so
// an admin-driven 403 assertion would be vacuous.

test.describe('agency mode: workspace gates', () => {
	test('a non-admin cannot create a workspace on a self-hosted instance', async ({ adminApi }) => {
		const ws = await createWorkspace(adminApi, { name: `Gate ${rand()}` });
		const inv = await inviteToWorkspace(adminApi, ws.id, { role: 'member' });
		const invitee = await redeemAsInvitee(inv.token);
		try {
			// The e2e server runs in single-deployment mode, where workspace
			// creation is an operator action.
			const res = await invitee.post(u('/api/workspaces'), { data: { name: 'Mine' } });
			expect(res.status(), await res.text()).toBe(403);
		} finally {
			await invitee.dispose();
		}
	});

	test('the plan field is not caller-controlled', async ({ adminApi }) => {
		// Unknown keys used to fall through billing.Lookup to oss = UNLIMITED,
		// so a typo granted more than the most expensive real plan.
		const bad = await adminApi.post(u('/api/workspaces'), {
			data: { name: `Typo ${rand()}`, plan: 'enterprise' }
		});
		expect(bad.status(), await bad.text()).toBe(400);

		// A real plan from a platform admin is still honoured.
		const ok = await createWorkspace(adminApi, { name: `Studio ${rand()}`, plan: 'studio' });
		expect(ok.plan).toBe('studio');
	});

	test('a workspace member sees their workspace sites and no others', async ({ adminApi }) => {
		const mine = await createWorkspace(adminApi, { name: `Mine ${rand()}` });
		const theirs = await createWorkspace(adminApi, { name: `Theirs ${rand()}` });

		const mySlug = rand('mine');
		const theirSlug = rand('theirs');
		const a = await adminApi.post(u('/api/sites'), {
			data: { name: 'Mine site', slug: mySlug, workspace_id: mine.id }
		});
		expect(a.status(), await a.text()).toBe(201);
		const b = await adminApi.post(u('/api/sites'), {
			data: { name: 'Their site', slug: theirSlug, workspace_id: theirs.id }
		});
		expect(b.status(), await b.text()).toBe(201);
		const theirSiteID = (await b.json()).id as string;

		const inv = await inviteToWorkspace(adminApi, mine.id, { role: 'member' });
		const invitee = await redeemAsInvitee(inv.token);
		try {
			// The headline fix: workspace membership alone puts the site on the
			// list. The invitee has no site_members row anywhere, and before
			// this the dashboard was empty while the site answered 200 by URL.
			const list = await invitee.get(u('/api/sites'));
			expect(list.status()).toBe(200);
			const slugs = ((await list.json()).sites as Array<{ slug: string }>).map((s) => s.slug);
			expect(slugs, 'workspace member must see their workspace site').toContain(mySlug);
			expect(slugs, 'list must not leak another workspace site').not.toContain(theirSlug);

			// And the list agrees with the gate: what is absent is also refused.
			const reach = await invitee.get(u(`/api/sites/${theirSiteID}`));
			expect(reach.status(), 'a site absent from the list must also be refused').toBe(403);
		} finally {
			await invitee.dispose();
		}
	});

	test('a workspace admin cannot mint an owner invite', async ({ adminApi }) => {
		const ws = await createWorkspace(adminApi, { name: `Mono ${rand()}` });
		const adminInv = await inviteToWorkspace(adminApi, ws.id, { role: 'admin' });
		const wsAdmin = await redeemAsInvitee(adminInv.token);
		try {
			// CreateInvite returns the raw token, so minting an owner invite
			// would be self-promotion for whoever holds the response.
			const escalate = await wsAdmin.post(u(`/api/workspaces/${ws.id}/invites`), {
				data: { email: `x-${rand()}@example.test`, role: 'owner' }
			});
			expect(escalate.status(), await escalate.text()).toBe(403);

			// At or below their own level is still allowed.
			const ok = await wsAdmin.post(u(`/api/workspaces/${ws.id}/invites`), {
				data: { email: `y-${rand()}@example.test`, role: 'member' }
			});
			expect(ok.status(), await ok.text()).toBe(201);
		} finally {
			await wsAdmin.dispose();
		}
	});

	test('PATCH preserves a billing email it was not given', async ({ adminApi }) => {
		const ws = await createWorkspace(adminApi, {
			name: `Billing ${rand()}`,
			billing_email: 'keep@example.test'
		});
		const patch = await adminApi.patch(u(`/api/workspaces/${ws.id}`), {
			data: { name: `${ws.name} renamed` }
		});
		expect(patch.status(), await patch.text()).toBe(200);
		expect((await patch.json()).billing_email).toBe('keep@example.test');
	});

	test('a deleted workspace disappears and stops answering for its members', async ({
		adminApi
	}) => {
		const ws = await createWorkspace(adminApi, { name: `Doomed ${rand()}` });
		const inv = await inviteToWorkspace(adminApi, ws.id, { role: 'member' });
		const invitee = await redeemAsInvitee(inv.token);
		try {
			expect((await invitee.get(u(`/api/workspaces/${ws.id}`))).status()).toBe(200);

			await adminApi.delete(u(`/api/workspaces/${ws.id}`));

			// Delete used to set status='deleted' and nothing ever read it, so
			// deleting a workspace revoked precisely nothing.
			const after = await invitee.get(u(`/api/workspaces/${ws.id}`));
			expect(after.status(), 'a deleted workspace must stop answering').toBe(404);

			const mine = await invitee.get(u('/api/workspaces'));
			const ids = ((await mine.json()).workspaces as Workspace[]).map((w) => w.id);
			expect(ids, 'a deleted workspace must leave the list').not.toContain(ws.id);
		} finally {
			await invitee.dispose();
		}
	});

	test('a reserved or malformed slug is refused on every create path', async ({ adminApi }) => {
		const ws = await createWorkspace(adminApi, { name: `Slugs ${rand()}` });
		// "dist" is a path component of EVERY tenant's served directory, which
		// is what made the local-deploy scope check spoofable.
		for (const slug of ['dist', '../../etc', 'srv']) {
			const res = await adminApi.post(u('/api/sites'), {
				data: { name: 'Nope', slug, workspace_id: ws.id }
			});
			expect(res.status(), `slug ${slug} should be refused`).toBe(400);
		}
	});

	test('seeding into a workspace you are not in is refused', async ({ adminApi }) => {
		const mine = await createWorkspace(adminApi, { name: `SeedMine ${rand()}` });
		const theirs = await createWorkspace(adminApi, { name: `SeedTheirs ${rand()}` });
		const inv = await inviteToWorkspace(adminApi, mine.id, { role: 'member' });
		const invitee = await redeemAsInvitee(inv.token);
		try {
			const res = await invitee.post(u('/api/sites/seed'), {
				data: {
					workspace_id: theirs.id,
					type: 'one-pager',
					info: { name: 'Nope', slug: rand('nope'), lang: 'en' },
					structure: { structure_type: 'one-pager', max_depth: 1 },
					silos: [],
					branding: {}
				}
			});
			expect(res.status(), await res.text()).toBe(400);
			expect(await res.text()).toContain('not a member');
		} finally {
			await invitee.dispose();
		}
	});
});
