import { existsSync, readdirSync, statSync } from 'node:fs';
import path from 'node:path';
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

test.describe('deploy: local kind copies dist to target path', () => {
	const cleanup: string[] = [];
	let site: Site;

	test.beforeAll(async ({ adminApi }) => {
		site = await createSite(adminApi, { siteType: 'b2b', structureType: 'one-pager' });
		cleanup.push(site.id);
		const page = await createPage(adminApi, site.id);
		// Pages default to draft; renderer requires at least one published page.
		await adminApi.patch(u(`/api/sites/${site.id}/pages/${page.id}`), {
			data: { status: 'published' }
		});
	});

	test.afterAll(async ({ adminApi }) => {
		for (const id of cleanup) await deleteSite(adminApi, id);
	});

	test('build + local deploy lands dist files at the configured path', async ({ adminApi }) => {
		const destPath = `/tmp/${site.id}/atomicsite-deploy-${rand()}`;
		const target = await seedDeployTarget(adminApi, site.id, 'local', {
			path: destPath,
			public_url: 'http://127.0.0.1:9999'
		});
		expect(target.id).toBeTruthy();
		expect(target.kind).toBe('local');

		const build = await triggerBuildAndWait(adminApi, site.id, 180_000);
		expect(build.status, `build error: ${build.error ?? '(no error)'}`).toBe('success');

		const res = await adminApi.post(u(`/api/sites/${site.id}/deploy`), {
			data: { build_id: build.build_id, target_id: target.id }
		});
		expect(res.ok(), `deploy failed: ${res.status()} ${await res.text()}`).toBe(true);
		const body = await res.json();
		expect(body.deployment_id).toBe(build.build_id);
		expect(body.deploy_url).toBe('http://127.0.0.1:9999');
		expect(typeof body.file_count).toBe('number');
		expect(body.file_count).toBeGreaterThan(0);

		// Atomic-swap copy should land the dist tree under destPath. We expect
		// at least one index.html somewhere (Astro emits one per page) plus
		// supporting files like robots.txt + sitemap.xml.
		expect(existsSync(destPath)).toBe(true);
		const entries = readdirSync(destPath);
		expect(entries.length).toBeGreaterThan(0);

		const everyHTMLFile: string[] = [];
		const walk = (dir: string) => {
			for (const e of readdirSync(dir)) {
				const full = path.join(dir, e);
				if (statSync(full).isDirectory()) walk(full);
				else if (e === 'index.html') everyHTMLFile.push(full);
			}
		};
		walk(destPath);
		expect(everyHTMLFile.length, `no index.html under ${destPath}`).toBeGreaterThan(0);
		expect(existsSync(path.join(destPath, 'robots.txt'))).toBe(true);
	});
});
