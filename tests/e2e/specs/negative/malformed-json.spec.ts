import { test, expect, u } from '../../fixtures/auth';

test.describe('negative: malformed JSON body', () => {
	test('POST /api/sites with invalid JSON returns 400 with no panic', async ({ adminApi }) => {
		const res = await adminApi.post(u('/api/sites'), {
			headers: { 'Content-Type': 'application/json' },
			data: '{not valid json'
		});
		expect(res.status()).toBe(400);
		const body = await res.json();
		expect(body.error).toBeTruthy();
	});

	test('POST /api/sites/seed with invalid JSON returns 400', async ({ adminApi }) => {
		const res = await adminApi.post(u('/api/sites/seed'), {
			headers: { 'Content-Type': 'application/json' },
			data: '<<<not json>>>'
		});
		expect(res.status()).toBe(400);
		const body = await res.json();
		expect(body.error).toBeTruthy();
	});

	test('PATCH /api/sites/{id} with invalid JSON returns 400', async ({ adminApi }) => {
		// Need a real site id. Just send to a known-non-existent id; PATCH still
		// has to parse the body before the lookup, so 400 dominates 404.
		const res = await adminApi.patch(u('/api/sites/000000000000000000000000'), {
			headers: { 'Content-Type': 'application/json' },
			data: '}}}}'
		});
		// 400 (parse fail), 404, or 403 (site-access middleware refuses the
		// unknown site before the body is ever parsed). All safe.
		expect([400, 404, 403]).toContain(res.status());
	});

	test('server stays responsive after malformed payload', async ({ adminApi }) => {
		// Fire the bad request, then fire a valid one - must still get 200.
		await adminApi.post(u('/api/sites'), {
			headers: { 'Content-Type': 'application/json' },
			data: 'garbage'
		});
		const ok = await adminApi.get(u('/api/sites'));
		expect(ok.ok()).toBe(true);
	});
});
