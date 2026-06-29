<script lang="ts">
	import * as cwvApi from '$lib/api/cwv';
	import Card from '$lib/components/ui/Card.svelte';
	import Badge from '$lib/components/ui/Badge.svelte';
	import Skeleton from '$lib/components/ui/Skeleton.svelte';
	import { Gauge } from 'lucide-svelte';

	let { siteID }: { siteID: string } = $props();

	let since = $state<'7d' | '30d'>('7d');
	let loading = $state(true);
	let err = $state<string | null>(null);
	let data = $state<cwvApi.CWVOverview | null>(null);

	async function load() {
		loading = true;
		err = null;
		try {
			data = await cwvApi.getCWVOverview(siteID, since);
		} catch (e) {
			err = e instanceof Error ? e.message : 'Failed to load CWV';
			data = null;
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		void load();
	});

	function setSince(next: '7d' | '30d') {
		since = next;
	}

	function fmt(metric: string, p75: number): string {
		if (p75 <= 0) return '-';
		if (metric === 'CLS') return p75.toFixed(3);
		if (p75 < 100) return `${Math.round(p75)}ms`;
		if (p75 < 1000) return `${Math.round(p75)}ms`;
		return `${(p75 / 1000).toFixed(2)}s`;
	}

	function variantFor(rating: string): 'success' | 'warning' | 'danger' | 'info' {
		if (rating === 'good') return 'success';
		if (rating === 'needs-improvement') return 'warning';
		if (rating === 'poor') return 'danger';
		return 'info';
	}

	function badgeLabel(rating: string): string {
		if (rating === 'good') return 'Good';
		if (rating === 'needs-improvement') return 'Needs work';
		if (rating === 'poor') return 'Poor';
		return 'No data';
	}

	// Filter the response down to the "all devices" row per metric (the
	// API returns one row per (metric, device) for all + desktop +
	// mobile). The card displays the all-devices p75 + a small
	// desktop/mobile split underneath for context.
	const allDevicesByMetric = $derived(() => {
		const out: Record<string, cwvApi.CWVMetricResult> = {};
		if (!data) return out;
		for (const m of data.metrics) {
			if (m.device === '') out[m.metric] = m;
		}
		return out;
	});

	const splitByMetric = $derived(() => {
		const out: Record<string, { desktop?: cwvApi.CWVMetricResult; mobile?: cwvApi.CWVMetricResult }> = {};
		if (!data) return out;
		for (const m of data.metrics) {
			if (m.device === 'desktop') {
				out[m.metric] = { ...(out[m.metric] ?? {}), desktop: m };
			} else if (m.device === 'mobile') {
				out[m.metric] = { ...(out[m.metric] ?? {}), mobile: m };
			}
		}
		return out;
	});

	const metricOrder: cwvApi.CWVMetricResult['metric'][] = ['LCP', 'INP', 'CLS', 'FCP', 'TTFB'];
</script>

<Card padding="md">
	<header class="flex items-center justify-between gap-3">
		<div class="flex items-center gap-2">
			<Gauge class="h-4 w-4 text-text-secondary" strokeWidth={1.5} />
			<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">
				Core Web Vitals (p75, last {since === '7d' ? '7 days' : '30 days'})
			</h2>
		</div>
		<div class="flex gap-1.5">
			{#each ['7d', '30d'] as opt (opt)}
				<button
					type="button"
					class="rounded-full border px-2.5 py-1 text-[11px] font-medium transition-colors {since === opt
						? 'border-accent bg-accent/10 text-text-primary'
						: 'border-border-light text-text-muted hover:text-text-primary'}"
					onclick={() => setSince(opt as '7d' | '30d')}
				>
					{opt}
				</button>
			{/each}
		</div>
	</header>

	{#if loading}
		<div class="mt-4 grid grid-cols-1 gap-3 md:grid-cols-3 lg:grid-cols-5">
			{#each metricOrder as m (m)}
				<Skeleton class="h-20" />
			{/each}
		</div>
	{:else if err}
		<p class="mt-4 text-[12px] text-danger">{err}</p>
	{:else if !data || (data.metrics.length === 0)}
		<p class="mt-4 text-[12px] text-text-muted">
			No CWV samples yet. Enable "Core Web Vitals collection" under Settings -> Analytics; after
			the next build, real visitors will populate this dashboard. Samples typically appear within
			minutes of first traffic.
		</p>
	{:else}
		{@const all = allDevicesByMetric()}
		{@const split = splitByMetric()}
		<div class="mt-4 grid grid-cols-2 gap-3 md:grid-cols-3 lg:grid-cols-5">
			{#each metricOrder as metric (metric)}
				{@const m = all[metric]}
				<div class="rounded-xl border border-border-light bg-bg-surface p-3">
					<div class="flex items-center justify-between gap-2">
						<span class="font-mono text-[11px] uppercase tracking-[0.16em] text-text-muted">
							{metric}
						</span>
						{#if m && m.samples > 0}
							<Badge variant={variantFor(m.rating)}>{badgeLabel(m.rating)}</Badge>
						{/if}
					</div>
					<p class="mt-2 font-mono text-2xl font-normal tracking-tight tabular-nums text-text-primary">
						{m && m.samples > 0 ? fmt(metric, m.p75) : '-'}
					</p>
					<p class="mt-1 text-[11px] text-text-muted">
						{m ? `${m.samples} sample${m.samples === 1 ? '' : 's'}` : '0 samples'}
					</p>
					{#if split[metric] && (split[metric].desktop?.samples || split[metric].mobile?.samples)}
						<div class="mt-2 flex flex-col gap-0.5 text-[11px] text-text-muted">
							{#if split[metric].desktop && split[metric].desktop.samples > 0}
								<div class="flex justify-between">
									<span>Desktop</span>
									<span class="text-text-primary">{fmt(metric, split[metric].desktop.p75)}</span>
								</div>
							{/if}
							{#if split[metric].mobile && split[metric].mobile.samples > 0}
								<div class="flex justify-between">
									<span>Mobile</span>
									<span class="text-text-primary">{fmt(metric, split[metric].mobile.p75)}</span>
								</div>
							{/if}
						</div>
					{/if}
				</div>
			{/each}
		</div>
		<p class="mt-3 text-[11px] text-text-muted">
			Ratings follow Google's published thresholds: LCP ≤2.5s, INP ≤200ms, CLS ≤0.1, FCP ≤1.8s,
			TTFB ≤0.8s. p75 = the visitor experience at the 75th percentile, so 25% of visits were
			slower than the value shown.
		</p>
	{/if}
</Card>
