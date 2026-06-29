<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import * as api from '$lib/api';
	import type { Evaluation, Site } from '$lib/api/types';
	import { auth } from '$lib/stores/auth.svelte';
	import Button from '$lib/components/ui/Button.svelte';
	import EmptyState from '$lib/components/ui/EmptyState.svelte';
	import GradeBadge from '$lib/components/ui/GradeBadge.svelte';
	import Skeleton from '$lib/components/ui/Skeleton.svelte';

	type Grade = 'A+' | 'A' | 'B+' | 'B' | 'C' | 'D' | 'F';

	interface BuildEntry {
		build_id: string;
		site_id: string;
		site_name: string;
		status: string;
		created_at: string;
		grades: Evaluation[];
	}

	let loading = $state(true);
	let sites = $state<Site[]>([]);
	let recent = $state<BuildEntry[]>([]);
	let latestGrade = $state<Grade | null>(null);
	let buildsLast7Days = $state<number>(0);

	const userName = $derived(auth.value?.name ?? '');

	const VALID_GRADES: Grade[] = ['A+', 'A', 'B+', 'B', 'C', 'D', 'F'];

	function isGrade(g: string): g is Grade {
		return (VALID_GRADES as string[]).includes(g);
	}

	function gradeRank(g: string): number {
		const idx = VALID_GRADES.indexOf(g as Grade);
		return idx === -1 ? VALID_GRADES.length : idx;
	}

	function formatTime(iso: string): string {
		if (!iso) return '';
		const d = new Date(iso);
		if (Number.isNaN(d.getTime())) return '';
		const now = new Date();
		const diffMs = now.getTime() - d.getTime();
		const diffMin = Math.floor(diffMs / 60000);
		if (diffMin < 1) return 'just now';
		if (diffMin < 60) return `${diffMin}m ago`;
		const diffH = Math.floor(diffMin / 60);
		if (diffH < 24) return `${diffH}h ago`;
		const diffD = Math.floor(diffH / 24);
		if (diffD < 7) return `${diffD}d ago`;
		return d.toLocaleDateString();
	}

	function statusDot(status: string): string {
		const s = status.toLowerCase();
		if (s === 'success' || s === 'succeeded' || s === 'complete' || s === 'completed')
			return 'status-dot-success';
		if (s === 'failed' || s === 'error' || s === 'errored') return 'status-dot-danger';
		return 'status-dot-warning';
	}

	function statusLabel(status: string): string {
		if (!status) return 'unknown';
		return status.charAt(0).toUpperCase() + status.slice(1);
	}

	async function load(): Promise<void> {
		loading = true;
		try {
			const sitesResp = await api.sites.list();
			sites = sitesResp.sites ?? [];

			if (sites.length === 0) {
				recent = [];
				latestGrade = null;
				buildsLast7Days = 0;
				return;
			}

			// Fan out evaluation fetches across up to 5 sites for v1.
			const fanout = sites.slice(0, 5);
			const results = await Promise.all(
				fanout.map(async (s) => {
					try {
						const evals = await api.evaluations.listBySite(s.id, 50);
						return { site: s, evals };
					} catch {
						return { site: s, evals: [] as Evaluation[] };
					}
				})
			);

			// Group evaluations by build_id within each site.
			const merged: BuildEntry[] = [];
			for (const { site, evals } of results) {
				const byBuild = new Map<string, Evaluation[]>();
				for (const e of evals) {
					const list = byBuild.get(e.build_id) ?? [];
					list.push(e);
					byBuild.set(e.build_id, list);
				}
				for (const [build_id, group] of byBuild) {
					const created = group.reduce(
						(acc, ev) => (ev.created_at > acc ? ev.created_at : acc),
						group[0]?.created_at ?? ''
					);
					merged.push({
						build_id,
						site_id: site.id,
						site_name: site.name,
						status: site.last_build_status || 'success',
						created_at: created,
						grades: group
					});
				}
			}

			merged.sort((a, b) => (a.created_at < b.created_at ? 1 : -1));
			recent = merged.slice(0, 10);

			// Builds in the last 7 days.
			const cutoff = Date.now() - 7 * 24 * 60 * 60 * 1000;
			buildsLast7Days = merged.filter((b) => {
				const t = new Date(b.created_at).getTime();
				return !Number.isNaN(t) && t >= cutoff;
			}).length;

			// Latest evaluation grade across sites: take worst grade of latest build.
			const latest = merged[0];
			if (latest) {
				let worst: Grade | null = null;
				for (const ev of latest.grades) {
					if (isGrade(ev.grade)) {
						if (!worst || gradeRank(ev.grade) > gradeRank(worst)) worst = ev.grade;
					}
				}
				latestGrade = worst;
			} else {
				latestGrade = null;
			}
		} finally {
			loading = false;
		}
	}

	onMount(() => {
		void load();
	});
</script>

<div class="px-8 py-12">
	<div class="mx-auto max-w-6xl space-y-12">
		<header class="space-y-2">
			<p class="font-mono text-[11px] uppercase tracking-[0.2em] text-text-muted">dashboard</p>
			<h1 class="page-title">
				Welcome back{userName ? `, ${userName.split(' ')[0]}` : ''}.
			</h1>
		</header>

		{#if loading}
			<section class="grid grid-cols-1 gap-5 md:grid-cols-3">
				{#each Array(3) as _, i (i)}
					<div class="card p-6 space-y-3">
						<Skeleton width="40%" height="0.875rem" />
						<Skeleton width="60%" height="2rem" />
						<Skeleton width="50%" height="0.75rem" />
					</div>
				{/each}
			</section>
		{:else if sites.length === 0}
			<EmptyState
				title="No sites yet"
				description="Create your first site to start building, evaluating, and shipping."
			>
				{#snippet action()}
					<Button variant="primary" onclick={() => goto('/sites/new')}>Create your first site</Button>
				{/snippet}
			</EmptyState>
		{:else}
			<section class="grid grid-cols-1 gap-5 md:grid-cols-3">
				<div class="card p-6 animate-stagger-1">
					<p class="text-sm font-medium text-text-secondary">Total sites</p>
					<p class="mt-3 font-mono text-3xl font-normal tracking-tight tabular-nums text-text-primary">
						{sites.length}
					</p>
					<p class="mt-1 text-xs text-text-muted">
						{sites.length === 1 ? '1 active workspace' : `${sites.length} active workspaces`}
					</p>
				</div>

				<div class="card p-6 animate-stagger-2">
					<p class="text-sm font-medium text-text-secondary">Builds, last 7 days</p>
					<p class="mt-3 font-mono text-3xl font-normal tracking-tight tabular-nums text-text-primary">
						{recent.length === 0 ? '0' : buildsLast7Days}
					</p>
					<p class="mt-1 text-xs text-text-muted">
						Across the {Math.min(sites.length, 5)} most recent sites.
					</p>
				</div>

				<div class="card p-6 animate-stagger-3">
					<p class="text-sm font-medium text-text-secondary">Latest grade</p>
					<div class="mt-3 flex items-center gap-3">
						{#if latestGrade}
							<GradeBadge grade={latestGrade} size="sm" />
							<span class="text-xs text-text-muted">worst-of-build, latest run</span>
						{:else}
							<p class="font-display text-3xl font-extralight tracking-tight text-text-muted">None yet</p>
						{/if}
					</div>
					{#if !latestGrade}
						<p class="mt-1 text-xs text-text-muted">Run a build to see grades.</p>
					{/if}
				</div>
			</section>

			<section class="space-y-4 animate-stagger-4">
				<div class="flex items-end justify-between">
					<div>
						<h2 class="font-display text-2xl font-extralight tracking-tight text-text-primary">
							Recent builds
						</h2>
						<p class="mt-1 text-[13px] text-text-secondary">
							The last 10 evaluations across your sites.
						</p>
					</div>
					<Button variant="ghost" size="sm" onclick={() => goto('/sites')}>All sites</Button>
				</div>

				{#if recent.length === 0}
					<div class="card p-10">
						<EmptyState
							title="No builds yet"
							description="Trigger a build from any site to populate this list."
						/>
					</div>
				{:else}
					<div class="card divide-y divide-border-light overflow-hidden">
						{#each recent as build, idx (build.build_id)}
							<a
								href={`/sites/${build.site_id}/evaluations/${build.build_id}`}
								class="group flex items-center gap-5 px-6 py-4 transition-colors hover:bg-bg-hover"
							>
								<span class={statusDot(build.status)} aria-hidden="true"></span>
								<div class="min-w-0 flex-1">
									<p class="truncate text-[13px] font-medium text-text-primary">
										{build.site_name}
									</p>
									<p class="mt-0.5 text-[11px] text-text-muted">
										{statusLabel(build.status)} · {formatTime(build.created_at)}
									</p>
								</div>
								<div class="hidden items-center gap-1.5 sm:flex">
									{#each build.grades.slice(0, 6) as ev (ev.id)}
										{#if isGrade(ev.grade)}
											<GradeBadge grade={ev.grade} size="sm" />
										{/if}
									{/each}
								</div>
								<svg
									class="h-4 w-4 shrink-0 text-text-muted transition-transform group-hover:translate-x-0.5"
									fill="none"
									viewBox="0 0 24 24"
									stroke-width="1.5"
									stroke="currentColor"
								>
									<path stroke-linecap="round" stroke-linejoin="round" d="M8.25 4.5l7.5 7.5-7.5 7.5" />
								</svg>
							</a>
						{/each}
					</div>
				{/if}
			</section>
		{/if}
	</div>
</div>
