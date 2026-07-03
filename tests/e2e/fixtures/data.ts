/**
 * Reusable factories so each spec stays self-contained. Every helper accepts
 * an APIRequestContext (typically `adminApi` from the auth fixture) and
 * returns the created entity. Random suffixes prevent slug collisions.
 */
import { type APIRequestContext, request } from '@playwright/test';
import { randomBytes } from 'node:crypto';
import { E2E_CONSTANTS } from '../playwright.config';
import { u } from './auth';

export { u };

export function rand(prefix = ''): string {
	const s = randomBytes(4).toString('hex');
	return prefix ? `${prefix}-${s}` : s;
}

async function jsonOrThrow(res: Awaited<ReturnType<APIRequestContext['post']>>, label: string) {
	if (!res.ok()) {
		const body = await res.text();
		throw new Error(`${label} failed: ${res.status()} ${body}`);
	}
	return res.json();
}

export type Site = {
	id: string;
	slug: string;
	name: string;
	domain: string;
	status: string;
	primary_color?: string;
	font_heading?: string;
};

export type Page_ = {
	id: string;
	site_id: string;
	slug: string;
	title: string;
	layout: string;
	status: string;
};

export type Block = {
	id: string;
	page_id: string;
	block_type: string;
	sort_order: number;
};

export type AgentKey = {
	id: string;
	site_id: string;
	name: string;
	key: string; // only on creation
	capabilities: string[];
};

export type DeployTarget = {
	id: string;
	site_id: string;
	kind: 'local' | 'rsync' | 'dockyard';
	is_default: boolean;
};

export type CreateSiteOptions = {
	name?: string;
	slug?: string;
	domain?: string;
	siteType?: 'b2b' | 'saas' | 'agency' | 'personal' | 'blog';
	starterKitID?: string;
	structureType?: 'one-pager' | 'soft-silo' | 'hard-silo';
	withSeed?: boolean;
};

/**
 * Creates a site via /api/sites/seed when withSeed is true (default), giving
 * you a realistic structure with pages + branding. Otherwise creates a blank
 * site via /api/sites.
 */
export async function createSite(api: APIRequestContext, opts: CreateSiteOptions = {}): Promise<Site> {
	const slug = opts.slug ?? rand('site');
	const name = opts.name ?? `E2E ${slug}`;
	const domain = opts.domain ?? `${slug}.e2e.test`;
	if (opts.withSeed === false) {
		const res = await api.post(u('/api/sites'), {
			data: { slug, name, domain }
		});
		const body = await jsonOrThrow(res, 'createSite (blank)');
		return body.site ?? body;
	}
	const seedBody: Record<string, unknown> = {
		type: opts.siteType ?? 'b2b',
		info: {
			name,
			slug,
			domain,
			lang: 'en',
			business_name: `${name} AB`,
			contact_email: `hej@${slug}.e2e.test`,
			country: 'SE'
		},
		structure: {
			structure_type: opts.structureType ?? 'one-pager',
			max_depth: 3
		},
		silos: [],
		branding: {
			primary_color: '#0e7490',
			secondary_color: '#52525b',
			bg_color: '#fafaf9',
			text_color: '#18181b',
			font_heading: 'Inter',
			font_body: 'Inter'
		}
	};
	if (opts.starterKitID) seedBody.starter_kit = opts.starterKitID;
	const res = await api.post(u('/api/sites/seed'), { data: seedBody });
	const body = await jsonOrThrow(res, 'createSite (seed)');
	const siteID: string = body.site_id ?? body.id ?? body.site?.id;
	if (!siteID) throw new Error(`createSite seed returned no site_id: ${JSON.stringify(body)}`);
	const get = await api.get(u(`/api/sites/${siteID}`));
	const fetched = await jsonOrThrow(get, 'createSite (fetch)');
	return (fetched.site ?? fetched) as Site;
}

export async function deleteSite(api: APIRequestContext, siteID: string): Promise<void> {
	const res = await api.delete(u(`/api/sites/${siteID}`));
	if (!res.ok() && res.status() !== 404) {
		throw new Error(`deleteSite failed: ${res.status()} ${await res.text()}`);
	}
}

export async function listSites(api: APIRequestContext): Promise<Site[]> {
	const res = await api.get(u('/api/sites'));
	const body = await jsonOrThrow(res, 'listSites');
	return body.sites ?? body;
}

export async function createPage(
	api: APIRequestContext,
	siteID: string,
	overrides: Partial<{ title: string; slug: string; layout: string; status: string }> = {}
): Promise<Page_> {
	// The agent endpoint normalizes slugs to start with `/`, so we store them
	// that way to keep both admin and agent code paths happy.
	const rawSlug = overrides.slug ?? rand('page');
	const slug = rawSlug.startsWith('/') ? rawSlug : `/${rawSlug}`;
	const res = await api.post(u(`/api/sites/${siteID}/pages`), {
		data: {
			title: overrides.title ?? `E2E Page ${rawSlug}`,
			slug,
			layout: overrides.layout ?? 'default',
			status: overrides.status ?? 'published'
		}
	});
	const created = await jsonOrThrow(res, 'createPage');
	const page = (created.page ?? created) as Page_;
	// The admin Create handler ignores `status` in the body and saves draft.
	// PATCH to set published if the caller asked for it (default).
	const wantStatus = overrides.status ?? 'published';
	if (page.status !== wantStatus) {
		const patch = await api.patch(u(`/api/sites/${siteID}/pages/${page.id}`), {
			data: { status: wantStatus }
		});
		if (patch.ok()) {
			const r2 = await api.get(u(`/api/sites/${siteID}/pages/${page.id}`));
			if (r2.ok()) {
				const b2 = await r2.json();
				return (b2.page ?? b2) as Page_;
			}
		}
	}
	return page;
}

export async function createBlock(
	api: APIRequestContext,
	siteID: string,
	pageID: string,
	overrides: Partial<{ block_type: string; data: unknown; sort_order: number; is_visible: boolean }> = {}
): Promise<Block> {
	const res = await api.post(u(`/api/sites/${siteID}/pages/${pageID}/blocks`), {
		data: {
			block_type: overrides.block_type ?? 'text',
			data: overrides.data ?? { content: 'Hello E2E' },
			sort_order: overrides.sort_order ?? 0,
			is_visible: overrides.is_visible ?? true
		}
	});
	const body = await jsonOrThrow(res, 'createBlock');
	return body.block ?? body;
}

export async function createAgentKey(
	api: APIRequestContext,
	siteID: string,
	capabilities: string[] = ['read', 'write', 'build']
): Promise<AgentKey> {
	const res = await api.post(u(`/api/sites/${siteID}/agent-keys`), {
		data: {
			name: rand('key'),
			capabilities
		}
	});
	const body = await jsonOrThrow(res, 'createAgentKey');
	return body.agent_key ?? body;
}

export async function agentApi(key: string): Promise<APIRequestContext> {
	// Don't set Content-Type globally - Playwright auto-derives it from the
	// payload (data: -> application/json, multipart: -> multipart/form-data).
	return request.newContext({
		extraHTTPHeaders: {
			'X-Agent-Key': key
		}
	});
}

export async function triggerBuild(api: APIRequestContext, siteID: string): Promise<{ build_id: string }> {
	const res = await api.post(u(`/api/sites/${siteID}/build`), { data: {} });
	const body = await jsonOrThrow(res, 'triggerBuild');
	return body;
}

export async function pollBuild(
	api: APIRequestContext,
	siteID: string,
	buildID: string,
	timeoutMs = 90_000
): Promise<{ status: string; build_log: string; error?: string; pages_built?: number }> {
	const start = Date.now();
	while (Date.now() - start < timeoutMs) {
		const res = await api.get(u(`/api/sites/${siteID}/builds/${buildID}/status`));
		if (res.ok()) {
			const body = await res.json();
			const b = body.build ?? body;
			if (b.status === 'success' || b.status === 'failed' || b.status === 'timeout') {
				return b;
			}
		}
		await new Promise((r) => setTimeout(r, 500));
	}
	throw new Error(`build ${buildID} did not finish within ${timeoutMs}ms`);
}

export async function triggerBuildAndWait(
	api: APIRequestContext,
	siteID: string,
	timeoutMs = 90_000
): Promise<{ build_id: string; status: string; build_log: string; error?: string }> {
	const { build_id } = await triggerBuild(api, siteID);
	const final = await pollBuild(api, siteID, build_id, timeoutMs);
	return { build_id, ...final };
}

export async function uploadMedia(
	_api: APIRequestContext,
	siteID: string,
	options: { filename?: string; contentType?: string; bytes?: Buffer; adminCookie?: string } = {}
): Promise<{ id: string; filename: string; variants_json?: unknown; original_path?: string }> {
	const filename = options.filename ?? `${rand('img')}.png`;
	const contentType = options.contentType ?? 'image/png';
	const bytes =
		options.bytes ??
		Buffer.from(
			'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=',
			'base64'
		);
	// The shared `adminApi` context sets `Content-Type: application/json` which
	// kills multipart's auto-generated boundary. Build a fresh context with
	// only the cookie. Caller can pass adminCookie; otherwise we read the file
	// the global setup wrote.
	let cookie = options.adminCookie;
	if (!cookie) {
		const { readFileSync } = await import('node:fs');
		const path = await import('node:path');
		cookie = readFileSync(
			path.resolve(process.cwd(), '.tmp/admin-cookie.txt'),
			'utf8'
		).trim();
	}
	const ctx = await request.newContext({
		extraHTTPHeaders: { Cookie: `${E2E_CONSTANTS.COOKIE_NAME}=${cookie}` }
	});
	try {
		const res = await ctx.post(u(`/api/sites/${siteID}/media`), {
			multipart: {
				file: { name: filename, mimeType: contentType, buffer: bytes }
			}
		});
		const body = await jsonOrThrow(res, 'uploadMedia');
		return body.media ?? body;
	} finally {
		await ctx.dispose();
	}
}

export async function seedDeployTarget(
	api: APIRequestContext,
	siteID: string,
	kind: 'local' | 'rsync' | 'dockyard',
	overrides: Record<string, unknown> = {}
): Promise<DeployTarget> {
	const configs: Record<typeof kind, Record<string, unknown>> = {
		local: { path: `/tmp/${siteID}/atomicsite-deploy-${rand()}`, public_url: 'http://127.0.0.1:9999' },
		rsync: {
			host: '127.0.0.1',
			user: 'tester',
			port: 22022,
			path: `/tmp/atomicsite-rsync-${rand()}`,
			public_url: 'http://127.0.0.1:9998',
			private_key_pem: '-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n-----END OPENSSH PRIVATE KEY-----\n'
		},
		dockyard: {
			// Dockyard validation requires HTTPS URL + UUID server_id. The stub
			// terminates TLS via fixtures/certs/, and the binary trusts it via
			// SSL_CERT_FILE injected by playwright.config.ts.
			dockyard_url: E2E_CONSTANTS.STUB_DOCKYARD,
			api_key: 'e2e-dy-key',
			server_id: '00000000-0000-4000-8000-000000000001',
			domain: 'e2e.example.test',
			host: '127.0.0.1',
			user: 'deploy',
			port: 22022,
			host_path: '/srv/sites/e2e',
			private_key_pem: '-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n-----END OPENSSH PRIVATE KEY-----\n'
		}
	};
	const res = await api.post(u(`/api/sites/${siteID}/deploy-targets`), {
		data: {
			name: rand(`${kind}-target`),
			kind,
			config: { ...configs[kind], ...overrides },
			is_default: true
		}
	});
	const body = await jsonOrThrow(res, 'seedDeployTarget');
	return body.deploy_target ?? body;
}

/**
 * Read recorded calls from a stub server.
 */
export async function getStubCalls(stub: 'cookieproof' | 'crm' | 'dockyard'): Promise<unknown[]> {
	const map = {
		cookieproof: E2E_CONSTANTS.STUB_COOKIEPROOF,
		crm: E2E_CONSTANTS.STUB_CRM,
		dockyard: E2E_CONSTANTS.STUB_DOCKYARD
	};
	// Dockyard stub serves a self-signed cert; trust it for these admin endpoints.
	const ctx = await request.newContext({ ignoreHTTPSErrors: true });
	try {
		const res = await ctx.get(`${map[stub]}/__calls`);
		if (!res.ok()) throw new Error(`stub ${stub} __calls returned ${res.status()}`);
		return await res.json();
	} finally {
		await ctx.dispose();
	}
}

export async function resetStub(stub: 'cookieproof' | 'crm' | 'dockyard'): Promise<void> {
	const map = {
		cookieproof: E2E_CONSTANTS.STUB_COOKIEPROOF,
		crm: E2E_CONSTANTS.STUB_CRM,
		dockyard: E2E_CONSTANTS.STUB_DOCKYARD
	};
	const ctx = await request.newContext({ ignoreHTTPSErrors: true });
	try {
		await ctx.post(`${map[stub]}/__reset`);
	} finally {
		await ctx.dispose();
	}
}
