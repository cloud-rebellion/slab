import { test, expect } from '../../fixtures/auth';
import { createSite, deleteSite, triggerBuildAndWait } from '../../fixtures/data';
import { createPublishedPageWithBlock } from '../build-pipeline/_helpers';
import { checkNames, findCategory, getEvaluations, VALID_GRADES } from './_helpers';

test.describe('evaluation-engine: performance category', () => {
	const cleanup: string[] = [];

	test.afterAll(async ({ adminApi }) => {
		for (const id of cleanup) await deleteSite(adminApi, id);
	});

	test('performance row has weight + scripts + images checks', async ({ adminApi }) => {
		test.setTimeout(240_000);
		const site = await createSite(adminApi);
		cleanup.push(site.id);
		await createPublishedPageWithBlock(adminApi, site.id, { slug: '/', title: 'Perf Home' });

		const result = await triggerBuildAndWait(adminApi, site.id, 180_000);
		expect(result.status, `build_log:\n${result.build_log}`).toBe('success');

		const rows = await getEvaluations(adminApi, site.id, result.build_id);
		const perf = findCategory(rows, 'performance');
		expect(typeof perf.score).toBe('number');
		expect(VALID_GRADES.has(perf.grade), `grade=${perf.grade}`).toBe(true);

		const names = checkNames(perf);
		// Names from internal/eval/performance.go.
		for (const expected of [
			'HTML Size',
			'No Render-Blocking Scripts',
			'Modern Image Formats',
			'Lazy-Loaded Images',
			'Resource Hints'
		]) {
			expect(names, `missing ${expected} in ${names.join(',')}`).toContain(expected);
		}
	});
});
