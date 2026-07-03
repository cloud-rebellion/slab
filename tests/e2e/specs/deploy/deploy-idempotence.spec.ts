import { existsSync } from 'node:fs';
import { test, expect, u } from '../../fixtures/auth';
import {
	createSite,
	createPage,
	deleteSite,
	rand,
	seedDeployTarget,
	triggerBuildAndWait,
	type Site
} from '../../fixtures/data';

// Deploy-idempotence asks: is re-running the same deploy a no-op (or at least
// safe + non-error)? The dockyard backend would be the cleaner case, but our
// http stub fails the https validator (see dockyard-deploy.spec.ts). Use the
// local backend instead -- LocalDeployer does an atomic-swap copy, and a
// second run on the same dist + path should overwrite cleanly with no error.

test.describe('deploy: idempotence (local backend)', () => {
	const cleanup: string[] = [];
	let site: Site;

	test.beforeAll(async ({ adminApi }) => {
		site = await createSite(adminApi, { siteType: 'b2b', structureType: 'one-pager' });
		cleanup.push(site.id);
		const page = await createPage(adminApi, site.id);
		await adminApi.patch(u(`/api/sites/${site.id}/pages/${page.id}`), {
			data: { status: 'published' }
		});
	});

	test.afterAll(async ({ adminApi }) => {
		for (const id of cleanup) await deleteSite(adminApi, id);
	});

	test('two back-to-back deploys against the same target both succeed', async ({ adminApi }) => {
		const destPath = `/tmp/${site.id}/atomicsite-deploy-idem-${rand()}`;
		const target = await seedDeployTarget(adminApi, site.id, 'local', {
			path: destPath,
			public_url: 'http://127.0.0.1:9990'
		});
		const build = await triggerBuildAndWait(adminApi, site.id, 180_000);
		expect(build.status, build.build_log).toBe('success');

		const first = await adminApi.post(u(`/api/sites/${site.id}/deploy`), {
			data: { build_id: build.build_id, target_id: target.id }
		});
		expect(first.ok(), `first deploy: ${first.status()} ${await first.text()}`).toBe(true);
		expect(existsSync(destPath)).toBe(true);

		const second = await adminApi.post(u(`/api/sites/${site.id}/deploy`), {
			data: { build_id: build.build_id, target_id: target.id }
		});
		expect(second.ok(), `second deploy: ${second.status()} ${await second.text()}`).toBe(true);

		const body = await second.json();
		expect(body.deploy_url).toBe('http://127.0.0.1:9990');
		expect(existsSync(destPath)).toBe(true);
	});
});
