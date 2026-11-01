import { request } from '@playwright/test';
import { randomUUID } from 'node:crypto';
import { test, expect, u } from '../../fixtures/auth';
import {
	createSite,
	deleteSite,
	getStubCalls,
	resetStub,
	crmDeliveryTimeoutMs,
	waitForOwnedCRMCalls,
	type CRMStubCall,
	type Site
} from '../../fixtures/data';
import { E2E_CONSTANTS } from '../../playwright.config';

// The owned receiver verifies HMAC-SHA256 over the exact captured request
// bytes using the same synthetic fixture key as the app. No JSON
// re-serialization or wrong-but-hex fallback is a signature verdict.
const SIGNATURE_HEADER = 'x-atomicsite-signature';

test.describe('analytics: BrightCRM HMAC signature is valid', () => {
	let site: Site;
	const cleanup: string[] = [];

	test.beforeAll(async ({ adminApi }) => {
		site = await createSite(adminApi);
		cleanup.push(site.id);
	});

	test.afterAll(async ({ adminApi }) => {
		for (const id of cleanup) await deleteSite(adminApi, id);
	});

	test('owned CRM calls have exact raw-body HMAC and forged hex is invalid', async ({ adminApi }) => {
		const proofPage = '/e2e-crm-hmac/' + randomUUID();
		const waitMs = await crmDeliveryTimeoutMs(adminApi, 1);
		// At most two existing outbox ticks plus bounded request/cleanup time.
		// The single-batch case retains the suite's normal 60-second budget.
		test.setTimeout(waitMs + 20_000);
		await resetStub('crm'); // Clears observations, never the durable outbox.

		const ctx = await request.newContext({
			extraHTTPHeaders: { 'Content-Type': 'application/json' }
		});
		try {
			const res = await ctx.post(u('/t/consent'), {
				data: {
					siteId: site.id,
					page: proofPage,
					consent: {
						version: 1,
						timestamp: Math.floor(Date.now() / 1000),
						method: 'accept-all',
						categories: { necessary: true, analytics: true }
					}
				}
			});
			expect(res.status()).toBe(204);
		} finally {
			await ctx.dispose();
		}

		const calls = await waitForOwnedCRMCalls(site.id, [proofPage], waitMs);
		expect(calls.length).toBeGreaterThanOrEqual(1);
		for (const call of calls) {
			const signature = call.headers[SIGNATURE_HEADER];
			expect(typeof signature === 'string' && /^[0-9a-f]{64}$/.test(signature)).toBe(true);
			expect(call.hmacOk, 'HMAC must match the exact captured request bytes').toBe(true);
		}

		// A syntactically valid hex string must never count as a valid HMAC.
		// This touches only the owned receiver stub, not the app or outbox.
		const forgedPage = proofPage + '/forged';
		const probe = await request.newContext();
		try {
			const response = await probe.post(E2E_CONSTANTS.STUB_CRM + '/webhook', {
				headers: { [SIGNATURE_HEADER]: '0'.repeat(64) },
				data: { event: 'identified', site_id: site.id, page: { url: forgedPage } }
			});
			expect(response.ok()).toBe(true);
		} finally {
			await probe.dispose();
		}
		const forged = ((await getStubCalls('crm')) as CRMStubCall[]).filter(
			(call) => call.body.site_id === site.id && call.body.page?.url === forgedPage
		);
		expect(forged.length).toBe(1);
		expect(forged[0].hmacOk).toBe(false);
	});
});
