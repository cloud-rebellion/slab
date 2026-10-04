// Optional, dependency-free browser error reporting for the Flare dashboard.
//
// This deliberately stays small and disabled unless PUBLIC_FLARE_DSN is set.
// A browser DSN is an ingest credential by design, so the server's per-DSN
// rate limit and project alert rules remain the control plane. Do not put a
// member API key or any other secret in this module.

export interface FlareBrowserCaptureOptions {
	dsn?: string;
	environment?: string;
	release?: string;
	maxEventsPerMinute?: number;
	captureResourceErrors?: boolean;
}

interface ParsedDsn {
	endpoint: string;
}

interface StackFrame {
	filename?: string;
	function?: string;
	lineno?: number;
	colno?: number;
	in_app?: boolean;
}

const maxTextBytes = 2048;
const maxFrames = 32;
const duplicateWindowMs = 10_000;
const defaultMaxEventsPerMinute = 20;
const browserCanaryPages = new WeakSet<Window>();
const browserCanaryMessage = 'Atomicsite Flare browser source-map canary v1';
function boundedText(value: unknown, fallback: string): string {
	const text = typeof value === 'string' ? value : fallback;
	const scrubbed = text
		.replace(/([?&](?:token|key|secret|password|authorization|code)=)[^&\s]+/gi, '$1[REDACTED]')
		.replace(/((?:password|secret|token|api[_-]?key|authorization|cookie|email)\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^,}\s]+)/gi, '$1[REDACTED]')
		.replace(/\b(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]+/gi, '[REDACTED]')
		.replace(/\b(?:sk|ghp|glpat|xoxb|xoxp|AIza)[A-Za-z0-9._-]{8,}\b/g, '[REDACTED]')
		.replace(/\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b/gi, '[REDACTED_EMAIL]');
	return scrubbed.length > maxTextBytes ? `${scrubbed.slice(0, maxTextBytes)}…` : scrubbed;
}

// Browser paths are untrusted. A nonnumeric signing token or customer slug can
// look like an ordinary word, so only these fixed route words may cross the
// public DSN boundary. Everything else becomes a template segment.
const safeRouteSegments = new Set([
	'_app', 'accept-invite', 'account', 'accounts', 'admin', 'alerts', 'api', 'app',
	'assets', 'auth', 'build', 'business-health', 'callbacks', 'chunks', 'connectors',
	'contacts', 'contracts', 'customers', 'dashboards', 'deals', 'docs', 'documents',
	'dsn', 'entry', 'files', 'forgot-password', 'immutable', 'integrations', 'issues',
	'jobs', 'login', 'logout', 'logs', 'members', 'metrics', 'monitors', 'nodes',
	'overview', 'pipelines', 'profile', 'projects', 'public', 'register', 'releases',
	'reset-password', 'runs', 'search', 'settings', 'sign', 'spans', 'static', 'tasks',
	'teams', 'traces', 'users', 'verify', 'webhooks', 'workflows'
]);

/** Return a bounded route template without arbitrary path segments. */
export function scrubFlareBrowserPath(raw: string, maxLength = 512): string {
	let path = raw;
	try {
		// Decode at most twice so encoded separators cannot hide path segments.
		for (let i = 0; i < 2; i++) {
			const decoded = decodeURIComponent(path);
			if (decoded === path) break;
			path = decoded;
		}
	} catch {
		// An invalid escape is still safe because unknown segments are replaced.
	}
	const segments = path.split('/').filter(Boolean).slice(0, 12);
	const template = segments.map((segment) => safeRouteSegments.has(segment.toLowerCase())
		? segment.toLowerCase() : ':id');
	return (`/${template.join('/')}`).slice(0, Math.max(1, maxLength));
}

function safePage(): string | undefined {
	if (typeof location === 'undefined') return undefined;
	// Query strings, fragments, and arbitrary path segments can all carry tokens.
	const path = scrubFlareBrowserPath(location.pathname);
	return `${location.origin}${path}`;
}

function parseDsn(raw: string): ParsedDsn | null {
	try {
		const url = new URL(raw.trim());
		if (url.protocol !== 'http:' && url.protocol !== 'https:') return null;
		if (!url.username || url.password) return null;
		const path = url.pathname.replace(/\/+$/, '');
		const slash = path.lastIndexOf('/');
		const projectID = path.slice(slash + 1);
		if (!/^[A-Za-z0-9_-]{1,64}$/.test(projectID)) return null;
		const basePath = slash > 0 ? path.slice(0, slash) : '';
		const key = decodeURIComponent(url.username);
		if (!/^[A-Za-z0-9_-]{1,64}$/.test(key)) return null;
		const endpoint = `${url.origin}${basePath}/api/${encodeURIComponent(projectID)}/events?sentry_key=${encodeURIComponent(key)}&sentry_version=7`;
		return { endpoint };
	} catch {
		return null;
	}
}

function eventID(): string {
	if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
		return crypto.randomUUID().replace(/-/g, '');
	}
	if (typeof crypto !== 'undefined' && typeof crypto.getRandomValues === 'function') {
		const bytes = new Uint8Array(16);
		crypto.getRandomValues(bytes);
		return Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('');
	}
	return `${Date.now().toString(16)}${Math.random().toString(16).slice(2)}`.slice(0, 32).padEnd(32, '0');
}

// A stack frame needs a stable asset identity to select its release's source
// map, but script URLs can carry customer IDs or signing tokens. Only known
// build output shapes on the current app origin qualify. An arbitrary .js URL
// from a route, a sourceURL comment, or another origin becomes `unknown`.
// The upload helper in examples/browser/source-map-name.mjs uses this same
// `origin + pathname` input so CI can name the matching artifact.
const compiledScriptPaths = [
	/^\/_app\/immutable\/(?:chunks|entry|nodes)\/(?:[A-Za-z0-9_-]{1,64}\.)?[A-Za-z0-9_-]{8,80}\.m?js$/,
	/^\/_astro\/(?:[A-Za-z0-9_-]{1,64}\.)?[A-Za-z0-9_-]{8,80}\.m?js$/,
	/^\/(?:assets|build\/assets|static\/js)\/[A-Za-z0-9._-]{1,64}[-.][A-Za-z0-9_-]{8,80}\.m?js$/
];

function wasLoadedAsScript(url: URL): boolean {
	// A sourceURL inside eval can claim any filename. Require evidence that the
	// browser loaded this exact URL as a script or module preload on this page.
	const loadedURL = `${url.origin}${url.pathname}${url.search}`;
	if (typeof document !== 'undefined' && document.scripts) {
		for (const script of Array.from(document.scripts).slice(0, 256)) {
			try {
				const src = new URL(script.src);
				if (`${src.origin}${src.pathname}${src.search}` === loadedURL) return true;
			} catch {
				// Inline scripts have no src.
			}
		}
	}
	if (typeof performance !== 'undefined' && typeof performance.getEntriesByName === 'function') {
		return performance.getEntriesByName(loadedURL, 'resource').some((entry) =>
			['script', 'link'].includes((entry as PerformanceResourceTiming).initiatorType));
	}
	return false;
}

export async function flareBrowserAssetName(raw: string, pageOrigin = typeof location === 'undefined' ? '' : location.origin): Promise<string | undefined> {
	try {
		const url = new URL(raw, typeof location === 'undefined' ? undefined : location.href);
		if (!['http:', 'https:'].includes(url.protocol) || url.origin !== pageOrigin ||
			!compiledScriptPaths.some((pattern) => pattern.test(url.pathname)) || !wasLoadedAsScript(url)) return undefined;
		if (url.pathname.length > 2048 || typeof crypto === 'undefined' || !crypto.subtle) return undefined;
		const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(url.origin + url.pathname));
		return `flare-asset-sha256:${Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, '0')).join('')}`;
	} catch {
		return undefined;
	}
}

async function parseStack(stack: string | undefined): Promise<StackFrame[]> {
	if (!stack) return [];
	const matched: Array<{ fn: string; file: string; lineno: number; colno?: number }> = [];
	for (const line of stack.split('\n').slice(0, maxFrames)) {
		const trimmed = line.trim();
		let match = trimmed.match(/^at\s+(.*?)\s+\(?(.+?):(\d+)(?::(\d+))?\)?$/);
		if (!match) match = trimmed.match(/^(.*?)@(.+?):(\d+)(?::(\d+))?$/);
		if (!match) continue;
		const [, fn, file, lineNumber, columnNumber] = match;
		matched.push({ fn: fn || '<anonymous>', file: file ?? '', lineno: Number(lineNumber), colno: columnNumber ? Number(columnNumber) : undefined });
	}
	return Promise.all(matched.map(async ({ fn, file, lineno, colno }) => ({
		filename: await flareBrowserAssetName(file) ?? 'unknown',
		function: boundedText(fn, '<anonymous>'),
		lineno,
		colno,
		in_app: true
	})));
}

function errorDetails(value: unknown): { type: string; message: string; stack?: string } {
	if (value instanceof Error) {
		return {
			type: boundedText(value.name, 'Error'),
			message: boundedText(value.message, 'Unhandled browser error'),
			stack: value.stack
		};
	}
	if (typeof value === 'string') return { type: 'UnhandledRejection', message: boundedText(value, 'Unhandled browser error') };
	try {
		return { type: 'UnhandledRejection', message: boundedText(JSON.stringify(value), 'Unhandled browser error') };
	} catch {
		return { type: 'UnhandledRejection', message: 'Unhandled browser error' };
	}
}

function installSender(dsn: ParsedDsn, options: FlareBrowserCaptureOptions): (value: unknown, stack?: string) => void {
	const maxEvents = Math.max(1, Math.min(options.maxEventsPerMinute ?? defaultMaxEventsPerMinute, 100));
	let windowStarted = Date.now();
	let sentInWindow = 0;
	const recent = new Map<string, number>();

	return (value, stack) => {
		const now = Date.now();
		if (now - windowStarted >= 60_000) {
			windowStarted = now;
			sentInWindow = 0;
			recent.clear();
		}
		if (sentInWindow >= maxEvents) return;
		const details = errorDetails(value);
		const signature = `${details.type}:${details.message}:${stack ?? details.stack ?? ''}`;
		const previous = recent.get(signature);
		if (previous && now - previous < duplicateWindowMs) return;
		recent.set(signature, now);
		sentInWindow++;
		const page = safePage();

		void parseStack(stack ?? details.stack).then((frames) => {
			const payload = JSON.stringify({
				event_id: eventID(),
				platform: 'javascript',
				level: 'error',
				environment: boundedText(options.environment, 'production'),
				release: boundedText(options.release, ''),
				timestamp: now / 1000,
				message: details.message,
				exception: {
					values: [{
						type: details.type,
						value: details.message,
						stacktrace: { frames }
					}]
				},
					request: page ? { url: page } : undefined,
				tags: { source: 'flare-browser-capture' }
			});

			// text/plain is a CORS-safelisted request, so cross-origin ingest does not
			// need a preflight. sendBeacon is the most reliable path during unload;
			// keepalive fetch covers browsers that reject the beacon queue.
			const body = new Blob([payload], { type: 'text/plain' });
			let queued = false;
			if (typeof navigator !== 'undefined' && typeof navigator.sendBeacon === 'function') {
				queued = navigator.sendBeacon(dsn.endpoint, body);
			}
			if (!queued && typeof fetch === 'function') {
				void fetch(dsn.endpoint, {
					method: 'POST',
					body: payload,
					headers: { 'Content-Type': 'text/plain' },
					mode: 'no-cors',
					credentials: 'omit',
					keepalive: true
				}).catch(() => {
					// Reporting must never surface as an application error or recurse.
				});
			}
		}).catch(() => {
			// A stack parser or browser crypto failure cannot become an app error.
		});
	};
}

interface FlareBoundaryEventDetail {
	error?: unknown;
	componentStack?: string;
}

/**
 * Install bounded browser error capture once.
 *
 * The bridge intentionally uses the Sentry envelope shape but no browser SDK:
 * this keeps first-party static frontends small and avoids putting an API key
 * in the bundle. The DSN is an ingest-only credential and is rate limited by
 * Flare. React error boundaries can report handled render failures by
 * dispatching `flare:exception` with `{ error, componentStack }`.
 * An operator can dispatch `flare:browser-canary` to report one fixed,
 * handled error per page. Its stack originates in this loaded client module.
 */
export function installFlareBrowserCapture(options: FlareBrowserCaptureOptions): () => void {
	if (typeof window === 'undefined' || !options.dsn?.trim()) return () => {};
	const dsn = parseDsn(options.dsn);
	if (!dsn) return () => {};
	const marker = '__flareBrowserCaptureInstalled';
	if ((window as Window & { [marker]?: boolean })[marker]) return () => {};
	(window as Window & { [marker]?: boolean })[marker] = true;
	const send = installSender(dsn, options);
	const onError = (event: ErrorEvent) => {
		// Resource failures also arrive as ErrorEvents. The capture-phase listener
		// below records those with a sanitized origin/path; do not emit a second
		// generic "Unhandled browser error" for the same failure.
		if (event.target instanceof Element) return;
		send(event.error ?? event.message, event.error?.stack);
	};
	const onRejection = (event: PromiseRejectionEvent) => send(event.reason);
	const onResourceError = (event: Event) => {
		const target = event.target;
		if (!(target instanceof Element)) return;
		const element = target.tagName.toLowerCase();
		const raw = element === 'script' ? target.getAttribute('src') :
			element === 'link' ? target.getAttribute('href') : target.getAttribute('src');
		let resource = element;
		try {
			if (raw) {
				const url = new URL(raw, location.href);
				resource = `${element}:${url.origin}${scrubFlareBrowserPath(url.pathname)}`.slice(0, 512);
			}
		} catch {
			resource = element;
		}
		send(new Error(`Failed to load browser resource (${resource})`));
	};
	const onBoundary = (event: Event) => {
		const detail = (event as CustomEvent<FlareBoundaryEventDetail>).detail;
		if (detail) send(detail.error ?? new Error('React error boundary caught an error'), detail.componentStack);
	};
	const onCanary = () => {
		if (browserCanaryPages.has(window)) return;
		browserCanaryPages.add(window);
		const error = new Error(browserCanaryMessage);
		error.name = 'FlareBrowserCanary';
		send(error);
	};
	window.addEventListener('error', onError);
	window.addEventListener('unhandledrejection', onRejection);
	if (options.captureResourceErrors !== false) window.addEventListener('error', onResourceError, true);
	window.addEventListener('flare:exception', onBoundary);
	window.addEventListener('flare:browser-canary', onCanary);
	return () => {
		window.removeEventListener('error', onError);
		window.removeEventListener('unhandledrejection', onRejection);
		if (options.captureResourceErrors !== false) window.removeEventListener('error', onResourceError, true);
		window.removeEventListener('flare:exception', onBoundary);
		window.removeEventListener('flare:browser-canary', onCanary);
		delete (window as Window & { [marker]?: boolean })[marker];
	};
}
