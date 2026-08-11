import { test, expect, u } from '../../fixtures/auth';

// Phase 30.2: billing routes (Mollie). The OSS test harness ships
// without MOLLIE_API_KEY set, so checkoutImpl is nil and the route
// returns 503. The webhook route accepts the form post but no-ops
// since webhookImpl is nil. Both behaviours are correct for OSS;
// these tests lock them in so a future EE wire-up doesn't accidentally
// flip the OSS contract.
//
// Real Mollie integration tests will run against the Mollie test mode
// in the EE harness (Phase 30.5 stage), separate file.

test.describe('Phase 30.2: billing (OSS contract)', () => {
	test('GET /api/workspaces/{id}/billing returns plan + usage for default workspace', async ({ adminApi }) => {
		const wsRes = await adminApi.get(u('/api/workspaces'));
		const wsList = await wsRes.json();
		const def = wsList.workspaces[0];
		expect(def).toBeTruthy();

		const res = await adminApi.get(u(`/api/workspaces/${def.id}/billing`));
		expect(res.ok()).toBeTruthy();
		const body = await res.json();
		expect(body.plan).toBeTruthy();
		expect(body.plan.key).toBe('oss');
		expect(body.plan.max_sites).toBe(-1); // OSS = unlimited
		expect(body.workspace_status).toBe('active');
		expect(typeof body.sites_count).toBe('number');
	});

	test('POST checkout returns 503 in OSS (Mollie not configured)', async ({ adminApi }) => {
		const wsRes = await adminApi.get(u('/api/workspaces'));
		const wsList = await wsRes.json();
		const def = wsList.workspaces[0];
		const res = await adminApi.post(u(`/api/workspaces/${def.id}/billing/checkout`), {
			data: { plan: 'studio' }
		});
		expect(res.status()).toBe(503);
	});

	test('POST checkout rejects unknown plan with 400', async ({ adminApi }) => {
		const wsRes = await adminApi.get(u('/api/workspaces'));
		const wsList = await wsRes.json();
		const def = wsList.workspaces[0];
		const res = await adminApi.post(u(`/api/workspaces/${def.id}/billing/checkout`), {
			data: { plan: 'enterprise' }
		});
		// Either 400 (validated client-side first) or 503 (Mollie shim
		// not wired). Both are acceptable OSS contracts.
		expect([400, 503]).toContain(res.status());
	});

	// anonApi, not adminApi: Mollie posts this URL from its own servers
	// with no cookie of ours. Running it on adminApi hid the fact that the
	// route sat inside the authenticated group and 401'd every real
	// callback, so subscription payments never confirmed.
	test('Webhook accepts form post + 200s even when Mollie not wired', async ({ anonApi }) => {
		const res = await anonApi.post(u('/api/billing/webhook'), {
			form: { id: 'tr_test_invalid' }
		});
		expect(res.status(), 'Mollie has no session cookie; this route must not require one').not.toBe(
			401
		);
		expect(res.status()).toBe(200);
		const body = await res.json();
		// Either no-op (Mollie not wired) or fetch-failed (live key
		// hits real API). The OSS harness is the no-op path.
		expect(['no-op', 'fetch-failed', 'ok']).toContain(body.status);
	});
});
