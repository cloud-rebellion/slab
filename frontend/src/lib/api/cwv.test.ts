import assert from 'node:assert/strict';
import { afterEach, test } from 'node:test';
import { getCWVOverview } from './cwv';

const originalFetch = globalThis.fetch;

afterEach(() => {
	globalThis.fetch = originalFetch;
});

test('CWV uses the shared client API prefix once with its default window', async () => {
	const overview = { site_id: 'fixture-site', since: '7d', metrics: [] };
	const calls: { url: RequestInfo | URL; init?: RequestInit }[] = [];
	globalThis.fetch = (async (url, init) => {
		calls.push({ url, init });
		return new Response(JSON.stringify(overview), { headers: { 'Content-Type': 'application/json' } });
	}) as typeof fetch;

	assert.deepEqual(await getCWVOverview('fixture-site'), overview);
	assert.equal(calls.length, 1);
	const call = calls[0];
	assert.ok(call);
	assert.equal(call.url, '/api/sites/fixture-site/analytics/cwv?since=7d');
	assert.equal(call.init?.method, 'GET');
	assert.equal(call.init?.credentials, 'include');
	assert.equal(new Headers(call.init?.headers).get('Accept'), 'application/json');
});

test('CWV preserves query encoding through the shared client', async () => {
	const calls: (RequestInfo | URL)[] = [];
	globalThis.fetch = (async (url) => {
		calls.push(url);
		return new Response(JSON.stringify({ site_id: 'fixture-site', since: '7d', metrics: [] }));
	}) as typeof fetch;

	await getCWVOverview('fixture-site', '7d&extra=fixture');
	assert.equal(calls.length, 1);
	assert.equal(calls[0], '/api/sites/fixture-site/analytics/cwv?since=7d%26extra%3Dfixture');
});
