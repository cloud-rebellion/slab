<script lang="ts">
	import { Popover } from 'bits-ui';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { ChevronsUpDown, Check, Plus, Search } from 'lucide-svelte';
	import * as sitesApi from '$lib/api/sites';
	import type { Site } from '$lib/api/types';
	import { currentSite, lastSiteID, setSite } from '$lib/stores/currentSite.svelte';

	let {
		currentSiteId = null
	}: {
		currentSiteId?: string | null;
	} = $props();

	let open = $state(false);
	let sites = $state<Site[]>([]);
	let loading = $state(false);
	let loaded = $state(false);
	let query = $state('');
	let listEl = $state<HTMLDivElement | null>(null);

	const filtered = $derived(
		query.trim() === ''
			? sites
			: sites.filter((s) => {
					const q = query.toLowerCase();
					return s.name.toLowerCase().includes(q) || s.slug.toLowerCase().includes(q);
				})
	);

	const activeId = $derived(currentSiteId ?? currentSite.value?.id ?? lastSiteID());
	const activeName = $derived.by(() => {
		if (currentSite.value && currentSite.value.id === activeId) return currentSite.value.name;
		const found = sites.find((s) => s.id === activeId);
		return found?.name ?? null;
	});

	async function ensureLoaded() {
		if (loaded || loading) return;
		loading = true;
		try {
			const res = await sitesApi.list();
			sites = res.sites ?? [];
			loaded = true;
		} catch {
			sites = [];
		} finally {
			loading = false;
		}
	}

	// Sections under /sites/{id}/* whose URL is safe to preserve verbatim
	// when switching sites (no entity-ID tail that'd 404 on the new site).
	const PRESERVABLE_SITE_SECTIONS = new Set([
		'pages',
		'branding',
		'media',
		'build',
		'analytics',
		'settings',
		'agent-keys',
		'components',
		'css-classes',
		'knowledgebase',
		'guardrails'
	]);

	function selectSite(site: Site) {
		setSite(site);
		open = false;
		query = '';

		// If the user is on a per-site URL, swap the site id in place and
		// preserve the section so they don't lose context. If they are on a
		// global route (/docs, /account, /members) just update the store and
		// stay where they are - those pages re-derive against currentSite.
		const path = page.url.pathname;
		const m = path.match(/^\/sites\/[^/]+(?:\/([^/]+))?/);
		if (m) {
			const section = m[1];
			if (section && PRESERVABLE_SITE_SECTIONS.has(section)) {
				void goto(`/sites/${site.id}/${section}`);
			} else {
				void goto(`/sites/${site.id}`);
			}
		}
		// else: stay on the current global route. The page reads currentSite
		// from the store, so links + bundle-binding pick up the new site.
	}

	function createNew() {
		open = false;
		query = '';
		goto('/sites/new');
	}

	function handleOpenChange(next: boolean) {
		open = next;
		if (next) {
			void ensureLoaded();
		} else {
			query = '';
		}
	}

	function handleListKeydown(e: KeyboardEvent) {
		if (!listEl) return;
		const buttons = Array.from(
			listEl.querySelectorAll<HTMLButtonElement>('button[data-site-item]')
		);
		if (buttons.length === 0) return;
		const idx = buttons.indexOf(document.activeElement as HTMLButtonElement);
		if (e.key === 'ArrowDown') {
			e.preventDefault();
			const next = buttons[(idx + 1) % buttons.length];
			next?.focus();
		} else if (e.key === 'ArrowUp') {
			e.preventDefault();
			const prev = buttons[(idx - 1 + buttons.length) % buttons.length];
			prev?.focus();
		}
	}
</script>

<Popover.Root bind:open onOpenChange={handleOpenChange}>
	<Popover.Trigger
		class="inline-flex h-9 max-w-[220px] items-center gap-2 rounded-md border border-border bg-bg-elevated px-3 text-[13px] text-text-primary transition-colors hover:bg-bg-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
	>
		<span class="truncate">
			{activeName ?? 'Select a site'}
		</span>
		<ChevronsUpDown class="h-3.5 w-3.5 shrink-0 text-text-muted" />
	</Popover.Trigger>
	<Popover.Portal>
		<Popover.Content
			sideOffset={6}
			align="start"
			class="z-[110] w-72 rounded-xl border border-border bg-bg-surface p-2 shadow-popover focus:outline-none data-[state=open]:animate-fadeIn"
		>
			{#if loaded && sites.length === 0}
				<div class="px-3 py-6 text-center">
					<p class="text-[13px] text-text-secondary">No sites yet.</p>
					<button
						type="button"
						class="mt-3 inline-flex h-8 items-center justify-center gap-1.5 rounded-md bg-accent px-3 text-[12px] font-medium text-accent-fg hover:bg-accent-hover"
						onclick={createNew}
					>
						<Plus class="h-3.5 w-3.5" />
						Create your first site
					</button>
				</div>
			{:else}
				<div class="flex items-center gap-2 rounded-md border border-border bg-bg-elevated px-2.5">
					<Search class="h-3.5 w-3.5 text-text-muted" />
					<input
						type="text"
						placeholder="Search sites."
						bind:value={query}
						class="h-8 w-full bg-transparent text-[12px] text-text-primary placeholder:text-text-muted focus:outline-none"
					/>
				</div>

				<div
					bind:this={listEl}
					class="mt-2 max-h-64 overflow-y-auto"
					role="listbox"
					tabindex="-1"
					onkeydown={handleListKeydown}
				>
					{#if loading && !loaded}
						<p class="px-3 py-4 text-center text-[12px] text-text-muted">Loading.</p>
					{:else if filtered.length === 0}
						<p class="px-3 py-4 text-center text-[12px] text-text-muted">No matches.</p>
					{:else}
						{#each filtered as site (site.id)}
							<button
								type="button"
								data-site-item
								class="flex w-full items-center justify-between gap-2 rounded-md px-2.5 py-2 text-left text-[13px] text-text-primary transition-colors hover:bg-bg-hover focus:bg-bg-hover focus:outline-none"
								onclick={() => selectSite(site)}
							>
								<span class="flex min-w-0 flex-col">
									<span class="truncate font-medium">{site.name}</span>
									<span class="truncate text-[11px] text-text-muted">{site.slug}</span>
								</span>
								{#if site.id === activeId}
									<Check class="h-3.5 w-3.5 shrink-0 text-accent" />
								{/if}
							</button>
						{/each}
					{/if}
				</div>

				<div class="my-2 h-px bg-border-light"></div>
				<button
					type="button"
					class="flex w-full items-center gap-2 rounded-md px-2.5 py-2 text-[13px] text-text-secondary transition-colors hover:bg-bg-hover hover:text-text-primary focus:bg-bg-hover focus:outline-none"
					onclick={createNew}
				>
					<Plus class="h-3.5 w-3.5" />
					Create new site
				</button>
			{/if}
		</Popover.Content>
	</Popover.Portal>
</Popover.Root>
