<script lang="ts" module>
	import type { Site, Page } from '$lib/api/types';

	interface SiteCache {
		sites: Site[];
		fetchedAt: number;
	}

	interface PagesCacheEntry {
		pages: Page[];
		fetchedAt: number;
	}

	const CACHE_TTL_MS = 60_000;

	let sitesCache = $state<SiteCache | null>(null);
	const pagesCache = $state<Record<string, PagesCacheEntry>>({});

	export function clearPaletteCache(): void {
		sitesCache = null;
		for (const key of Object.keys(pagesCache)) {
			delete pagesCache[key];
		}
	}

	export function _sitesCache(): SiteCache | null {
		return sitesCache;
	}

	export function _setSitesCache(next: SiteCache | null): void {
		sitesCache = next;
	}

	export function _pagesCache(): Record<string, PagesCacheEntry> {
		return pagesCache;
	}

	export function _setPagesCache(siteID: string, entry: PagesCacheEntry): void {
		pagesCache[siteID] = entry;
	}

	export const _ttl = CACHE_TTL_MS;
</script>

<script lang="ts">
	import { Dialog as BitsDialog } from 'bits-ui';
	import { goto } from '$app/navigation';
	import {
		Search,
		Globe,
		FileText,
		Layers,
		Plus,
		Hammer,
		Sun,
		LogOut,
		KeyRound,
		ArrowRight,
		BookOpen
	} from 'lucide-svelte';
	import * as sitesApi from '$lib/api/sites';
	import * as pagesApi from '$lib/api/pages';
	import * as buildsApi from '$lib/api/builds';
	import * as authApi from '$lib/api/auth';
	import { currentSite, clearSite } from '$lib/stores/currentSite.svelte';
	import { clearUser } from '$lib/stores/auth.svelte';
	import { toggleTheme } from '$lib/stores/theme.svelte';
	import { toast } from '$lib/stores/toast.svelte';
	import KeyboardHint from './KeyboardHint.svelte';
	import { palette, openPalette, closePalette, togglePalette } from '$lib/stores/commandPalette.svelte';

	type ResultCategory = 'Sites' | 'Pages' | 'Sections' | 'Actions';

	interface CommandResult {
		id: string;
		category: ResultCategory;
		label: string;
		secondary?: string;
		hint?: string[];
		run: () => void | Promise<void>;
	}

	let query = $state<string>('');
	let highlight = $state<number>(0);
	let inputEl = $state<HTMLInputElement | null>(null);
	let listEl = $state<HTMLDivElement | null>(null);
	let loading = $state<boolean>(false);

	let sitesData = $state<Site[]>([]);
	let pagesData = $state<Page[]>([]);

	const isMac = $derived(
		typeof navigator !== 'undefined' && navigator.platform.includes('Mac')
	);
	const cmdLabel = $derived(isMac ? '⌘' : 'Ctrl');

	// Global Cmd+K / Ctrl+K listener
	$effect(() => {
		if (typeof window === 'undefined') return;
		function onKeydown(e: KeyboardEvent) {
			const mod = isMac ? e.metaKey : e.ctrlKey;
			if (mod && (e.key === 'k' || e.key === 'K')) {
				e.preventDefault();
				togglePalette();
			}
		}
		window.addEventListener('keydown', onKeydown);
		return () => {
			window.removeEventListener('keydown', onKeydown);
		};
	});

	// Load data when palette opens (with caching)
	$effect(() => {
		if (!palette.open) return;
		void loadData();
	});

	// Reset query and focus input on open
	$effect(() => {
		if (palette.open) {
			query = '';
			highlight = 0;
			queueMicrotask(() => {
				inputEl?.focus();
			});
		}
	});

	async function loadData(): Promise<void> {
		const now = Date.now();
		const cachedSites = _sitesCache();
		const siteFresh = cachedSites && now - cachedSites.fetchedAt < _ttl;
		const siteID = currentSite.value?.id ?? null;
		const cachedPages = siteID ? _pagesCache()[siteID] : null;
		const pagesFresh = cachedPages && now - cachedPages.fetchedAt < _ttl;

		if (siteFresh) {
			sitesData = cachedSites.sites;
		}
		if (siteID && pagesFresh) {
			pagesData = cachedPages.pages;
		}
		if (siteFresh && (!siteID || pagesFresh)) {
			return;
		}

		loading = true;
		try {
			if (!siteFresh) {
				const res = await sitesApi.list();
				sitesData = res.sites ?? [];
				_setSitesCache({ sites: sitesData, fetchedAt: now });
			}
			if (siteID && !pagesFresh) {
				const res = await pagesApi.list(siteID);
				pagesData = res.pages ?? [];
				_setPagesCache(siteID, { pages: pagesData, fetchedAt: now });
			} else if (!siteID) {
				pagesData = [];
			}
		} catch {
			// Swallow; palette still works with whatever data we have.
		} finally {
			loading = false;
		}
	}

	function navigate(href: string): void {
		closePalette();
		void goto(href);
	}

	async function triggerCurrentBuild(): Promise<void> {
		const site = currentSite.value;
		if (!site) {
			toast.error('Open a site first.');
			return;
		}
		closePalette();
		try {
			await buildsApi.triggerBuild(site.id);
			toast.success('Build triggered.');
			void goto(`/sites/${site.id}/build`);
		} catch (err) {
			toast.error(err instanceof Error ? err.message : 'Failed to trigger build');
		}
	}

	async function signOut(): Promise<void> {
		closePalette();
		try {
			await authApi.logout();
		} catch {
			// best effort
		}
		clearUser();
		clearSite();
		void goto('/login');
	}

	const sectionItems = $derived.by(() => {
		const site = currentSite.value;
		if (!site) return [] as Array<{ label: string; href: string }>;
		const id = site.id;
		return [
			{ label: 'Overview', href: `/sites/${id}` },
			{ label: 'Pages', href: `/sites/${id}/pages` },
			{ label: 'Branding', href: `/sites/${id}/branding` },
			{ label: 'Components', href: `/sites/${id}/components` },
			{ label: 'CSS classes', href: `/sites/${id}/css-classes` },
			{ label: 'Knowledgebase', href: `/sites/${id}/knowledgebase` },
			{ label: 'Guardrails', href: `/sites/${id}/guardrails` },
			{ label: 'Media', href: `/sites/${id}/media` },
			{ label: 'Build', href: `/sites/${id}/build` },
			{ label: 'Settings', href: `/sites/${id}/settings` },
			{ label: 'Agent keys', href: `/sites/${id}/agent-keys` }
		];
	});

	const allResults = $derived.by<CommandResult[]>(() => {
		const out: CommandResult[] = [];
		for (const s of sitesData) {
			out.push({
				id: `site:${s.id}`,
				category: 'Sites',
				label: s.name || s.slug,
				secondary: s.domain || s.slug,
				run: () => navigate(`/sites/${s.id}`)
			});
		}
		const site = currentSite.value;
		if (site) {
			for (const p of pagesData) {
				out.push({
					id: `page:${p.id}`,
					category: 'Pages',
					label: p.title || p.slug || 'Untitled',
					secondary: p.slug ? `/${p.slug}` : undefined,
					run: () => navigate(`/sites/${site.id}/pages/${p.id}`)
				});
			}
			for (const item of sectionItems) {
				out.push({
					id: `section:${item.href}`,
					category: 'Sections',
					label: item.label,
					secondary: item.href,
					run: () => navigate(item.href)
				});
			}
			out.push({
				id: 'action:design-critique',
				category: 'Actions',
				label: 'View design critique',
				secondary: 'Latest build, AntiPatterns + slop-term findings',
				run: () => navigate(`/sites/${site.id}/evaluations`)
			});
		}
		out.push({
			id: 'action:new-site',
			category: 'Actions',
			label: 'New site',
			secondary: 'Create a new site',
			run: () => navigate('/sites/new')
		});
		out.push({
			id: 'action:docs',
			category: 'Actions',
			label: 'Documentation',
			secondary: 'System overview, features, and agent API',
			run: () => navigate('/docs')
		});
		out.push({
			id: 'action:docs-architecture',
			category: 'Actions',
			label: 'Architecture',
			secondary: 'How Atomic Site works',
			run: () => navigate('/docs/architecture')
		});
		out.push({
			id: 'action:docs-features',
			category: 'Actions',
			label: 'Features',
			secondary: 'Every admin feature',
			run: () => navigate('/docs/features')
		});
		out.push({
			id: 'action:docs-agent-api',
			category: 'Actions',
			label: 'Agent API',
			secondary: 'Endpoint catalogue for BYOAI',
			run: () => navigate('/docs/agent-api')
		});
		out.push({
			id: 'action:docs-connect-your-agent',
			category: 'Actions',
			label: 'Connect your agent',
			secondary: 'Wire Claude CLI, Cursor, or any agent',
			run: () => navigate('/docs/connect-your-agent')
		});
		out.push({
			id: 'action:docs-personalization',
			category: 'Actions',
			label: 'Personalization',
			secondary: 'Bidirectional CRM loop, conditional blocks, identity tiers',
			run: () => navigate('/docs/personalization')
		});
		out.push({
			id: 'action:docs-apps',
			category: 'Actions',
			label: 'Apps & integrations',
			secondary: 'External connectors plus platform-level mechanics',
			run: () => navigate('/docs/apps')
		});
		if (site) {
			out.push({
				id: 'action:trigger-build',
				category: 'Actions',
				label: 'Trigger build for current site',
				secondary: site.name,
				run: () => void triggerCurrentBuild()
			});
		}
		out.push({
			id: 'action:toggle-theme',
			category: 'Actions',
			label: 'Toggle theme',
			secondary: 'Switch between light and dark',
			run: () => {
				toggleTheme();
				closePalette();
			}
		});
		out.push({
			id: 'action:sign-out',
			category: 'Actions',
			label: 'Sign out',
			secondary: 'End your session',
			run: () => void signOut()
		});
		return out;
	});

	const filtered = $derived.by<CommandResult[]>(() => {
		const q = query.trim().toLowerCase();
		if (!q) return allResults;
		return allResults.filter((r) => {
			const haystack = `${r.label} ${r.secondary ?? ''} ${r.category}`.toLowerCase();
			return haystack.includes(q);
		});
	});

	type Group = { category: ResultCategory; items: CommandResult[]; startIndex: number };

	const grouped = $derived.by<Group[]>(() => {
		const order: ResultCategory[] = ['Sites', 'Pages', 'Sections', 'Actions'];
		const groups: Group[] = [];
		let runningIndex = 0;
		for (const cat of order) {
			const items = filtered.filter((r) => r.category === cat);
			if (items.length === 0) continue;
			groups.push({ category: cat, items, startIndex: runningIndex });
			runningIndex += items.length;
		}
		return groups;
	});

	$effect(() => {
		// Reset highlight when query changes; clamp when results shrink.
		const _ = query;
		const total = filtered.length;
		if (highlight >= total) highlight = total === 0 ? 0 : total - 1;
		if (highlight < 0) highlight = 0;
	});

	function categoryIcon(cat: ResultCategory) {
		switch (cat) {
			case 'Sites':
				return Globe;
			case 'Pages':
				return FileText;
			case 'Sections':
				return Layers;
			case 'Actions':
				return ArrowRight;
		}
	}

	function actionIcon(r: CommandResult) {
		if (r.id === 'action:new-site') return Plus;
		if (r.id === 'action:trigger-build') return Hammer;
		if (r.id === 'action:toggle-theme') return Sun;
		if (r.id === 'action:sign-out') return LogOut;
		if (r.id.startsWith('action:docs')) return BookOpen;
		if (r.category === 'Sections' && r.label === 'Agent keys') return KeyRound;
		return categoryIcon(r.category);
	}

	function onKeyDown(e: KeyboardEvent): void {
		if (e.key === 'ArrowDown') {
			e.preventDefault();
			const total = filtered.length;
			if (total === 0) return;
			highlight = (highlight + 1) % total;
			scrollHighlightIntoView();
		} else if (e.key === 'ArrowUp') {
			e.preventDefault();
			const total = filtered.length;
			if (total === 0) return;
			highlight = (highlight - 1 + total) % total;
			scrollHighlightIntoView();
		} else if (e.key === 'Enter') {
			e.preventDefault();
			const target = filtered[highlight];
			if (target) {
				void target.run();
			}
		} else if (e.key === 'Escape') {
			e.preventDefault();
			closePalette();
		}
	}

	function scrollHighlightIntoView(): void {
		queueMicrotask(() => {
			const el = listEl?.querySelector<HTMLElement>(`[data-cmd-index="${highlight}"]`);
			el?.scrollIntoView({ block: 'nearest' });
		});
	}

	function handleOpenChange(next: boolean): void {
		if (next) {
			openPalette();
		} else {
			closePalette();
		}
	}
</script>

<BitsDialog.Root open={palette.open} onOpenChange={handleOpenChange}>
	<BitsDialog.Portal>
		<BitsDialog.Overlay
			class="fixed inset-0 z-50 bg-black/40 backdrop-blur-sm data-[state=open]:animate-fadeIn"
		/>
		<BitsDialog.Content
			class="fixed left-1/2 top-[15%] z-50 w-[calc(100%-2rem)] max-w-2xl -translate-x-1/2 rounded-2xl border border-border/50 bg-bg-surface shadow-modal focus:outline-none data-[state=open]:animate-scaleIn"
			onkeydown={onKeyDown}
		>
			<BitsDialog.Title class="sr-only">Command palette</BitsDialog.Title>
			<BitsDialog.Description class="sr-only">
				Search sites, pages, sections, and quick actions.
			</BitsDialog.Description>

			<div class="flex items-center gap-3 border-b border-border-light px-4 py-3">
				<Search class="h-4 w-4 shrink-0 text-text-muted" />
				<input
					bind:this={inputEl}
					bind:value={query}
					type="text"
					placeholder="Search sites, pages, actions..."
					class="flex-1 bg-transparent text-[14px] text-text-primary placeholder:text-text-muted focus:outline-none"
					aria-label="Command palette search"
					autocomplete="off"
					spellcheck="false"
				/>
				<KeyboardHint keys={['Esc']} />
			</div>

			<div bind:this={listEl} role="listbox" class="max-h-[60vh] overflow-y-auto py-2">
				{#if loading && filtered.length === 0}
					<div class="px-4 py-6 text-[13px] text-text-muted">Loading...</div>
				{:else if filtered.length === 0}
					<div class="px-4 py-6 text-[13px] text-text-muted">
						No results for "{query}".
					</div>
				{:else}
					{#each grouped as group (group.category)}
						<div class="px-2 pb-1 pt-2">
							<div
								class="px-3 pb-1 text-[10px] font-medium uppercase tracking-wider text-text-muted"
							>
								{group.category}
							</div>
							{#each group.items as result, localIdx (result.id)}
								{@const flatIdx = group.startIndex + localIdx}
								{@const Icon = actionIcon(result)}
								{@const active = flatIdx === highlight}
								<button
									type="button"
									role="option"
									data-cmd-index={flatIdx}
									aria-selected={active}
									onmousemove={() => (highlight = flatIdx)}
									onclick={() => void result.run()}
									class="flex w-full items-center gap-3 rounded-md px-3 py-2 text-left transition-colors {active
										? 'bg-bg-hover'
										: 'hover:bg-bg-hover/60'}"
								>
									<Icon class="h-4 w-4 shrink-0 text-text-muted" />
									<span class="flex min-w-0 flex-1 items-baseline gap-2">
										<span class="truncate text-[13px] text-text-primary">{result.label}</span>
										{#if result.secondary}
											<span class="truncate font-mono text-[11px] text-text-muted">
												{result.secondary}
											</span>
										{/if}
									</span>
									{#if active}
										<span class="shrink-0 text-[10px] text-text-muted">Enter</span>
									{/if}
								</button>
							{/each}
						</div>
					{/each}
				{/if}
			</div>

			<div
				class="flex items-center justify-between gap-3 border-t border-border-light px-4 py-2 text-[11px] text-text-muted"
			>
				<div class="flex items-center gap-3">
					<span class="inline-flex items-center gap-1">
						<KeyboardHint keys={['↑', '↓']} />
						<span>Navigate</span>
					</span>
					<span class="inline-flex items-center gap-1">
						<KeyboardHint keys={['Enter']} />
						<span>Select</span>
					</span>
				</div>
				<KeyboardHint keys={[cmdLabel, 'K']} />
			</div>
		</BitsDialog.Content>
	</BitsDialog.Portal>
</BitsDialog.Root>
