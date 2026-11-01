import { mkdirSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { request } from '@playwright/test';
import { E2E_CONSTANTS } from './playwright.config';

const currentDir = path.dirname(fileURLToPath(import.meta.url));

const TMP_DIR = path.resolve(currentDir, '.tmp');
const COOKIE_FILE = path.join(TMP_DIR, 'admin-cookie.txt');

async function waitForServer(baseURL: string, maxMs = 60_000): Promise<void> {
	// NOTE: Bun's fetch sets response.url to the path-only string, which breaks
	// Playwright's internal Set-Cookie parser when baseURL is set on the
	// context. We use absolute URLs everywhere to avoid the bug.
	const ctx = await request.newContext();
	const start = Date.now();
	while (Date.now() - start < maxMs) {
		try {
			const res = await ctx.get(`${baseURL}/api/health`);
			if (res.ok()) {
				await ctx.dispose();
				return;
			}
		} catch {}
		await new Promise((r) => setTimeout(r, 250));
	}
	await ctx.dispose();
	throw new Error(`atomicsite did not become healthy at ${baseURL} within ${maxMs}ms`);
}

function extractCookieValue(setCookie: string | null, name: string): string | null {
	if (!setCookie) return null;
	// Set-Cookie header may contain multiple cookies separated by commas. Be lenient.
	const parts = setCookie.split(/,(?=[^ ]+=)/);
	for (const part of parts) {
		const m = part.match(new RegExp(`(?:^|;\\s*)${name}=([^;]+)`));
		if (m) return m[1];
	}
	return null;
}

export default async function globalSetup() {
	mkdirSync(TMP_DIR, { recursive: true });

	await waitForServer(E2E_CONSTANTS.API_BASE);

	const ctx = await request.newContext();
	const res = await ctx.post(`${E2E_CONSTANTS.API_BASE}/api/auth/login`, {
		data: {
			email: E2E_CONSTANTS.ADMIN_EMAIL,
			password: E2E_CONSTANTS.ADMIN_PASSWORD
		}
	});
	if (!res.ok()) {
		const body = await res.text();
		await ctx.dispose();
		throw new Error(`admin login failed: ${res.status()} ${body}`);
	}

	// Cookie comes back as Set-Cookie header. Playwright merges multiple
	// Set-Cookie headers into a single string, so parse robustly.
	const setCookieHeader = res.headersArray().find((h) => h.name.toLowerCase() === 'set-cookie');
	const cookieValue = extractCookieValue(setCookieHeader?.value ?? null, E2E_CONSTANTS.COOKIE_NAME);
	if (!cookieValue) {
		await ctx.dispose();
		throw new Error('admin login did not set atomicsite_token cookie');
	}

	await ctx.dispose();
	writeFileSync(COOKIE_FILE, cookieValue, 'utf8');
	// eslint-disable-next-line no-console
	console.log(`[e2e] admin cookie cached at ${COOKIE_FILE}`);
}
