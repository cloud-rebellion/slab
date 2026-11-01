import { request, type APIRequestContext } from '@playwright/test';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { test, expect, u } from '../../fixtures/auth';
import { createSite, deleteSite, rand, type Site } from '../../fixtures/data';
import { E2E_CONSTANTS } from '../../playwright.config';

const currentDir = path.dirname(fileURLToPath(import.meta.url));

// 1x1 transparent PNG
const PNG_BYTES = Buffer.from(
	'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=',
	'base64'
);

// Multipart upload context with the admin cookie but WITHOUT Content-Type:application/json,
// since the auth fixture's adminApi sets that globally and breaks multipart boundaries on Bun.
async function multipartContext(): Promise<APIRequestContext> {
	const cookieFile = path.resolve(currentDir, '../../.tmp/admin-cookie.txt');
	const cookie = readFileSync(cookieFile, 'utf8').trim();
	return request.newContext({
		extraHTTPHeaders: {
			Cookie: `${E2E_CONSTANTS.COOKIE_NAME}=${cookie}`
		}
	});
}

async function uploadOne(siteID: string) {
	const ctx = await multipartContext();
	try {
		const res = await ctx.post(u(`/api/sites/${siteID}/media`), {
			multipart: {
				file: { name: `${rand('img')}.png`, mimeType: 'image/png', buffer: PNG_BYTES }
			}
		});
		if (!res.ok()) {
			throw new Error(`upload failed ${res.status()} ${await res.text()}`);
		}
		return res.json();
	} finally {
		await ctx.dispose();
	}
}

test.describe('admin-api: media CRUD', () => {
	const cleanup: string[] = [];
	let site: Site;

	test.beforeAll(async ({ adminApi }) => {
		site = await createSite(adminApi);
		cleanup.push(site.id);
	});

	test.afterAll(async ({ adminApi }) => {
		for (const id of cleanup) await deleteSite(adminApi, id);
	});

	test('POST multipart upload returns media row with variants_json + blurhash', async () => {
		const m = await uploadOne(site.id);
		expect(m.id).toBeTruthy();
		expect(m.filename).toBeTruthy();
		expect(m.variants_json).toBeTruthy();
		expect(m.blurhash).toBeTruthy();
	});

	test('GET lists media (paginated shape)', async ({ adminApi }) => {
		const m = await uploadOne(site.id);
		const res = await adminApi.get(u(`/api/sites/${site.id}/media`));
		expect(res.ok()).toBe(true);
		const body = await res.json();
		expect(Array.isArray(body.items)).toBe(true);
		expect(typeof body.total).toBe('number');
		expect(body.items.find((x: { id: string }) => x.id === m.id)).toBeTruthy();
	});

	test('GET /{id} returns a single media row', async ({ adminApi }) => {
		const m = await uploadOne(site.id);
		const res = await adminApi.get(u(`/api/sites/${site.id}/media/${m.id}`));
		expect(res.ok()).toBe(true);
		const got = await res.json();
		expect(got.id).toBe(m.id);
	});

	test('PATCH updates alt_text', async ({ adminApi }) => {
		const m = await uploadOne(site.id);
		const res = await adminApi.patch(u(`/api/sites/${site.id}/media/${m.id}`), {
			data: { alt_text: 'red square' }
		});
		expect(res.ok()).toBe(true);
		const updated = await res.json();
		expect(updated.alt_text).toBe('red square');
	});

	test('public /media/* serves without auth', async ({ adminApi }) => {
		const m = await uploadOne(site.id);
		const get = await adminApi.get(u(`/api/sites/${site.id}/media/${m.id}`));
		const row = await get.json();
		const originalPath: string = row.original_path;
		expect(originalPath).toBeTruthy();
		const noAuth = await request.newContext();
		try {
			const res = await noAuth.get(u(`/media/${originalPath}`));
			expect(res.ok()).toBe(true);
			const buf = await res.body();
			expect(buf.byteLength).toBeGreaterThan(0);
			expect(res.headers()['content-type']).toMatch(/image\//);
		} finally {
			await noAuth.dispose();
		}
	});

	test('DELETE removes media', async ({ adminApi }) => {
		const m = await uploadOne(site.id);
		const res = await adminApi.delete(u(`/api/sites/${site.id}/media/${m.id}`));
		expect(res.ok()).toBe(true);
		const body = await res.json();
		expect(body.status).toBe('deleted');
		const get = await adminApi.get(u(`/api/sites/${site.id}/media/${m.id}`));
		expect(get.status()).toBe(404);
	});
});
