import { request } from '@playwright/test';
import { randomUUID } from 'node:crypto';
import { test, expect, u } from '../../fixtures/auth';
import {
	createSite,
	deleteSite,
	getStubCalls,
	resetStub,
	crmDeliveryTimeoutMs,
	waitForOwnedCRMCalls
} from '../../fixtures/data';

// /t/consent flow: the in-page CookieProof relay POSTs a payload after the
// visitor opts in. Server upserts a visit_session, identifies it, and fires
// an "identified" event to BrightCRM (our stub). 204 on success.

test.describe('analytics: consent updates session + emits identified event', () => {
	const cleanup: string[] = [];

	test.afterAll(async ({ adminApi }) => {
		for (const id of cleanup) await deleteSite(adminApi, id);
	});

	test('analytics opt-in returns 204 and lands an identified event in the CRM stub', async ({ adminApi }) => {
		const site = await createSite(adminApi);
		cleanup.push(site.id);
		const proofPage = '/e2e-consent/opt-in/' + randomUUID();
		const proofSession = 'sess-e2e-' + randomUUID();
		const waitMs = await crmDeliveryTimeoutMs(adminApi, 1);
		test.setTimeout(waitMs + 20_000); // Existing bounded 60/90-second per-case budget.
		await resetStub('crm'); // Observations only; durable outbox rows remain.

		const ctx = await request.newContext({
			extraHTTPHeaders: {
				'Content-Type': 'application/json',
				'User-Agent': 'e2e-consent/1.0',
				'Accept-Language': 'en-US'
			}
		});
		try {
			const res = await ctx.post(u('/t/consent'), {
				data: {
					siteId: site.id,
					sessionId: proofSession,
					page: proofPage,
					referrer: '',
					consent: {
						version: 1,
						timestamp: Math.floor(Date.now() / 1000),
						method: 'accept-all',
						gpc: false,
						categories: { necessary: true, analytics: true, functional: true }
					}
				}
			});
			expect(res.status()).toBe(204);
		} finally {
			await ctx.dispose();
		}

		// Identified events use the existing durable outbox delivery window.
		const calls = await waitForOwnedCRMCalls(site.id, [proofPage], waitMs);
		expect(calls.length, 'expected at least one CRM webhook call').toBeGreaterThanOrEqual(1);
		const first = calls[0];
		expect(first.method).toBe('POST');
		const body = first.body as Record<string, unknown>;
		expect(body.event).toBe('identified');
		expect(body.site_id).toBe(site.id);
		expect(typeof body.visitor_id).toBe('string');
	});

	test('reject-all consent does NOT fire a CRM event', async ({ adminApi }) => {
		const site = await createSite(adminApi);
		cleanup.push(site.id);
		const proofPage = '/e2e-consent/reject-all/' + randomUUID();
		const proofSession = 'sess-e2e-' + randomUUID();
		// Reserve the delivery window for a counterfactual erroneous queued event.
		const waitMs = await crmDeliveryTimeoutMs(adminApi, 1);
		test.setTimeout(waitMs + 20_000); // No global suite deadline change.
		await resetStub('crm'); // Foreign durable rows may still arrive.
		const ctx = await request.newContext({
			extraHTTPHeaders: { 'Content-Type': 'application/json' }
		});
		try {
			const res = await ctx.post(u('/t/consent'), {
				data: {
					siteId: site.id,
					sessionId: proofSession,
					page: proofPage,
					consent: {
						version: 1,
						timestamp: Math.floor(Date.now() / 1000),
						method: 'reject-all',
						gpc: false,
						categories: { necessary: true, analytics: false }
					}
				}
			});
			expect(res.status()).toBe(204);
		} finally {
			await ctx.dispose();
		}
		// Observe the full drain window, including a final poll at/after it.
		// Reject ANY event for this fresh site: no page/event/method filter can
		// hide a misrouted own-site event, and foreign backlog cannot fail it.
		const deadline = Date.now() + waitMs;
		for (;;) {
			const calls = (await getStubCalls('crm')).filter((value) => {
				if (value === null || typeof value !== 'object') return false;
				const body = (value as { body?: unknown }).body;
				return body !== null && typeof body === 'object' &&
					(body as Record<string, unknown>).site_id === site.id;
			});
			expect(calls).toHaveLength(0);
			const remaining = deadline - Date.now();
			if (remaining <= 0) break;
			await new Promise((resolve) => setTimeout(resolve, Math.min(100, remaining)));
		}
	});

	test('unknown siteId returns 404', async () => {
		const ctx = await request.newContext({
			extraHTTPHeaders: { 'Content-Type': 'application/json' }
		});
		try {
			const res = await ctx.post(u('/t/consent'), {
				data: {
					siteId: 'aaaaaaaaaaaaaaaaaaaaaaaa',
					consent: {
						version: 1,
						timestamp: 0,
						method: 'accept-all',
						categories: { necessary: true, analytics: true }
					}
				}
			});
			expect(res.status()).toBe(404);
		} finally {
			await ctx.dispose();
		}
	});
});
