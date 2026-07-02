<script lang="ts">
	import { page } from '$app/stores';
	import { goto } from '$app/navigation';
	import * as evaluationsApi from '$lib/api/evaluations';
	import Card from '$lib/components/ui/Card.svelte';
	import Button from '$lib/components/ui/Button.svelte';
	import Skeleton from '$lib/components/ui/Skeleton.svelte';
	import CategoryDonut from '$lib/components/evaluations/CategoryDonut.svelte';
	import CategoryGradeCard from '$lib/components/evaluations/CategoryGradeCard.svelte';
	import type { Site, Evaluation } from '$lib/api/types';

	let { data }: { data: { site: Site } } = $props();
	const siteID = $derived(data.site.id);
	const buildID = $derived($page.params.buildID ?? '');

	const CATEGORIES = ['security', 'seo', 'performance', 'accessibility', 'privacy'] as const;

	let loading = $state(true);
	let loadError = $state<string | null>(null);
	let buildEvals = $state<Evaluation[]>([]);

	async function load(id: string) {
		loading = true;
		loadError = null;
		try {
			buildEvals = await evaluationsApi.listByBuild(siteID, id);
		} catch (err) {
			loadError = err instanceof Error ? err.message : 'Failed to load evaluation';
			buildEvals = [];
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		void load(buildID);
	});

	const buildCreatedAt = $derived(buildEvals[0]?.created_at ?? '');

	const evalByCategory = $derived.by<Record<string, Evaluation | null>>(() => {
		const map: Record<string, Evaluation | null> = {};
		for (const cat of CATEGORIES) {
			map[cat] = buildEvals.find((e) => e.category === cat) ?? null;
		}
		return map;
	});

	function formatDate(ts: string): string {
		if (!ts) return '';
		const d = new Date(ts);
		if (Number.isNaN(d.getTime())) return '';
		return d.toLocaleString(undefined, {
			year: 'numeric',
			month: 'short',
			day: 'numeric',
			hour: '2-digit',
			minute: '2-digit'
		});
	}
</script>

<div class="mx-auto max-w-7xl px-6 py-8">
	<div class="mb-6 flex flex-wrap items-start justify-between gap-4">
		<div>
			<a
				href={`/sites/${siteID}/build`}
				class="text-[12px] text-text-muted hover:text-text-primary transition-colors"
			>
				&larr; All builds
			</a>
			<h1 class="mt-2 font-display text-3xl font-extralight tracking-tight text-text-primary">
				Build evaluation
			</h1>
			<p class="mt-1 text-[12px] text-text-muted font-mono">{buildID}</p>
			{#if buildCreatedAt}
				<p class="mt-0.5 text-[12px] text-text-secondary">{formatDate(buildCreatedAt)}</p>
			{/if}
			{#if buildEvals[0]?.profile}
				<p class="mt-1.5">
					<span
						class="rounded-full border border-border-light bg-bg-elevated/60 px-2 py-0.5 font-mono text-[10px] uppercase tracking-[0.14em] text-text-secondary"
						title="Design fidelity this build was graded under"
					>
						graded as {buildEvals[0].profile}
					</span>
				</p>
			{/if}
		</div>
		<Button variant="primary" onclick={() => goto(`/sites/${siteID}/build`)}>
			Trigger new build
		</Button>
	</div>

	{#if loading}
		<Card padding="md" class="mb-6">
			<div class="grid grid-cols-2 gap-6 sm:grid-cols-3 lg:grid-cols-5">
				{#each CATEGORIES as cat (cat)}
					<div class="flex flex-col items-center gap-2">
						<Skeleton width="8rem" height="8rem" rounded="full" />
						<Skeleton width="3rem" height="0.7rem" />
					</div>
				{/each}
			</div>
		</Card>
		<div class="space-y-4">
			{#each Array(5) as _, i (i)}
				<Skeleton width="100%" height="120px" />
			{/each}
		</div>
	{:else if loadError}
		<Card padding="md">
			<p class="text-[13px] text-danger">{loadError}</p>
		</Card>
	{:else}
		<!-- Donut row: matches the eval index "Latest scores" panel. -->
		<section class="mb-8">
			<Card padding="md">
				<div class="flex items-baseline justify-between">
					<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">
						Category scores
					</h2>
					<span class="text-[11px] text-text-muted">5 categories</span>
				</div>
				<div class="mt-5 grid grid-cols-2 gap-6 sm:grid-cols-3 lg:grid-cols-5">
					{#each CATEGORIES as cat (cat)}
						<CategoryDonut category={cat} evaluation={evalByCategory[cat] ?? null} />
					{/each}
				</div>
			</Card>
		</section>

		<!-- Per-category bars: full-width, embedded donut, 2-col check grid on expand. -->
		<section class="space-y-4">
			{#each CATEGORIES as cat (cat)}
				<CategoryGradeCard
					category={cat}
					evaluation={evalByCategory[cat] ?? null}
					{siteID}
				/>
			{/each}
		</section>
	{/if}
</div>
