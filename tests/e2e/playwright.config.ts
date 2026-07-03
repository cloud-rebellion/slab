import { defineConfig } from '@playwright/test';
import path from 'node:path';

const TEST_PORT = process.env.TEST_PORT ?? '18290';
const STUB_COOKIEPROOF_PORT = process.env.STUB_COOKIEPROOF_PORT ?? '18291';
const STUB_CRM_PORT = process.env.STUB_CRM_PORT ?? '18292';
const STUB_DOCKYARD_PORT = process.env.STUB_DOCKYARD_PORT ?? '18293';
const BASE_URL = process.env.E2E_BASE_URL ?? `http://127.0.0.1:${TEST_PORT}`;
const DATA_DIR = process.env.E2E_DATA_DIR ?? `/tmp/atomicsite-e2e`;

export const E2E_CONSTANTS = {
	ADMIN_EMAIL: 'admin@atomicsite.dev',
	ADMIN_PASSWORD: 'changeme123',
	API_BASE: BASE_URL,
	TEST_PORT,
	DATA_DIR,
	NGINX_LOG_DIR: `${DATA_DIR}/nginx-logs`,
	STUB_COOKIEPROOF: `http://127.0.0.1:${STUB_COOKIEPROOF_PORT}`,
	STUB_CRM: `http://127.0.0.1:${STUB_CRM_PORT}`,
	STUB_DOCKYARD: `https://127.0.0.1:${STUB_DOCKYARD_PORT}`,
	STUB_CRM_SECRET: 'e2e-crm-hmac-secret',
	COOKIE_NAME: 'atomicsite_token'
};

export default defineConfig({
	testDir: './specs',
	testMatch: '**/*.spec.ts',
	fullyParallel: false,
	workers: 1,
	retries: process.env.CI ? 1 : 0,
	reporter: process.env.CI ? [['html', { open: 'never' }], ['list']] : 'list',
	timeout: 60_000,
	expect: { timeout: 8_000 },

	globalSetup: './global-setup.ts',

	use: {
		baseURL: BASE_URL,
		trace: 'on-first-retry',
		video: 'retain-on-failure',
		screenshot: 'only-on-failure',
		actionTimeout: 10_000,
		navigationTimeout: 15_000
	},

	webServer: [
		{
			// Stubs for CookieProof / BrightCRM / Dockyard. Must come up before
			// the atomicsite binary so it sees the env-injected URLs.
			command: 'bun fixtures/stubs.ts',
			cwd: __dirname,
			url: `http://127.0.0.1:${STUB_COOKIEPROOF_PORT}/__health`,
			reuseExistingServer: false,
			timeout: 15_000,
			stdout: 'pipe',
			stderr: 'pipe',
			env: {
				STUB_COOKIEPROOF_PORT,
				STUB_CRM_PORT,
				STUB_DOCKYARD_PORT
			}
		},
		{
			// Wipe the test DB on every run so the seed step always starts fresh
			// (the slug uniqueness check would 409 on a re-run otherwise).
			command: `bash -lc 'rm -rf "$DATA_DIR" && ./bin/atomicsite'`,
			cwd: path.resolve(__dirname, '../..'),
			url: `${BASE_URL}/api/health`,
			reuseExistingServer: false,
			timeout: 60_000,
			stdout: 'pipe',
			stderr: 'pipe',
			env: {
				DATA_DIR,
				PORT: TEST_PORT,
				BASE_URL,
				JWT_SECRET: 'e2e-jwt-secret-not-for-production',
				ANALYTICS_SALT: 'e2e-analytics-salt',
				// The suite bursts createSite/deleteSite far past the production
				// write bucket (20 burst / 1 per sec, keyed per admin user + IP),
				// and every test shares one admin user on 127.0.0.1. Disable the
				// limiter for E2E; no spec asserts a 429.
				ADMIN_WRITE_RATE_CAPACITY: '0',
				COOKIEPROOF_API_BASE: `http://127.0.0.1:${STUB_COOKIEPROOF_PORT}`,
				COOKIEPROOF_ADMIN_TOKEN: 'e2e-cookieproof-token',
				BRIGHTCRM_WEBHOOK_URL: `http://127.0.0.1:${STUB_CRM_PORT}/webhook`,
				BRIGHTCRM_WEBHOOK_SECRET: 'e2e-crm-hmac-secret',
				CRM_SYNC_MIN_INTERVAL_SECONDS: '1',
				// Trust the self-signed cert the dockyard stub serves over TLS so
				// internal/deploy/dockyard.go's outbound HTTPS calls succeed.
				SSL_CERT_FILE: path.resolve(__dirname, 'fixtures/certs/stub-cert.pem'),
				// Override the per-site nginx access log dir so the analytics parser
				// can tail a path the test can append to (default /var/log/atomicsite
				// is unwritable in CI).
				ATOMICSITE_LOG_DIR: `${DATA_DIR}/nginx-logs`
			}
		}
	]
});
