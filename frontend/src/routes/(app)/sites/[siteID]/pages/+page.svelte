<script lang="ts">
	import { goto } from '$app/navigation';
	import { page as pageState } from '$app/state';
	import * as pagesApi from '$lib/api/pages';
	import Button from '$lib/components/ui/Button.svelte';
	import Card from '$lib/components/ui/Card.svelte';
	import Skeleton from '$lib/components/ui/Skeleton.svelte';
	import EmptyState from '$lib/components/ui/EmptyState.svelte';
	import ErrorState from '$lib/components/ui/ErrorState.svelte';
	import Dialog from '$lib/components/ui/Dialog.svelte';
	import Input from '$lib/components/ui/Input.svelte';
	import Select from '$lib/components/ui/Select.svelte';
	import { confirm } from '$lib/components/ui/ConfirmDialog.svelte';
	import { toast } from '$lib/stores/toast.svelte';
	import PageRow from '$lib/components/pages/PageRow.svelte';
	import RevisionDrawer from '$lib/components/revisions/RevisionDrawer.svelte';
	import { transliterateSlugChars } from '$lib/stores/wizard.svelte';
	import type { Page } from '$lib/api/types';

	const siteID = $derived(pageState.params.siteID as string);

	let pages = $state<Page[]>([]);
	let loading = $state(true);
	let loadError = $state<string | null>(null);
	let draggingId = $state<string | null>(null);

	let newDialogOpen = $state(false);
	let creating = $state(false);
	let newTitle = $state('');
	let newSlug = $state('');
	let newLayout = $state('default');
	let newSlugTouched = $state(false);
	// Parent silo for new page. '' means top-level. Otherwise a silo prefix
	// like 'tjanster' meaning the new slug becomes 'tjanster/<your-slug>'.
	let newParentSilo = $state('');

	// Revision history drawer state
	let historyOpen = $state(false);
	let historyPage = $state<Page | null>(null);

	function openHistory(p: Page) {
		historyPage = p;
		historyOpen = true;
	}

	async function onRevisionRestored() {
		// Reload pages so the row reflects the restored state (slug, title etc).
		await loadPages(siteID);
	}

	// Move dialog state
	let moveDialogOpen = $state(false);
	let moving = $state(false);
	let movePage = $state<Page | null>(null);
	let moveParent = $state('');
	let moveLocalSlug = $state('');

	const layoutOptions = [
		{ value: 'default', label: 'Default' },
		{ value: 'landing', label: 'Landing' },
		{ value: 'legal', label: 'Legal' },
		{ value: 'minimal', label: 'Minimal' }
	];

	function slugify(input: string): string {
		return transliterateSlugChars(input)
			.replace(/[^a-z0-9]+/g, '-')
			.replace(/^-+|-+$/g, '')
			.slice(0, 60);
	}

	$effect(() => {
		if (!newSlugTouched) {
			newSlug = slugify(newTitle);
		}
	});

	function normalizeSlug(s: string): string {
		return s.replace(/^\/+/, '').replace(/\/+$/, '');
	}

	function siloOf(slug: string): string {
		const norm = normalizeSlug(slug);
		if (!norm) return '';
		const idx = norm.indexOf('/');
		return idx === -1 ? '' : norm.slice(0, idx);
	}

	function isUnderSilo(page: Page): boolean {
		const norm = normalizeSlug(page.slug);
		return norm.includes('/');
	}

	// Compute which top-level pages are silo hubs (have at least one child page
	// whose slug starts with `<this slug>/`).
	const siloHubs = $derived.by<Set<string>>(() => {
		const set = new Set<string>();
		for (const p of pages) {
			const silo = siloOf(p.slug);
			if (silo) set.add(silo);
		}
		return set;
	});

	// Group pages: top-level rendered first in their original order (preserving
	// drag-reorder), with each silo hub immediately followed by its children
	// (also in their saved order).
	type Grouped = { page: Page; depth: number; isHub: boolean; siloType: string };

	let siloTypesByPrefix = $state<Record<string, string>>({});

	async function loadSiloTypes(id: string) {
		try {
			const res = await fetch(`/api/sites/${id}/silos`, { credentials: 'include' });
			if (!res.ok) return;
			const data = (await res.json()) as { silos?: { slug_prefix: string; silo_type: string }[] };
			const map: Record<string, string> = {};
			for (const s of data.silos ?? []) {
				map[s.slug_prefix.replace(/^\/+|\/+$/g, '')] = s.silo_type;
			}
			siloTypesByPrefix = map;
		} catch {
			// Endpoint may not exist yet on older servers; silently degrade.
		}
	}

	$effect(() => {
		void loadSiloTypes(siteID);
	});

	const groupedPages = $derived.by<Grouped[]>(() => {
		const out: Grouped[] = [];
		const childrenBySilo = new Map<string, Page[]>();
		const topLevel: Page[] = [];
		for (const p of pages) {
			if (isUnderSilo(p)) {
				const silo = siloOf(p.slug);
				const existing = childrenBySilo.get(silo) ?? [];
				existing.push(p);
				childrenBySilo.set(silo, existing);
			} else {
				topLevel.push(p);
			}
		}
		for (const p of topLevel) {
			const norm = normalizeSlug(p.slug);
			const isHub = siloHubs.has(norm) && norm.length > 0;
			const siloType = isHub ? (siloTypesByPrefix[norm] ?? '') : '';
			out.push({ page: p, depth: 0, isHub, siloType });
			if (isHub) {
				for (const child of childrenBySilo.get(norm) ?? []) {
					out.push({ page: child, depth: 1, isHub: false, siloType: '' });
				}
				childrenBySilo.delete(norm);
			}
		}
		for (const [, children] of childrenBySilo) {
			for (const child of children) {
				out.push({ page: child, depth: 1, isHub: false, siloType: '' });
			}
		}
		return out;
	});

	const siloOptions = $derived.by<{ value: string; label: string }[]>(() => {
		const options = [{ value: '', label: '(top-level page)' }];
		for (const hub of siloHubs) {
			options.push({ value: hub, label: `Under /${hub}` });
		}
		return options;
	});

	const parentSiloPath = $derived(newParentSilo ? `${newParentSilo}/` : '');

	async function loadPages(id: string) {
		loading = true;
		loadError = null;
		try {
			const res = await pagesApi.list(id);
			pages = [...(res.pages ?? [])].sort((a, b) => a.sort_order - b.sort_order);
		} catch (err) {
			loadError = err instanceof Error ? err.message : 'Failed to load pages';
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		void loadPages(siteID);
	});

	function openNewDialog(parentSilo = '') {
		newTitle = '';
		newSlug = '';
		newLayout = 'default';
		newSlugTouched = false;
		newParentSilo = parentSilo;
		newDialogOpen = true;
	}

	async function createPage() {
		const title = newTitle.trim();
		const localSlug = newSlug.trim();
		if (title.length < 2) {
			toast.error('Title must be at least 2 characters.');
			return;
		}
		if (!/^[a-z0-9-]+$/.test(localSlug)) {
			toast.error('Slug must use lowercase letters, numbers, and hyphens.');
			return;
		}
		const fullSlug = newParentSilo ? `${newParentSilo}/${localSlug}` : localSlug;
		creating = true;
		try {
			const created = await pagesApi.create(siteID, {
				title,
				slug: fullSlug,
				layout: newLayout
			});
			newDialogOpen = false;
			toast.success('Page created.');
			await goto(`/sites/${siteID}/pages/${created.id}`);
		} catch (err) {
			const msg = err instanceof Error ? err.message : 'Failed to create page';
			toast.error(msg);
		} finally {
			creating = false;
		}
	}

	function openPage(p: Page) {
		void goto(`/sites/${siteID}/pages/${p.id}`);
	}

	function openMoveDialog(p: Page) {
		movePage = p;
		const norm = normalizeSlug(p.slug);
		const idx = norm.indexOf('/');
		moveParent = idx === -1 ? '' : norm.slice(0, idx);
		moveLocalSlug = idx === -1 ? norm : norm.slice(idx + 1);
		moveDialogOpen = true;
	}

	const moveOptions = $derived.by<{ value: string; label: string }[]>(() => {
		const opts = [{ value: '', label: '(top-level)' }];
		for (const hub of siloHubs) {
			// Don't let a page move into its own subtree
			if (movePage && normalizeSlug(movePage.slug) === hub) continue;
			opts.push({ value: hub, label: `Under /${hub}` });
		}
		return opts;
	});

	const movePreviewSlug = $derived(
		moveParent ? `/${moveParent}/${moveLocalSlug}` : `/${moveLocalSlug}`
	);

	async function applyMove() {
		if (!movePage) return;
		const local = moveLocalSlug.trim();
		if (!/^[a-z0-9-]+$/.test(local)) {
			toast.error('Slug must use lowercase letters, numbers, hyphens.');
			return;
		}
		const newSlugFull = moveParent ? `${moveParent}/${local}` : local;
		if (normalizeSlug(movePage.slug) === newSlugFull) {
			moveDialogOpen = false;
			return;
		}
		moving = true;
		try {
			await pagesApi.update(siteID, movePage.id, { slug: newSlugFull });
			toast.success('Page moved.');
			moveDialogOpen = false;
			await loadPages(siteID);
		} catch (err) {
			const msg = err instanceof Error ? err.message : 'Failed to move page';
			toast.error(msg);
		} finally {
			moving = false;
		}
	}

	async function deletePage(p: Page) {
		const ok = await confirm({
			title: `Delete "${p.title || p.slug}"?`,
			message: 'This removes the page and its blocks. This cannot be undone.',
			confirmText: 'Delete',
			cancelText: 'Cancel',
			variant: 'danger'
		});
		if (!ok) return;
		const previous = pages;
		pages = pages.filter((x) => x.id !== p.id);
		try {
			await pagesApi.remove(siteID, p.id);
			toast.success('Page deleted.');
		} catch (err) {
			pages = previous;
			const msg = err instanceof Error ? err.message : 'Failed to delete page';
			toast.error(msg);
		}
	}

	function indexOf(id: string): number {
		return pages.findIndex((p) => p.id === id);
	}

	function handleDragStart(id: string, e: DragEvent) {
		draggingId = id;
		if (e.dataTransfer) {
			e.dataTransfer.effectAllowed = 'move';
			e.dataTransfer.setData('text/plain', id);
		}
	}

	function handleDragOver(id: string, e: DragEvent) {
		e.preventDefault();
		if (e.dataTransfer) e.dataTransfer.dropEffect = 'move';
		if (!draggingId || draggingId === id) return;
		const fromIdx = indexOf(draggingId);
		const toIdx = indexOf(id);
		if (fromIdx === -1 || toIdx === -1 || fromIdx === toIdx) return;
		const next = [...pages];
		const moved = next.splice(fromIdx, 1)[0];
		if (!moved) return;
		next.splice(toIdx, 0, moved);
		pages = next.map((p, i) => ({ ...p, sort_order: i }));
	}

	async function handleDragEnd() {
		const movedId = draggingId;
		draggingId = null;
		if (!movedId) return;
		try {
			await pagesApi.reorder(
				siteID,
				pages.map((p) => p.id)
			);
		} catch (err) {
			const msg = err instanceof Error ? err.message : 'Failed to save order';
			toast.error(msg);
			void loadPages(siteID);
		}
	}

	function handleDrop(e: DragEvent) {
		e.preventDefault();
	}
</script>

<div class="mx-auto max-w-7xl px-6 py-8">
	<header class="flex items-center justify-between">
		<div>
			<h1 class="font-display text-3xl font-extralight tracking-tight text-text-primary">
				Pages
			</h1>
			<p class="mt-1 text-[13px] text-text-secondary">
				Drag rows to reorder. Click a page to open the editor.
			</p>
		</div>
		<Button variant="primary" onclick={() => openNewDialog()}>New page</Button>
	</header>

	<section class="mt-6">
		{#if loading}
			<div class="flex flex-col gap-2">
				{#each Array(4) as _, i (i)}
					<Card padding="sm">
						<div class="flex items-center gap-3">
							<Skeleton width="40%" height="0.95rem" />
							<Skeleton width="20%" height="0.7rem" />
						</div>
					</Card>
				{/each}
			</div>
		{:else if loadError}
			<ErrorState message={loadError} />
		{:else if pages.length === 0}
			<Card padding="none">
				<EmptyState
					title="No pages yet"
					description="Pages are the top-level URLs of your site. Add a homepage, an about page, or a legal page."
				>
					{#snippet action()}
						<Button variant="primary" onclick={() => openNewDialog()}>Create page</Button>
					{/snippet}
				</EmptyState>
			</Card>
		{:else}
			<div class="flex flex-col gap-2" role="list">
				{#each groupedPages as g (g.page.id)}
					<PageRow
						page={g.page}
						depth={g.depth}
						isSiloHub={g.isHub}
						siloType={g.siloType}
						dragging={draggingId === g.page.id}
						onOpen={() => openPage(g.page)}
						onDelete={() => deletePage(g.page)}
						onMove={() => openMoveDialog(g.page)}
						onHistory={() => openHistory(g.page)}
						ondragstart={(e) => handleDragStart(g.page.id, e)}
						ondragover={(e) => handleDragOver(g.page.id, e)}
						ondragend={handleDragEnd}
						ondrop={handleDrop}
					/>
					{#if g.isHub}
						{@const childCount = groupedPages.filter(
							(x) => x.depth === 1 && normalizeSlug(x.page.slug).startsWith(normalizeSlug(g.page.slug) + '/')
						).length}
						<div
							class="-mt-1 ml-6 flex items-center justify-between border-l-2 border-l-accent/30 pl-4 pb-1 text-[11px] text-text-muted"
						>
							<span>
								{childCount === 0
									? 'No pages under this silo yet.'
									: `${childCount} page${childCount === 1 ? '' : 's'} under /${normalizeSlug(g.page.slug)}`}
							</span>
							<button
								type="button"
								class="text-accent hover:underline"
								onclick={() => openNewDialog(normalizeSlug(g.page.slug))}
							>
								+ Add page under this silo
							</button>
						</div>
					{/if}
				{/each}
			</div>
		{/if}
	</section>
</div>

<Dialog bind:open={newDialogOpen} title="New page" size="sm">
	<div class="flex flex-col gap-3">
		<Input
			label="Title"
			value={newTitle}
			oninput={(e) => {
				newTitle = (e.currentTarget as HTMLInputElement).value;
			}}
			placeholder="About us"
		/>
		{#if siloOptions.length > 1}
			<div class="flex flex-col gap-1.5">
				<span class="text-[12px] font-medium text-text-secondary">Parent</span>
				<Select options={siloOptions} bind:value={newParentSilo} />
				<span class="text-[11px] text-text-muted">
					Pick a silo to nest this page under. Stays inline with the silo hub in the page list.
				</span>
			</div>
		{/if}
		<Input
			label="Slug"
			value={newSlug}
			oninput={(e) => {
				newSlug = (e.currentTarget as HTMLInputElement).value;
				newSlugTouched = true;
			}}
			placeholder="about"
			hint={newParentSilo
				? `Final URL will be /${parentSiloPath}${newSlug || 'your-slug'}`
				: 'Lowercase letters, numbers, hyphens. Used in the URL.'}
		/>
		<div class="flex flex-col gap-1.5">
			<span class="text-[12px] font-medium text-text-secondary">Layout</span>
			<Select options={layoutOptions} bind:value={newLayout} />
		</div>
	</div>
	{#snippet footer()}
		<Button variant="secondary" onclick={() => (newDialogOpen = false)}>Cancel</Button>
		<Button variant="primary" loading={creating} onclick={createPage}>Create</Button>
	{/snippet}
</Dialog>

<Dialog bind:open={moveDialogOpen} title="Move page" size="sm">
	<div class="flex flex-col gap-3">
		{#if movePage}
			<p class="text-[13px] text-text-secondary">
				Currently <span class="font-mono text-[12px] text-text-primary">{movePage.slug}</span>
			</p>
		{/if}
		<div class="flex flex-col gap-1.5">
			<span class="text-[12px] font-medium text-text-secondary">Parent</span>
			<Select options={moveOptions} bind:value={moveParent} />
			<span class="text-[11px] text-text-muted">
				Pick a silo or move to top-level. Children of this page move with it.
			</span>
		</div>
		<Input
			label="Slug"
			value={moveLocalSlug}
			oninput={(e) => {
				moveLocalSlug = (e.currentTarget as HTMLInputElement).value;
			}}
			hint="New URL: {movePreviewSlug}"
		/>
	</div>
	{#snippet footer()}
		<Button variant="secondary" onclick={() => (moveDialogOpen = false)}>Cancel</Button>
		<Button variant="primary" loading={moving} onclick={applyMove}>Move</Button>
	{/snippet}
</Dialog>


{#if historyPage}
	<RevisionDrawer
		bind:open={historyOpen}
		{siteID}
		entityType="page"
		entityID={historyPage.id}
		entityLabel={historyPage.title}
		onRestored={onRevisionRestored}
	/>
{/if}
