<script lang="ts">
	import { goto } from '$app/navigation';
	import * as pagesApi from '$lib/api/pages';
	import * as componentsApi from '$lib/api/components';
	import * as mediaApi from '$lib/api/media';
	import * as agentKeysApi from '$lib/api/agentKeys';
	import * as evaluationsApi from '$lib/api/evaluations';
	import Card from '$lib/components/ui/Card.svelte';
	import Button from '$lib/components/ui/Button.svelte';
	import Tooltip from '$lib/components/ui/Tooltip.svelte';
	import GradeBadge from '$lib/components/ui/GradeBadge.svelte';
	import Badge from '$lib/components/ui/Badge.svelte';
	import Skeleton from '$lib/components/ui/Skeleton.svelte';
	import EmptyState from '$lib/components/ui/EmptyState.svelte';
	import ErrorState from '$lib/components/ui/ErrorState.svelte';
	import { asGrade } from '$lib/evaluations/grade';
	import type { Site, Evaluation } from '$lib/api/types';

	let { data }: { data: { site: Site } } = $props();

	type Stats = {
		pages: number;
		components: number;
		media: number;
		agentKeys: number;
	};

	let stats = $state<Stats | null>(null);
	let evaluations = $state<Evaluation[] | null>(null);
	let loading = $state(true);
	let loadError = $state<string | null>(null);

	const siteID = $derived(data.site.id);

	// 13-grade helpers from the shared module: a local 7-grade fork here
	// rendered 'None' whenever the latest eval carried a minus grade.

	async function safeCount<T>(p: Promise<T>, count: (v: T) => number): Promise<number> {
		try {
			return count(await p);
		} catch {
			return 0;
		}
	}

	async function loadAll(id: string) {
		loading = true;
		loadError = null;
		try {
			const [pagesCount, componentsCount, mediaCount, agentKeysCount, evals] = await Promise.all([
				safeCount(pagesApi.list(id), (r) => r.pages.length),
				safeCount(componentsApi.list(id), (r) => r.length),
				safeCount(mediaApi.list(id, { limit: 1 }), (r) => r.total ?? r.items.length),
				safeCount(agentKeysApi.list(id), (r) => r.length),
				evaluationsApi.listBySite(id, 5).catch(() => [] as Evaluation[])
			]);
			stats = {
				pages: pagesCount,
				components: componentsCount,
				media: mediaCount,
				agentKeys: agentKeysCount
			};
			evaluations = evals;
		} catch (err) {
			loadError = err instanceof Error ? err.message : 'Failed to load site overview';
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		void loadAll(siteID);
	});

	const latestBuildStatus = $derived(data.site.last_build_status || 'never');
	const latestEval = $derived(evaluations && evaluations.length > 0 ? evaluations[0] : null);
	const latestEvalGrade = $derived(latestEval ? asGrade(latestEval.grade) : null);

	function buildStatusVariant(s: string): 'success' | 'warning' | 'danger' | 'default' {
		if (s === 'success') return 'success';
		if (s === 'building' || s === 'pending') return 'warning';
		if (s === 'failed' || s === 'error') return 'danger';
		return 'default';
	}

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
	<section class="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
		<div class="animate-stagger-1">
			<Card padding="sm">
				<p class="text-xs text-text-muted">Pages</p>
				{#if loading || !stats}
					<Skeleton width="3rem" height="1.5rem" class="mt-2" />
				{:else}
					<p class="mt-1 font-display text-2xl font-extralight tracking-tight">{stats.pages}</p>
				{/if}
				<p class="mt-1 text-[11px] text-text-muted">in this site</p>
			</Card>
		</div>

		<div class="animate-stagger-2">
			<Card padding="sm">
				<p class="text-xs text-text-muted">Components</p>
				{#if loading || !stats}
					<Skeleton width="3rem" height="1.5rem" class="mt-2" />
				{:else}
					<p class="mt-1 font-display text-2xl font-extralight tracking-tight">{stats.components}</p>
				{/if}
				<p class="mt-1 text-[11px] text-text-muted">reusable</p>
			</Card>
		</div>

		<div class="animate-stagger-3">
			<Card padding="sm">
				<p class="text-xs text-text-muted">Media</p>
				{#if loading || !stats}
					<Skeleton width="3rem" height="1.5rem" class="mt-2" />
				{:else}
					<p class="mt-1 font-display text-2xl font-extralight tracking-tight">{stats.media}</p>
				{/if}
				<p class="mt-1 text-[11px] text-text-muted">files uploaded</p>
			</Card>
		</div>

		<div class="animate-stagger-4">
			<Card padding="sm">
				<p class="text-xs text-text-muted">Agent keys</p>
				{#if loading || !stats}
					<Skeleton width="3rem" height="1.5rem" class="mt-2" />
				{:else}
					<p class="mt-1 font-display text-2xl font-extralight tracking-tight">{stats.agentKeys}</p>
				{/if}
				<p class="mt-1 text-[11px] text-text-muted">active</p>
			</Card>
		</div>

		<div class="animate-stagger-5">
			<Card padding="sm">
				<p class="text-xs text-text-muted">Last build</p>
				<div class="mt-2">
					<Badge variant={buildStatusVariant(latestBuildStatus)} dot>
						{latestBuildStatus}
					</Badge>
				</div>
				<p class="mt-2 text-[11px] text-text-muted">
					{data.site.last_build_at ? formatDate(data.site.last_build_at) : 'never built'}
				</p>
			</Card>
		</div>

		<div class="animate-stagger-6">
			<Card padding="sm">
				<p class="text-xs text-text-muted">Latest eval</p>
				{#if loading}
					<Skeleton width="3rem" height="1.5rem" class="mt-2" />
				{:else if latestEvalGrade}
					<div class="mt-1">
						<GradeBadge grade={latestEvalGrade} size="sm" />
					</div>
				{:else}
					<p class="mt-1 font-display text-2xl font-extralight tracking-tight text-text-muted">None</p>
				{/if}
				<p class="mt-1 text-[11px] text-text-muted">
					{latestEval ? latestEval.category : 'no evals yet'}
				</p>
			</Card>
		</div>
	</section>

	<section class="mt-8">
		<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">Quick actions</h2>
		<div class="mt-3 flex flex-wrap items-center gap-2">
			<Button variant="primary" onclick={() => goto(`/sites/${siteID}/pages`)}>
				New page
			</Button>
			<Button variant="secondary" onclick={() => goto(`/sites/${siteID}/build`)}>
				Trigger build
			</Button>
			<Tooltip content="Coming soon">
				{#snippet trigger({ props })}
					<button
						{...props}
						type="button"
						class="inline-flex h-9 items-center justify-center gap-2 rounded-lg px-3 text-[13px] text-text-muted hover:text-text-primary hover:bg-bg-hover transition-colors"
					>
						Open in editor
					</button>
				{/snippet}
			</Tooltip>
		</div>
	</section>

	<section class="mt-8">
		<div class="flex items-baseline justify-between">
			<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">
				Recent evaluations
			</h2>
			{#if evaluations && evaluations.length > 0}
				<a
					href={`/sites/${siteID}/evaluations`}
					class="text-[12px] text-text-muted hover:text-text-primary transition-colors"
				>
					View all
				</a>
			{/if}
		</div>

		<div class="mt-3">
			{#if loading}
				<div class="space-y-2">
					{#each Array(3) as _, i (i)}
						<Card padding="sm">
							<div class="flex items-center gap-3">
								<Skeleton width="2.25rem" height="2.25rem" rounded="full" />
								<div class="flex-1 space-y-1.5">
									<Skeleton width="40%" height="0.9rem" />
									<Skeleton width="25%" height="0.7rem" />
								</div>
							</div>
						</Card>
					{/each}
				</div>
			{:else if loadError}
				<ErrorState message={loadError} onRetry={() => loadAll(siteID)} />
			{:else if !evaluations || evaluations.length === 0}
				<Card padding="none">
					<EmptyState
						title="No evaluations yet"
						description="Run a build to score this site across security, SEO, accessibility, performance, and privacy."
					>
						{#snippet action()}
							<Button variant="secondary" onclick={() => goto(`/sites/${siteID}/build`)}>
								Trigger first build
							</Button>
						{/snippet}
					</EmptyState>
				</Card>
			{:else}
				<div class="space-y-2">
					{#each evaluations as evaluation (evaluation.id)}
						{@const grade = asGrade(evaluation.grade)}
						<Card padding="sm">
							<div class="flex items-center gap-4">
								{#if grade}
									<GradeBadge {grade} size="sm" />
								{:else}
									<span
										class="inline-flex h-9 w-9 items-center justify-center rounded-2xl bg-bg-elevated text-text-muted text-base"
									>
										{evaluation.grade || '?'}
									</span>
								{/if}
								<div class="flex-1 min-w-0">
									<p class="text-[13px] font-medium text-text-primary capitalize">
										{evaluation.category}
									</p>
									<p class="text-[11px] text-text-muted">
										{evaluation.score} / {evaluation.max_score} · {formatDate(evaluation.created_at)}
									</p>
								</div>
							</div>
						</Card>
					{/each}
				</div>
			{/if}
		</div>
	</section>
</div>
