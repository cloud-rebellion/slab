import { test, expect, u } from '../../fixtures/auth';
import { createSite, deleteSite, type Site } from '../../fixtures/data';

// MCP server protocol coverage. The /mcp endpoint speaks JSON-RPC 2.0 and
// translates tool / resource / prompt calls into the existing store +
// builder logic. This spec locks in:
//
//   - the protocol handshake (initialize + ping)
//   - tools/list returns the curated allow-list (privacy-critical: no
//     visitor-metadata tool)
//   - tools/call round-trips through to a real DB write
//   - resources/list + resources/read deliver the live site context
//   - prompts/list returns the playbook templates
//   - validation errors surface as application-level isError, not as
//     JSON-RPC envelope errors (so the model sees them)
//   - admin-only categories get rejected at the MCP boundary
//   - bad auth gets a 401 from the AgentAuthMiddleware before reaching
//     the dispatcher

interface JSONRPCResponse {
	jsonrpc: string;
	id?: number | string;
	result?: any;
	error?: { code: number; message: string };
}

test.describe('agent-api: MCP server', () => {
	const cleanup: string[] = [];
	let site: Site;
	let agentKey: string;

	test.beforeAll(async ({ adminApi }) => {
		site = await createSite(adminApi);
		cleanup.push(site.id);
		const keyRes = await adminApi.post(u(`/api/sites/${site.id}/agent-keys`), {
			data: { name: 'mcp-probe', capabilities: ['read', 'write'] }
		});
		const body = await keyRes.json();
		agentKey = body.key;
	});

	test.afterAll(async ({ adminApi }) => {
		for (const id of cleanup) await deleteSite(adminApi, id);
	});

	async function rpc(adminApi: any, id: number, method: string, params?: any): Promise<JSONRPCResponse> {
		const res = await adminApi.post(u('/mcp'), {
			headers: { 'X-Agent-Key': agentKey, 'Content-Type': 'application/json' },
			data: { jsonrpc: '2.0', id, method, params }
		});
		expect(res.status()).toBe(200);
		return await res.json();
	}

	test('initialize negotiates protocol + advertises capabilities', async ({ adminApi }) => {
		const r = await rpc(adminApi, 1, 'initialize', {
			protocolVersion: '2025-03-26',
			capabilities: {}
		});
		expect(r.error).toBeUndefined();
		expect(r.result.serverInfo.name).toBe('atomicsite');
		expect(r.result.protocolVersion).toBe('2025-03-26');
		expect(r.result.capabilities.tools).toBeDefined();
		expect(r.result.capabilities.resources).toBeDefined();
		expect(r.result.capabilities.prompts).toBeDefined();
	});

	test('ping returns empty result', async ({ adminApi }) => {
		const r = await rpc(adminApi, 2, 'ping');
		expect(r.error).toBeUndefined();
		expect(r.result).toEqual({});
	});

	test('tools/list returns the curated allow-list (NO visitor-metadata)', async ({
		adminApi
	}) => {
		const r = await rpc(adminApi, 3, 'tools/list');
		expect(r.error).toBeUndefined();
		const names = (r.result.tools as Array<{ name: string }>).map((t) => t.name);
		// Required tools present
		for (const required of [
			'get_site_context',
			'get_settings_catalog',
			'list_pages',
			'create_page',
			'update_page',
			'delete_page',
			'list_blocks',
			'create_block',
			'update_block',
			'delete_block',
			'get_branding',
			'get_profile',
			'update_profile',
			'list_settings',
			'bulk_upsert_settings',
			'get_security_posture',
			'list_media',
			'trigger_build',
			'get_build_status',
			'get_evaluation'
		]) {
			expect(names, `missing required tool: ${required}`).toContain(required);
		}
		// Privacy headline: NO tool name leaks visitor PII through MCP.
		for (const name of names) {
			expect(name.toLowerCase()).not.toContain('visitor');
		}
	});

	test('resources/list returns the live-context resources', async ({ adminApi }) => {
		const r = await rpc(adminApi, 4, 'resources/list');
		expect(r.error).toBeUndefined();
		const uris = (r.result.resources as Array<{ uri: string }>).map((res) => res.uri);
		for (const required of [
			'atomicsite://site/context',
			'atomicsite://site/settings_catalog',
			'atomicsite://site/security_posture',
			'atomicsite://site/i18n',
			'atomicsite://site/pending_setup',
			'atomicsite://site/structure'
		]) {
			expect(uris).toContain(required);
		}
		// Every resource URI must be readable.
		for (const uri of uris) {
			const read = await rpc(adminApi, 100, 'resources/read', { uri });
			expect(read.error, `failed to read ${uri}`).toBeUndefined();
			expect(read.result.contents[0].text.length).toBeGreaterThan(2);
		}
	});

	test('prompts/list returns the playbook templates', async ({ adminApi }) => {
		const r = await rpc(adminApi, 5, 'prompts/list');
		expect(r.error).toBeUndefined();
		const names = (r.result.prompts as Array<{ name: string }>).map((p) => p.name);
		expect(names).toEqual(
			expect.arrayContaining([
				'walk_through_pending_setup',
				'audit_seo',
				'connect_analytics',
				'add_iframe_integration',
				'create_landing_page'
			])
		);
	});

	test('tools/call create_page round-trips to a real DB row', async ({ adminApi }) => {
		const r = await rpc(adminApi, 6, 'tools/call', {
			name: 'create_page',
			arguments: { title: 'MCP Test Page', slug: 'mcp-test' }
		});
		expect(r.error).toBeUndefined();
		expect(r.result.isError).toBeFalsy();
		const page = JSON.parse(r.result.content[0].text);
		expect(page.title).toBe('MCP Test Page');
		expect(page.slug).toBe('mcp-test');
		// Verify via the existing admin REST surface that the row exists.
		const verify = await adminApi.get(u(`/api/sites/${site.id}/pages`));
		const body = await verify.json();
		const pages: any[] = Array.isArray(body) ? body : (body.pages ?? []);
		expect(pages.find((p) => p.id === page.id)).toBeTruthy();
	});

	test('tools/call bulk_upsert_settings writes + validates + rejects admin-only', async ({
		adminApi
	}) => {
		const r = await rpc(adminApi, 7, 'tools/call', {
			name: 'bulk_upsert_settings',
			arguments: {
				items: [
					{ category: 'analytics', key: 'ga4_enabled', value: '1' },
					{ category: 'analytics', key: 'ga4_id', value: 'G-MCPTEST' },
					{ category: 'security', key: 'hsts_enabled', value: '1' } // admin-only
				]
			}
		});
		expect(r.error).toBeUndefined();
		expect(r.result.isError).toBeFalsy();
		const body = JSON.parse(r.result.content[0].text);
		expect(body.rejected_admin_only).toContain('security.hsts_enabled');
		const ga4 = body.settings.find(
			(s: any) => s.category === 'analytics' && s.key === 'ga4_id'
		);
		expect(ga4?.value).toBe('G-MCPTEST');
	});

	test('tools/call validation error surfaces as isError=true (not JSON-RPC error)', async ({
		adminApi
	}) => {
		const r = await rpc(adminApi, 8, 'tools/call', {
			name: 'bulk_upsert_settings',
			arguments: {
				items: [{ category: 'analytics', key: 'ga4_id', value: 'BAD-FORMAT' }]
			}
		});
		expect(r.error).toBeUndefined(); // JSON-RPC envelope is fine
		expect(r.result.isError).toBe(true); // application error
		expect(r.result.content[0].text).toMatch(/ga4_id/);
	});

	test('tools/call unknown tool returns isError, not JSON-RPC error', async ({
		adminApi
	}) => {
		const r = await rpc(adminApi, 9, 'tools/call', {
			name: 'this_tool_does_not_exist',
			arguments: {}
		});
		expect(r.error).toBeUndefined();
		expect(r.result.isError).toBe(true);
		expect(r.result.content[0].text).toContain('unknown tool');
	});

	test('unknown method returns JSON-RPC method-not-found', async ({ adminApi }) => {
		const r = await rpc(adminApi, 10, 'totally/madeup');
		expect(r.error).toBeDefined();
		expect(r.error!.code).toBe(-32601);
	});

	test('missing X-Agent-Key returns 401 from middleware', async ({ adminApi }) => {
		const res = await adminApi.post(u('/mcp'), {
			data: { jsonrpc: '2.0', id: 11, method: 'ping' }
		});
		expect(res.status()).toBe(401);
	});

	test('GET /mcp returns 405 (POST-only synchronous transport today)', async ({
		adminApi
	}) => {
		const res = await adminApi.get(u('/mcp'), {
			headers: { 'X-Agent-Key': agentKey }
		});
		expect(res.status()).toBe(405);
	});
});
