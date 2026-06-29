<script lang="ts">
	import { goto } from '$app/navigation';
	import { ArrowLeft, Plus, Database } from 'lucide-svelte';
	import Button from '$lib/components/ui/Button.svelte';
	import Card from '$lib/components/ui/Card.svelte';
	import EmptyState from '$lib/components/ui/EmptyState.svelte';
	import { ApiError } from '$lib/api/client';
	import * as collectionsApi from '$lib/api/collections';
	import type { Collection } from '$lib/api/collections';

	let { data }: { data: { site: { id: string; name: string } } } = $props();
	const siteID = $derived(data.site.id);

	let collections = $state<Collection[]>([]);
	let loading = $state(true);
	let loadError = $state<string | null>(null);

	async function load() {
		loading = true;
		loadError = null;
		try {
			collections = await collectionsApi.listCollections(siteID);
		} catch (err) {
			loadError = err instanceof ApiError ? err.message : 'Failed to load collections.';
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		void load();
	});
</script>

<div class="mx-auto max-w-6xl px-6 py-8">
	<a
		href={`/sites/${siteID}`}
		class="inline-flex items-center gap-1.5 text-[12px] text-text-muted transition-colors hover:text-text-primary"
	>
		<ArrowLeft size={14} strokeWidth={1.75} />
		Site dashboard
	</a>
	<header class="mt-3 flex items-start justify-between gap-4">
		<div>
			<h1 class="font-display text-3xl font-extralight tracking-tight text-text-primary">
				Collections
			</h1>
			<p class="mt-1 text-[13px] text-text-secondary">
				Custom content types: case studies, products, team members, FAQs, events, anything you'd add new entries to over time.
			</p>
		</div>
		<Button variant="primary" onclick={() => goto(`/sites/${siteID}/collections/new`)}>
			<Plus size={14} strokeWidth={1.75} />New collection
		</Button>
	</header>

	<div class="mt-8 flex flex-col gap-3">
		{#if loading}
			<p class="text-[12px] text-text-muted">Loading...</p>
		{:else if loadError}
			<Card padding="md" class="border-danger/30">
				<p class="text-[13px] text-danger">{loadError}</p>
			</Card>
		{:else if collections.length === 0}
			<EmptyState
				title="No collections yet"
				description="Create one to start modelling content beyond pages: case studies, products, team members, anything you'd add new entries to over time."
			>
				{#snippet icon()}
					<Database size={20} strokeWidth={1.5} />
				{/snippet}
			</EmptyState>
		{:else}
			{#each collections as col (col.id)}
				<Card padding="md">
					<div class="flex items-start justify-between gap-4">
						<div class="min-w-0 flex-1">
							<h2 class="text-[15px] font-medium text-text-primary">{col.name}</h2>
							<p class="mt-1 text-[12px] text-text-muted">
								<span class="font-mono">/{col.slug}</span>
								{#if col.settings.schema_org_type}
									<span class="ml-2 rounded-md bg-bg-elevated px-1.5 py-0.5 text-[10px] uppercase tracking-wider">{col.settings.schema_org_type}</span>
								{/if}
								{#if col.settings.render_as_pages}
									<span class="ml-2 text-[10px] text-accent">renders as pages</span>
								{/if}
							</p>
							<p class="mt-2 text-[13px] text-text-primary">
								<strong class="font-medium">{col.item_count}</strong>
								<span class="text-text-secondary">items</span>
								<span class="ml-2 text-[11px] text-text-muted">
									{col.schema.length} {col.schema.length === 1 ? 'field' : 'fields'}
								</span>
							</p>
						</div>
						<Button
							variant="secondary"
							size="sm"
							onclick={() => goto(`/sites/${siteID}/collections/${col.id}`)}
						>
							Open
						</Button>
					</div>
				</Card>
			{/each}
		{/if}
	</div>
</div>
