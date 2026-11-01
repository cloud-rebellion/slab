import { test, expect, u } from '../../fixtures/auth';
import {
	createSite,
	createPage,
	deleteSite,
	getStubCalls,
	resetStub,
	seedDeployTarget,
	triggerBuildAndWait,
	type Site
} from '../../fixtures/data';

// Dockyard requires HTTPS metadata and an allowed SSH destination. The reserved
// .invalid target exercises the guarded deployment-failure path without dialing
// internal infrastructure. Unit tests cover the owned HTTPS round-trip.
import { execSync } from 'node:child_process';

const hasRsync = (() => {
	try {
		execSync('command -v rsync', { stdio: 'ignore' });
		return true;
	} catch {
		return false;
	}
})();

test.describe('deploy: dockyard kind', () => {
	const cleanup: string[] = [];
	let site: Site;

	test.beforeAll(async ({ adminApi }) => {
		site = await createSite(adminApi, { siteType: 'b2b', structureType: 'one-pager' });
		cleanup.push(site.id);
		await createPage(adminApi, site.id);
	});

	test.afterAll(async ({ adminApi }) => {
		for (const id of cleanup) await deleteSite(adminApi, id);
	});

	test('http dockyard_url is rejected at target creation', async ({ adminApi }) => {
		await resetStub('dockyard');
		const res = await adminApi.post(u(`/api/sites/${site.id}/deploy-targets`), {
			data: {
				name: 'dy-http',
				kind: 'dockyard',
				config: {
					dockyard_url: 'http://127.0.0.1:18293',
					api_key: 'e2e-dy-key',
					server_id: '11111111-2222-3333-4444-555555555555',
					domain: 'e2e.example.test',
					host: '127.0.0.1',
					user: 'deploy',
					port: 22022,
					host_path: '/srv/sites/e2e',
					private_key_pem:
						'-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n-----END OPENSSH PRIVATE KEY-----\n'
				},
				is_default: false
			}
		});
		expect(res.status()).toBe(400);
		const body = await res.json();
		expect(String(body.error ?? '')).toMatch(/https/);

		// Validator rejected before we hit the network: stub recorded zero calls.
		const calls = await getStubCalls('dockyard');
		expect(calls).toHaveLength(0);
	});

	test('dockyard deploy refuses an unresolvable SSH destination before contacting Dockyard', async ({
		adminApi
	}) => {
		test.skip(!hasRsync, 'rsync not on PATH');
		await resetStub('dockyard');
		// Offline HTTPS/config validation passes; the resolving SSH guard must refuse deployment.
		const target = await seedDeployTarget(adminApi, site.id, 'dockyard');
		const build = await triggerBuildAndWait(adminApi, site.id, 180_000);
		expect(build.status).toBe('success');
		const res = await adminApi.post(u(`/api/sites/${site.id}/deploy`), {
			data: { build_id: build.build_id, target_id: target.id }
		});
		// The unchanged request-time destination guard refuses before rsync or TLS I/O.
		expect(res.status()).toBe(502);
		const body = await res.json();
		expect(String(body.error ?? '')).toMatch(/rsync.*not an allowed destination/i);
		expect(String(body.error ?? '')).toContain('rsync.e2e.invalid');
		expect(await getStubCalls('dockyard')).toHaveLength(0);
	});
});
