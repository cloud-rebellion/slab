import {
	test as base,
	expect,
	request,
	type APIRequestContext,
	type Page
} from '@playwright/test';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { E2E_CONSTANTS } from '../playwright.config';

const currentDir = path.dirname(fileURLToPath(import.meta.url));

const COOKIE_FILE = path.resolve(currentDir, '../.tmp/admin-cookie.txt');

let cachedCookie: string | null = null;

function loadCookie(): string {
	if (cachedCookie) return cachedCookie;
	try {
		cachedCookie = readFileSync(COOKIE_FILE, 'utf8').trim();
	} catch {
		throw new Error(
			`admin cookie not found at ${COOKIE_FILE}; globalSetup did not run or failed`
		);
	}
	if (!cachedCookie) throw new Error('admin cookie file is empty');
	return cachedCookie;
}

export const ADMIN_EMAIL = E2E_CONSTANTS.ADMIN_EMAIL;
export const ADMIN_PASSWORD = E2E_CONSTANTS.ADMIN_PASSWORD;

/**
 * Legacy helper used by the existing wizard.spec.ts. Performs a UI login.
 * Prefer the `loggedInPage` fixture for new specs.
 */
export async function loginAsAdmin(page: Page): Promise<void> {
	await page.goto('/login');
	await page.getByLabel(/email/i).fill(ADMIN_EMAIL);
	await page.getByLabel(/password/i).fill(ADMIN_PASSWORD);
	await page.getByRole('button', { name: /sign in|log in|login/i }).click();
	await page.waitForURL((url) => !url.pathname.startsWith('/login'), { timeout: 10_000 });
}

type AuthFixtures = {
	adminCookie: string;
	adminApi: APIRequestContext;
	anonApi: APIRequestContext;
	loggedInPage: Page;
};

export const test = base.extend<AuthFixtures>({
	adminCookie: async ({}, use) => {
		await use(loadCookie());
	},

	adminApi: async ({ adminCookie }, use) => {
		// NOTE: We deliberately do NOT set baseURL. Bun's fetch returns
		// response.url as a path-only string, which breaks Playwright's
		// internal Set-Cookie parser when the request was made via baseURL +
		// relative path. Use the `u()` helper below for absolute URLs.
		// NOTE: Do NOT set Content-Type here. Playwright auto-sets it from the
		// payload type (`data:` -> application/json, `multipart:` ->
		// multipart/form-data; boundary=...). A global override breaks
		// multipart uploads (the boundary disappears and the server reads junk).
		const ctx = await request.newContext({
			extraHTTPHeaders: {
				Cookie: `${E2E_CONSTANTS.COOKIE_NAME}=${adminCookie}`
			}
		});
		await use(ctx);
		await ctx.dispose();
	},

	/**
	 * A request context carrying NO session cookie. Use this for anything
	 * a stranger must be able to reach: the Mollie billing callback, the
	 * public waitlist form, the storefront checkout.
	 *
	 * Testing a public endpoint with `adminApi` proves nothing about it
	 * being public, and that is not hypothetical: both the waitlist submit
	 * and the billing webhook specs did exactly that, stayed green for
	 * months, and hid two routes that were registered inside the
	 * authenticated group and 401'd every real caller.
	 */
	anonApi: async ({}, use) => {
		const ctx = await request.newContext();
		await use(ctx);
		await ctx.dispose();
	},

	loggedInPage: async ({ page, adminCookie }, use) => {
		const url = new URL(E2E_CONSTANTS.API_BASE);
		await page.context().addCookies([
			{
				name: E2E_CONSTANTS.COOKIE_NAME,
				value: adminCookie,
				domain: url.hostname,
				path: '/',
				httpOnly: true,
				secure: false,
				sameSite: 'Lax'
			}
		]);
		await use(page);
	}
});

/**
 * Absolute-URL helper. All API calls must use this - see the comment in
 * `adminApi` above for why.
 */
export function u(path: string): string {
	if (/^https?:\/\//.test(path)) return path;
	return `${E2E_CONSTANTS.API_BASE}${path.startsWith('/') ? path : `/${path}`}`;
}

export { expect };
