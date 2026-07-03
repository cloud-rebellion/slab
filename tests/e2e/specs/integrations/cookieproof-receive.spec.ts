import { request } from '@playwright/test';
import { test, expect, u } from '../../fixtures/auth';
import { createSite, deleteSite, type Site } from '../../fixtures/data';

// /t/consent is the *receiver* side of the CookieProof relay: the in-page
// element POSTs the consent payload here. Atomicsite parses it, validates
// shape, and updates visit_sessions. consent.spec.ts already covers the
// happy path + CRM emission; this spec drills into payload-parsing edge
// cases that the receiver enforces.

test.describe('integrations: /t/consent payload validation', () => {
	let site: Site;
	const cleanup: string[] = [];

	test.beforeAll(async ({ adminApi }) => {
		site = await createSite(adminApi);
		cleanup.push(site.id);
	});

	test.afterAll(async ({ adminApi }) => {
		for (const id of cleanup) await deleteSite(adminApi, id);
	});

	test('valid GPC payload returns 204', async () => {
		const ctx = await request.newContext({
			extraHTTPHeaders: { 'Content-Type': 'application/json' }
		});
		try {
			const res = await ctx.post(u('/t/consent'), {
				data: {
					siteId: site.id,
					page: 'https://e2e.example.test/',
					consent: {
						version: 1,
						timestamp: 1700000000,
						method: 'gpc',
						gpc: true,
						categories: { necessary: true, analytics: true }
					}
				}
			});
			expect(res.status()).toBe(204);
		} finally {
			await ctx.dispose();
		}
	});

	test('payload missing consent block returns 400', async () => {
		const ctx = await request.newContext({
			extraHTTPHeaders: { 'Content-Type': 'application/json' }
		});
		try {
			const res = await ctx.post(u('/t/consent'), {
				data: { siteId: site.id, page: '/', sessionId: 'anon' }
			});
			expect(res.status()).toBe(400);
		} finally {
			await ctx.dispose();
		}
	});

	test('non-JSON body returns 400', async () => {
		const ctx = await request.newContext();
		try {
			const res = await ctx.post(u('/t/consent'), {
				headers: { 'Content-Type': 'application/json' },
				data: 'not-json'
			});
			expect(res.status()).toBe(400);
		} finally {
			await ctx.dispose();
		}
	});

	test('missing siteId returns 400', async () => {
		const ctx = await request.newContext({
			extraHTTPHeaders: { 'Content-Type': 'application/json' }
		});
		try {
			const res = await ctx.post(u('/t/consent'), {
				data: { page: '/', consent: { version: 1, timestamp: 0, categories: {} } }
			});
			expect(res.status()).toBe(400);
		} finally {
			await ctx.dispose();
		}
	});

	test('non-hex siteId is rejected (path traversal guard)', async () => {
		const ctx = await request.newContext({
			extraHTTPHeaders: { 'Content-Type': 'application/json' }
		});
		try {
			const res = await ctx.post(u('/t/consent'), {
				data: { siteId: '../etc/passwd', page: '/', consent: { categories: {} } }
			});
			expect(res.status()).toBe(400);
		} finally {
			await ctx.dispose();
		}
	});

	test('oversized body is rejected', async () => {
		const ctx = await request.newContext({
			extraHTTPHeaders: { 'Content-Type': 'application/json' }
		});
		try {
			// trackBodyMaxBytes = 16 KiB. 32 KiB of garbage in a string field
			// definitely exceeds the cap.
			const big = 'A'.repeat(32 * 1024);
			const res = await ctx.post(u('/t/consent'), {
				data: { siteId: site.id, page: big, consent: { categories: {} } }
			});
			expect([400, 413]).toContain(res.status());
		} finally {
			await ctx.dispose();
		}
	});
});
