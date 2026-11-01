/**
 * Stub servers for external integrations. Started by playwright.config.ts as
 * the first webServer entry. Records every call so specs can assert on them.
 *
 * - CookieProof API on STUB_COOKIEPROOF_PORT (provisioning + receipt)
 * - BrightCRM webhook on STUB_CRM_PORT (HMAC-signed activity events)
 * - Dockyard deploy API on STUB_DOCKYARD_PORT (route registration)
 *
 * State is exposed via /__calls (GET, JSON) and resettable via /__reset (POST).
 */
import { serve, type Server } from 'bun';
import { createHmac, timingSafeEqual } from 'node:crypto';
import { readFileSync } from 'node:fs';

type Call = {
	method: string;
	path: string;
	headers: Record<string, string>;
	body: unknown;
	hmacOk?: boolean;
	at: number;
};

const cpPort = Number(process.env.STUB_COOKIEPROOF_PORT ?? 18291);
const crmPort = Number(process.env.STUB_CRM_PORT ?? 18292);
const dyPort = Number(process.env.STUB_DOCKYARD_PORT ?? 18293);
const crmSecret = process.env.BRIGHTCRM_WEBHOOK_SECRET ?? 'e2e-crm-hmac-secret';

const calls: Record<'cookieproof' | 'crm' | 'dockyard', Call[]> = {
	cookieproof: [],
	crm: [],
	dockyard: []
};

async function readBody(req: Request): Promise<unknown> {
	const ct = req.headers.get('content-type') ?? '';
	const text = await req.text();
	if (!text) return null;
	if (ct.includes('application/json')) {
		try {
			return JSON.parse(text);
		} catch {
			return text;
		}
	}
	return text;
}

function headersToRecord(h: Headers): Record<string, string> {
	const out: Record<string, string> = {};
	h.forEach((v, k) => {
		out[k] = v;
	});
	return out;
}

function jsonOk(data: unknown = { ok: true }, status = 200): Response {
	return new Response(JSON.stringify(data), {
		status,
		headers: { 'content-type': 'application/json' }
	});
}

type StubOpts = { tls?: boolean };

const certFile = process.env.SLAB_E2E_TLS_CERT_FILE;
const keyFile = process.env.SLAB_E2E_TLS_KEY_FILE;
if (!certFile || !keyFile) {
	throw new Error('Missing temporary TLS fixture from the Playwright configuration');
}

function makeServer(
	label: keyof typeof calls,
	port: number,
	handler: (req: Request) => Promise<Response>,
	opts: StubOpts = {}
): Server {
	const cfg: Parameters<typeof serve>[0] = {
		port,
		hostname: '127.0.0.1',
		async fetch(req) {
			const url = new URL(req.url);
			if (url.pathname === '/__health') return new Response('ok');
			if (url.pathname === '/__calls') return jsonOk(calls[label]);
			if (url.pathname === '/__reset' && req.method === 'POST') {
				calls[label] = [];
				return jsonOk();
			}
			return handler(req);
		}
	};
	if (opts.tls) {
		cfg.tls = {
			cert: readFileSync(certFile),
			key: readFileSync(keyFile)
		};
	}
	return serve(cfg);
}

const cookieproof = makeServer('cookieproof', cpPort, async (req) => {
	const url = new URL(req.url);
	const body = await readBody(req);
	calls.cookieproof.push({
		method: req.method,
		path: url.pathname,
		headers: headersToRecord(req.headers),
		body,
		at: Date.now()
	});
	if (req.method === 'POST' && url.pathname === '/api/sites') {
		return jsonOk({
			id: `cp-${Math.random().toString(36).slice(2, 10)}`,
			site_id: (body as { site_id?: string })?.site_id ?? 'unknown',
			snippet_url: 'http://127.0.0.1:' + cpPort + '/snippet.js',
			created_at: new Date().toISOString()
		});
	}
	return jsonOk();
});

const crm = makeServer('crm', crmPort, async (req) => {
	const url = new URL(req.url);
	const rawBytes = Buffer.from(await req.arrayBuffer());
	const rawText = rawBytes.toString('utf8');
	let body: unknown = rawText;
	try {
		body = rawText ? JSON.parse(rawText) : null;
	} catch {}
	const sig = req.headers.get('x-atomicsite-signature') ?? '';
	let hmacOk = false;
	if (sig && rawText) {
		const expected = createHmac('sha256', crmSecret).update(rawBytes).digest('hex');
		try {
			hmacOk =
				expected.length === sig.length &&
				timingSafeEqual(Buffer.from(expected, 'utf8'), Buffer.from(sig, 'utf8'));
		} catch {
			hmacOk = false;
		}
	}
	calls.crm.push({
		method: req.method,
		path: url.pathname,
		headers: headersToRecord(req.headers),
		body,
		hmacOk,
		at: Date.now()
	});
	return jsonOk();
});

const dockyard = makeServer(
	'dockyard',
	dyPort,
	async (req) => {
		const url = new URL(req.url);
		const body = await readBody(req);
		calls.dockyard.push({
			method: req.method,
			path: url.pathname,
			headers: headersToRecord(req.headers),
			body,
			at: Date.now()
		});
		// Real Dockyard surface: POST /api/servers/{id}/proxy/routes (create/update Caddy route)
		// and POST /api/servers/{id}/proxy/apply (reload Caddy). Older /api/routes path is also
		// accepted for forward-compat tests.
		if (
			req.method === 'POST' &&
			(url.pathname.startsWith('/api/routes') ||
				/^\/api\/servers\/[^/]+\/proxy\/routes$/.test(url.pathname))
		) {
			return jsonOk({
				id: `route-${Math.random().toString(36).slice(2, 10)}`,
				created_at: new Date().toISOString()
			});
		}
		if (req.method === 'POST' && /^\/api\/servers\/[^/]+\/proxy\/apply$/.test(url.pathname)) {
			return jsonOk({ applied: true });
		}
		return jsonOk();
	},
	{ tls: true }
);

console.log(`[stubs] cookieproof :${cpPort} crm :${crmPort} dockyard :${dyPort}`);

const shutdown = () => {
	cookieproof.stop(true);
	crm.stop(true);
	dockyard.stop(true);
	process.exit(0);
};
process.on('SIGINT', shutdown);
process.on('SIGTERM', shutdown);
