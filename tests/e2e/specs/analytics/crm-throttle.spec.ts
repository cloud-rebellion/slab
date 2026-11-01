import { request } from '@playwright/test';
import { randomUUID } from 'node:crypto';
import { test, expect, u } from '../../fixtures/auth';
import {
	createSite,
	deleteSite,
	crmDeliveryTimeoutMs,
	waitForOwnedCRMCalls,
	resetStub,
	type Site
} from '../../fixtures/data';

// internal/crmsync/throttle.go suppresses repeated *non-identified* events
// for the same (site, visitor). Identified events (the ones /t/consent emits)
// bypass throttling by design, so two consents back-to-back must BOTH land
// in the CRM stub.
//
// /t/pageview itself does not currently call the CRM client, so the only
// outbound channel we can observe from e2e is the consent path. Delivery is
// durable and batched, so assert the bypass for each unique owned event.

async function postConsent(ctx: Awaited<ReturnType<typeof request.newContext>>, siteId: string, page: string) {
	return ctx.post(u('/t/consent'), {
		data: {
			siteId,
			page,
			consent: {
				version: 1,
				timestamp: Math.floor(Date.now() / 1000),
				method: 'accept-all',
				categories: { necessary: true, analytics: true }
			}
		}
	});
}

test.describe('analytics: identified events bypass throttler', () => {
	let site: Site;
	const cleanup: string[] = [];

	test.beforeAll(async ({ adminApi }) => {
		site = await createSite(adminApi);
		cleanup.push(site.id);
	});

	test.afterAll(async ({ adminApi }) => {
		for (const id of cleanup) await deleteSite(adminApi, id);
	});

	test('three rapid consents all reach CRM (identified events skip throttle)', async ({ adminApi }) => {
		const marker = '/e2e-crm-throttle/' + randomUUID();
		const pages = [marker + '/1', marker + '/2', marker + '/3'];
		const waitMs = await crmDeliveryTimeoutMs(adminApi, pages.length);
		test.setTimeout(waitMs + 20_000);
		await resetStub('crm'); // Observations only; older outbox rows remain.
		const ctx = await request.newContext({
			extraHTTPHeaders: { 'Content-Type': 'application/json' }
		});
		try {
			const r1 = await postConsent(ctx, site.id, pages[0]);
			const r2 = await postConsent(ctx, site.id, pages[1]);
			const r3 = await postConsent(ctx, site.id, pages[2]);
			expect([r1.status(), r2.status(), r3.status()]).toEqual([204, 204, 204]);
		} finally {
			await ctx.dispose();
		}

		const calls = await waitForOwnedCRMCalls(site.id, pages, waitMs);
		expect(calls.length).toBeGreaterThanOrEqual(3);
		// Duplicate/replayed delivery of one event cannot stand in for the
		// other two, and unrelated durable backlog cannot satisfy this case.
		expect(pages.every((page) => calls.some((call) => call.body.page?.url === page))).toBe(true);
	});
});
