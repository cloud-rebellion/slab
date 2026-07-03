<script lang="ts">
	import { goto } from '$app/navigation';
	import * as buildsApi from '$lib/api/builds';
	import * as evaluationsApi from '$lib/api/evaluations';
	import * as deployApi from '$lib/api/deploy';
	import {
		pollBuild,
		getState,
		clearBuild,
		type BuildStatusState,
		type BuildStatusValue
	} from '$lib/stores/buildStatus.svelte';
	import { toast } from '$lib/stores/toast.svelte';
	import Card from '$lib/components/ui/Card.svelte';
	import Button from '$lib/components/ui/Button.svelte';
	import Badge from '$lib/components/ui/Badge.svelte';
	import GradeBadge from '$lib/components/ui/GradeBadge.svelte';
	import EmptyState from '$lib/components/ui/EmptyState.svelte';
	import Skeleton from '$lib/components/ui/Skeleton.svelte';
	import Select from '$lib/components/ui/Select.svelte';
	import CategoryDonut from '$lib/components/evaluations/CategoryDonut.svelte';
	import ScoreHistoryChart, {
		type BuildHistoryEntry
	} from '$lib/components/evaluations/ScoreHistoryChart.svelte';
	import { type Grade as SharedGrade, asGrade as asGradeShared, compositeGrade as sharedComposite, gradeTone } from '$lib/evaluations/grade';
	import type { Site, Evaluation, DeployTarget, DeployResult } from '$lib/api/types';
	import { Copy as CopyIcon, ExternalLink, Download } from 'lucide-svelte';

	let { data }: { data: { site: Site } } = $props();

	const siteID = $derived(data.site.id);

	let triggering = $state(false);
	let activeBuildID = $state<string | null>(null);
	let stopFn = $state<(() => void) | null>(null);
	let nowTick = $state(Date.now());
	let nowInterval: ReturnType<typeof setInterval> | null = null;

	let logEl = $state<HTMLPreElement | null>(null);
	let userScrolledUp = $state(false);
	let lastTerminalReported = $state<BuildStatusValue | null>(null);

	let recentLoading = $state(true);
	let recentRows = $state<RecentBuildRow[]>([]);
	let recentError = $state<string | null>(null);
	// All evaluation rows for this site, grouped by build_id. Drives both the
	// "Latest scores" donut row and the score history chart at the top of
	// this page (the merged Evals tab content).
	let allEvals = $state<Evaluation[]>([]);

	import { CATEGORIES } from '$lib/evaluations/categories';

	let deployTargets = $state<DeployTarget[]>([]);
	let deployTargetsLoading = $state(true);
	let deployTargetID = $state<string>('');
	let deploying = $state(false);
	let deployResult = $state<DeployResult | null>(null);
	let deployedAt = $state<string | null>(null);
	let deployTargetUsedID = $state<string | null>(null);

	// Grade machinery lives in $lib/evaluations/grade (13-grade scale
	// mirroring the backend). A local 7-grade fork here used to render
	// minus grades (A-, B-...) as '?' in build history.
	interface RecentBuildRow {
		buildID: string;
		created_at: string;
		composite: SharedGrade | null;
		categoryCount: number;
		evaluations: Evaluation[];
		targetID?: string;
		deployURL?: string;
		deployedAt?: string;
		targetName?: string;
	}

	function groupRecentBuilds(evals: Evaluation[]): RecentBuildRow[] {
		const map = new Map<string, Evaluation[]>();
		for (const e of evals) {
			if (!e.build_id) continue;
			const list = map.get(e.build_id) ?? [];
			list.push(e);
			map.set(e.build_id, list);
		}
		const rows: RecentBuildRow[] = [];
		for (const [buildID, group] of map.entries()) {
			const newest = group.reduce((acc, cur) =>
				new Date(cur.created_at).getTime() > new Date(acc.created_at).getTime() ? cur : acc
			);
			rows.push({
				buildID,
				created_at: newest.created_at,
				composite: sharedComposite(group),
				categoryCount: group.length,
				evaluations: group
			});
		}
		rows.sort((a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime());
		return rows.slice(0, 10);
	}

	async function loadRecent() {
		recentLoading = true;
		recentError = null;
		try {
			const evals = await evaluationsApi.listBySite(siteID);
			allEvals = evals;
			const rows = groupRecentBuilds(evals);
			// Best-effort augmentation: if the build status response carries deploy
			// info (target_id/deploy_url/deployed_at), surface it on the row.
			const enriched = await Promise.all(
				rows.map(async (row) => {
					try {
						const status = await buildsApi.getBuildStatus(siteID, row.buildID);
						return {
							...row,
							targetID: status.target_id,
							deployURL: status.deploy_url,
							deployedAt: status.deployed_at
						};
					} catch {
						return row;
					}
				})
			);
			recentRows = enriched;
		} catch (err) {
			recentError = err instanceof Error ? err.message : 'Failed to load build history';
			recentRows = [];
		} finally {
			recentLoading = false;
		}
	}

	async function loadDeployTargets() {
		deployTargetsLoading = true;
		try {
			deployTargets = await deployApi.listTargets(siteID);
			if (deployTargets.length > 0 && !deployTargetID) {
				const def = deployTargets.find((t) => t.is_default);
				const first = deployTargets[0];
				deployTargetID = def?.id ?? first?.id ?? '';
			}
		} catch (err) {
			toast.error(err instanceof Error ? err.message : 'Failed to load deploy targets.');
		} finally {
			deployTargetsLoading = false;
		}
	}

	async function onDeploy() {
		if (!activeBuildID || !deployTargetID || deploying) return;
		deploying = true;
		try {
			const resp = await deployApi.triggerDeploy(siteID, {
				build_id: activeBuildID,
				target_id: deployTargetID
			});
			deployResult = resp;
			deployedAt = new Date().toISOString();
			deployTargetUsedID = deployTargetID;
			toast.success('Deploy started.');
		} catch (err) {
			toast.error(err instanceof Error ? err.message : 'Failed to deploy.');
		} finally {
			deploying = false;
		}
	}

	async function copyDeployURL() {
		if (!deployResult?.deploy_url) return;
		try {
			await navigator.clipboard.writeText(deployResult.deploy_url);
			toast.success('URL copied.');
		} catch {
			toast.error('Failed to copy URL.');
		}
	}

	function formatRelative(ts: string): string {
		const d = new Date(ts);
		if (Number.isNaN(d.getTime())) return '';
		const diffMs = nowTick - d.getTime();
		const sec = Math.max(0, Math.floor(diffMs / 1000));
		if (sec < 60) return `${sec}s ago`;
		const min = Math.floor(sec / 60);
		if (min < 60) return `${min}m ago`;
		const hr = Math.floor(min / 60);
		if (hr < 24) return `${hr}h ago`;
		const days = Math.floor(hr / 24);
		return `${days}d ago`;
	}

	// Latest build's evaluations grouped, used by the donut row.
	const latestBuildEvals = $derived.by<Evaluation[]>(() => {
		if (allEvals.length === 0) return [];
		const map = new Map<string, Evaluation[]>();
		for (const e of allEvals) {
			if (!e.build_id) continue;
			const list = map.get(e.build_id) ?? [];
			list.push(e);
			map.set(e.build_id, list);
		}
		let newestBid = '';
		let newestTs = 0;
		for (const [bid, group] of map.entries()) {
			const ts = Math.max(...group.map((e) => new Date(e.created_at).getTime()));
			if (ts > newestTs) {
				newestTs = ts;
				newestBid = bid;
			}
		}
		return map.get(newestBid) ?? [];
	});

	const latestComposite = $derived(sharedComposite(latestBuildEvals));

	function evalForCategory(evals: Evaluation[], cat: string): Evaluation | null {
		return evals.find((e) => e.category === cat) ?? null;
	}

	// Build history (last 10) for the score history chart.
	const chartData = $derived.by<BuildHistoryEntry[]>(() => {
		const map = new Map<string, Evaluation[]>();
		for (const e of allEvals) {
			if (!e.build_id) continue;
			const list = map.get(e.build_id) ?? [];
			list.push(e);
			map.set(e.build_id, list);
		}
		const out: BuildHistoryEntry[] = [];
		for (const [bid, group] of map.entries()) {
			const newest = group.reduce((acc, cur) =>
				new Date(cur.created_at).getTime() > new Date(acc.created_at).getTime() ? cur : acc
			);
			out.push({ build_id: bid, created_at: newest.created_at, evaluations: group });
		}
		out.sort((a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime());
		return out.slice(0, 10);
	});

	function categoryGradeBadgeVariant(g: string): 'success' | 'warning' | 'danger' | 'default' {
		const grade = asGradeShared(g);
		if (!grade) return 'default';
		const tone = gradeTone(grade);
		if (tone === 'success' || tone === 'success-soft') return 'success';
		if (tone === 'warning') return 'warning';
		if (tone === 'warning-strong' || tone === 'danger') return 'danger';
		return 'default';
	}

	function categoryGrade(evals: Evaluation[], cat: string): string {
		return evals.find((e) => e.category === cat)?.grade ?? '-';
	}

	const deployTargetOptions = $derived(
		deployTargets.map((t) => ({
			value: t.id,
			label: t.is_default ? `${t.name} (default)` : t.name
		}))
	);

	const deployTargetUsed = $derived(
		deployTargetUsedID ? deployTargets.find((t) => t.id === deployTargetUsedID) : null
	);

	$effect(() => {
		void loadRecent();
	});

	$effect(() => {
		void loadDeployTargets();
	});

	$effect(() => {
		nowInterval = setInterval(() => {
			nowTick = Date.now();
		}, 1_000);
		return () => {
			if (nowInterval) clearInterval(nowInterval);
		};
	});

	const buildState = $derived.by<BuildStatusState | null>(() => {
		// Touch nowTick so this re-runs every second while polling.
		void nowTick;
		if (!activeBuildID) return null;
		return getState(activeBuildID);
	});

	const elapsedSeconds = $derived.by(() => {
		if (!buildState) return 0;
		return Math.max(0, Math.floor((nowTick - buildState.started_at) / 1000));
	});

	$effect(() => {
		if (!buildState) return;
		if (lastTerminalReported === buildState.status) return;
		if (buildState.status === 'success') {
			lastTerminalReported = 'success';
			toast.success('Build succeeded. View evaluations to see results.');
			void loadRecent();
		} else if (buildState.status === 'failed') {
			lastTerminalReported = 'failed';
			toast.error(buildState.error || 'Build failed.');
			void loadRecent();
		} else if (buildState.status === 'timeout') {
			lastTerminalReported = 'timeout';
			toast.error('Build timed out after 5 minutes.');
		}
	});

	$effect(() => {
		if (!buildState || !logEl) return;
		// Touch build_log to subscribe.
		const _ = buildState.build_log;
		void _;
		if (!userScrolledUp) {
			queueMicrotask(() => {
				if (logEl) logEl.scrollTop = logEl.scrollHeight;
			});
		}
	});

	function onLogScroll() {
		if (!logEl) return;
		const distFromBottom = logEl.scrollHeight - (logEl.scrollTop + logEl.clientHeight);
		userScrolledUp = distFromBottom > 12;
	}

	async function onTrigger() {
		if (triggering) return;
		triggering = true;
		try {
			const resp = await buildsApi.triggerBuild(siteID);
			activeBuildID = resp.build_id;
			lastTerminalReported = null;
			userScrolledUp = false;
			if (stopFn) stopFn();
			stopFn = pollBuild(siteID, resp.build_id);
			toast.info('Build started.');
		} catch (err) {
			toast.error(err instanceof Error ? err.message : 'Failed to trigger build.');
		} finally {
			triggering = false;
		}
	}

	function onClearActive() {
		if (stopFn) stopFn();
		stopFn = null;
		if (activeBuildID) clearBuild(activeBuildID);
		activeBuildID = null;
		userScrolledUp = false;
		lastTerminalReported = null;
	}

	$effect(() => {
		return () => {
			if (stopFn) stopFn();
		};
	});

	function statusVariant(s: BuildStatusValue): 'success' | 'warning' | 'danger' | 'default' {
		if (s === 'success') return 'success';
		if (s === 'building' || s === 'pending') return 'warning';
		if (s === 'failed' || s === 'timeout') return 'danger';
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

	const isTerminal = $derived.by(() => {
		if (!buildState) return false;
		return (
			buildState.status === 'success' ||
			buildState.status === 'failed' ||
			buildState.status === 'timeout'
		);
	});
</script>

<div class="mx-auto max-w-7xl px-6 py-8">
	<header class="mb-6 flex flex-wrap items-start justify-between gap-4">
		<div>
			<h1 class="font-display text-3xl font-extralight tracking-tight text-text-primary">Build</h1>
			<p class="mt-1 text-[13px] text-text-secondary">
				Compile your site to a static Astro project.
			</p>
		</div>
		<Button
			variant="primary"
			loading={triggering || (buildState !== null && !isTerminal)}
			disabled={triggering || (buildState !== null && !isTerminal)}
			onclick={onTrigger}
		>
			{buildState && !isTerminal ? 'Building...' : 'Trigger build'}
		</Button>
	</header>

	<!-- Latest scores: 5-up donut row from the most recent build's evaluations. -->
	<section class="mt-6">
		<Card padding="md">
			<div class="flex items-baseline justify-between">
				<div>
					<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">
						Latest scores
					</h2>
					<p class="mt-1 text-[12px] text-text-muted">
						Security, SEO, performance, accessibility, and privacy from the most recent build.
					</p>
				</div>
				{#if latestComposite}
					<div class="flex items-center gap-2 text-right">
						{#if latestBuildEvals[0]?.profile}
							<span
								class="rounded-full border border-border-light bg-bg-elevated/60 px-2 py-0.5 font-mono text-[10px] uppercase tracking-[0.14em] text-text-secondary"
								title="Design fidelity this build was graded under"
							>
								{latestBuildEvals[0].profile}
							</span>
						{/if}
						<span class="text-[11px] text-text-muted">Composite</span>
						<GradeBadge grade={latestComposite} size="sm" />
					</div>
				{/if}
			</div>

			<div class="mt-5">
				{#if recentLoading}
					<div class="grid grid-cols-2 gap-6 sm:grid-cols-3 lg:grid-cols-5">
						{#each CATEGORIES as cat (cat)}
							<div class="flex flex-col items-center gap-2">
								<Skeleton width="8rem" height="8rem" rounded="full" />
								<Skeleton width="3rem" height="0.7rem" />
							</div>
						{/each}
					</div>
				{:else if latestBuildEvals.length === 0}
					<p class="py-6 text-center text-[12px] text-text-muted">
						No scores yet. Trigger a build to score this site.
					</p>
				{:else}
					<div class="grid grid-cols-2 gap-6 sm:grid-cols-3 lg:grid-cols-5">
						{#each CATEGORIES as cat (cat)}
							<CategoryDonut
								category={cat}
								evaluation={evalForCategory(latestBuildEvals, cat)}
							/>
						{/each}
					</div>
				{/if}
			</div>
		</Card>
	</section>

	<!-- Score history: trend chart across the last 10 builds. -->
	{#if chartData.length > 0}
		<section class="mt-4">
			<Card padding="md">
				<div class="flex items-baseline justify-between">
					<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">
						Score history
					</h2>
					<span class="text-[11px] text-text-muted">
						{chartData.length} build{chartData.length === 1 ? '' : 's'}
					</span>
				</div>
				<div class="mt-3">
					<ScoreHistoryChart builds={chartData} height={180} />
				</div>
			</Card>
		</section>
	{/if}

	{#if buildState}
		<section class="mt-6">
			<Card padding="md">
				<div class="flex flex-wrap items-center justify-between gap-3">
					<div class="flex items-center gap-3">
						<Badge variant={statusVariant(buildState.status)} dot>
							{buildState.status}
						</Badge>
						<span class="text-[12px] text-text-muted">
							{elapsedSeconds}s elapsed
						</span>
						<span class="text-[12px] text-text-muted">
							{buildState.pages_built} page{buildState.pages_built === 1 ? '' : 's'} built
						</span>
					</div>
					<div class="flex items-center gap-2">
						{#if isTerminal && buildState.status === 'success'}
							<Button
								variant="secondary"
								onclick={() => goto(`/sites/${siteID}/evaluations/${buildState!.buildID}`)}
							>
								View evaluation
							</Button>
						{/if}
						{#if isTerminal}
							<Button variant="ghost" onclick={onClearActive}>Dismiss</Button>
						{/if}
					</div>
				</div>

				{#if buildState.error}
					<p class="mt-3 text-[12px] text-danger">{buildState.error}</p>
				{/if}

				<pre
					bind:this={logEl}
					onscroll={onLogScroll}
					class="mt-4 rounded-lg border border-border-light bg-bg-elevated p-3 font-mono text-xs whitespace-pre-wrap max-h-[60vh] overflow-auto text-text-primary"
				>{buildState.build_log || 'Waiting for output...'}</pre>
			</Card>
		</section>
	{/if}

	{#if buildState && buildState.status === 'success'}
		<section class="mt-6">
			<Card padding="md">
				<div class="flex items-start justify-between gap-3">
					<div>
						<h2 class="text-[13px] font-medium text-text-primary">Deploy</h2>
						<p class="mt-1 text-[12px] text-text-muted">Publish this build to a configured target.</p>
					</div>
				</div>

				{#if deployTargetsLoading}
					<div class="mt-4 flex items-center gap-2 text-[12px] text-text-muted">
						<Skeleton width="16rem" height="2.25rem" />
					</div>
				{:else if deployTargets.length === 0}
					<div
						class="mt-4 flex items-center justify-between gap-3 rounded-lg border border-border-light bg-bg-elevated p-3"
					>
						<p class="text-[12px] text-text-muted">
							No deploy targets configured. Add one to publish this build, or
							download the static <code class="font-mono">dist/</code> bundle and
							ship it anywhere yourself.
						</p>
						<div class="flex items-center gap-2">
							<a
								href={`/api/sites/${siteID}/dist.zip`}
								class="inline-flex h-7 items-center gap-1.5 rounded-lg border border-border-light bg-bg-surface px-2 text-[12px] text-text-secondary hover:bg-bg-hover hover:text-text-primary transition-colors"
							>
								<Download size={12} strokeWidth={1.75} />
								Download ZIP
							</a>
							<a
								href={`/sites/${siteID}/settings/deployment`}
								class="text-[12px] font-medium text-accent hover:underline"
							>
								Add target
							</a>
						</div>
					</div>
				{:else}
					<div class="mt-4 flex flex-wrap items-end gap-3">
						<div class="flex-1 min-w-[16rem]">
							<label for="deploy-target" class="text-[12px] font-medium text-text-secondary"
								>Target</label
							>
							<div class="mt-1.5">
								<Select options={deployTargetOptions} bind:value={deployTargetID} />
							</div>
						</div>
						<Button
							variant="primary"
							loading={deploying}
							disabled={deploying || !deployTargetID}
							onclick={onDeploy}
						>
							Deploy
						</Button>
						<a
							href={`/api/sites/${siteID}/dist.zip`}
							class="inline-flex h-9 items-center gap-1.5 rounded-lg border border-border-light bg-bg-surface px-3 text-[12px] font-medium text-text-secondary hover:bg-bg-hover hover:text-text-primary transition-colors"
							title="Download the latest dist/ as a ZIP archive"
						>
							<Download size={12} strokeWidth={1.75} />
							Download ZIP
						</a>
					</div>

					{#if deployResult}
						<div class="mt-4 rounded-lg border border-border-light bg-bg-elevated p-3">
							<div class="flex flex-wrap items-center justify-between gap-2">
								<div class="flex items-center gap-2">
									<Badge variant="success" dot>deployed</Badge>
									{#if deployTargetUsed}
										<span class="text-[12px] text-text-muted">via {deployTargetUsed.name}</span>
									{/if}
									{#if deployedAt}
										<span class="text-[12px] text-text-muted">{formatRelative(deployedAt)}</span>
									{/if}
								</div>
							</div>
							{#if deployResult.deploy_url}
								<div class="mt-2 flex flex-wrap items-center gap-2">
									<code class="font-mono text-[12px] text-text-primary truncate">
										{deployResult.deploy_url}
									</code>
									<button
										type="button"
										class="inline-flex h-7 items-center gap-1.5 rounded-lg border border-border-light bg-bg-surface px-2 text-[12px] text-text-secondary hover:bg-bg-hover hover:text-text-primary transition-colors"
										onclick={copyDeployURL}
										aria-label="Copy URL"
									>
										<CopyIcon size={12} strokeWidth={1.75} />
										Copy
									</button>
									<a
										href={deployResult.deploy_url}
										target="_blank"
										rel="noopener noreferrer"
										class="inline-flex h-7 items-center gap-1.5 rounded-lg border border-border-light bg-bg-surface px-2 text-[12px] text-text-secondary hover:bg-bg-hover hover:text-text-primary transition-colors"
									>
										<ExternalLink size={12} strokeWidth={1.75} />
										Open
									</a>
								</div>
							{/if}
						</div>
					{/if}
				{/if}
			</Card>
		</section>
	{/if}

	<section class="mt-8">
		<div class="flex items-baseline justify-between">
			<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">
				Build history
			</h2>
			{#if recentRows.length > 0}
				<span class="text-[11px] text-text-muted">
					last {recentRows.length} build{recentRows.length === 1 ? '' : 's'}
				</span>
			{/if}
		</div>

		<div class="mt-3">
			{#if recentLoading}
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
			{:else if recentError}
				<Card padding="md">
					<p class="text-[13px] text-danger">{recentError}</p>
				</Card>
			{:else if recentRows.length === 0}
				<Card padding="none">
					<EmptyState
						title="No builds yet"
						description="Trigger your first build to compile and score this site."
					/>
				</Card>
			{:else}
				<div class="space-y-2">
					{#each recentRows as row (row.buildID)}
						<Card padding="sm" hoverable>
							<div class="flex flex-wrap items-center gap-4">
								{#if row.composite}
									<GradeBadge grade={row.composite} size="sm" />
								{:else}
									<span
										class="inline-flex h-9 w-9 items-center justify-center rounded-2xl bg-bg-elevated text-text-muted text-base"
									>
										?
									</span>
								{/if}
								<div class="min-w-0 flex-1">
									<p class="text-[13px] font-medium text-text-primary">
										{formatDate(row.created_at)}
									</p>
									<p class="font-mono text-[11px] text-text-muted truncate">
										{row.buildID}
									</p>
									{#if row.deployURL && row.deployedAt}
										<div class="mt-1 flex items-center gap-2">
											<Badge variant="success" dot>deployed</Badge>
											<a
												href={row.deployURL}
												target="_blank"
												rel="noopener noreferrer"
												class="font-mono text-[11px] text-text-muted hover:text-text-primary truncate transition-colors"
											>
												{row.deployURL}
											</a>
											<span class="text-[11px] text-text-muted">
												{formatRelative(row.deployedAt)}
											</span>
										</div>
									{/if}
								</div>
								<div class="flex flex-wrap gap-1.5">
									{#each CATEGORIES as cat (cat)}
										{@const g = categoryGrade(row.evaluations, cat)}
										<Badge variant={categoryGradeBadgeVariant(g)}>
											<span class="capitalize">{cat.slice(0, 3)}</span>
											<span class="ml-1 font-mono">{g}</span>
										</Badge>
									{/each}
								</div>
								<a
									href={`/sites/${siteID}/evaluations/${row.buildID}`}
									class="text-[12px] text-text-secondary hover:text-text-primary transition-colors"
								>
									View
								</a>
							</div>
						</Card>
					{/each}
				</div>
			{/if}
		</div>
	</section>
</div>
