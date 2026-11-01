import { execSync } from 'node:child_process';
import { test, expect, u } from '../../fixtures/auth';
import {
	createSite,
	createPage,
	deleteSite,
	seedDeployTarget,
	triggerBuildAndWait,
	type Site
} from '../../fixtures/data';

// Save-time validation is DNS-free; the reserved .invalid destination cannot
// resolve to an SSH server. Exercise the request-time guard with a real build,
// preserving the production SSRF policy and making no private-address exception.

const hasRsync = (() => {
	try {
		execSync('command -v rsync', { stdio: 'ignore' });
		return true;
	} catch {
		return false;
	}
})();

test.describe('deploy: rsync kind', () => {
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

	test('rsync deploy refuses an unresolvable destination with a clean failure', async ({ adminApi }) => {
		test.skip(!hasRsync, 'rsync not on PATH');
		const target = await seedDeployTarget(adminApi, site.id, 'rsync');
		const build = await triggerBuildAndWait(adminApi, site.id, 180_000);
		expect(build.status).toBe('success');
		const res = await adminApi.post(u(`/api/sites/${site.id}/deploy`), {
			data: { build_id: build.build_id, target_id: target.id }
		});
		expect(res.status()).toBe(502);
		const body = await res.json();
		expect(String(body.error ?? '')).toMatch(/rsync.*not an allowed destination/i);
		expect(String(body.error ?? '')).toContain('rsync.e2e.invalid');
	});
});
