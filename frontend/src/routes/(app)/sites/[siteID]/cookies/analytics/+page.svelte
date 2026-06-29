<script lang="ts">
	import * as consentApi from '$lib/api/consent';
	import * as ca from '$lib/api/cookieAnalytics';
	import { ApiError } from '$lib/api/client';
	import Card from '$lib/components/ui/Card.svelte';
	import Spinner from '$lib/components/ui/Spinner.svelte';
	import { toast } from '$lib/stores/toast.svelte';
	import type { Site } from '$lib/api/types';

	let { data }: { data: { site: Site } } = $props();

	const siteID = $derived(data.site.id);

	let loading = $state(true);
	let stats = $state<consentApi.ConsentStats | null>(null);

	// Stitched cookie analytics . DuckDB ATTACH on the SQLite file.
	// Each report is a separate fetch so partial failures (e.g. 503 when
	// DuckDB is unavailable) only hide the affected card.
	let funnel = $state<ca.FunnelKPIs | null>(null);
	let preConsent = $state<ca.PreConsentShare | null>(null);
	let timeToConsent = $state<ca.TimeToConsent | null>(null);
	let engaged = $state<ca.EngagedAcceptRate | null>(null);
	let topPages = $state<ca.AcceptingPage[] | null>(null);
	let stitchedUnavailable = $state(false);

	async function load() {
		loading = true;
		try {
			stats = await consentApi.stats(siteID);
		} catch (err) {
			toast.error(err instanceof ApiError ? err.message : 'Failed to load consent stats.');
		} finally {
			loading = false;
		}

		// Fire all stitched queries in parallel; settle each independently
		// so DuckDB unavailability doesn't take the whole page down.
		const settled = await Promise.allSettled([
			ca.funnel(siteID),
			ca.preConsent(siteID),
			ca.timeToConsent(siteID),
			ca.engagedAcceptRate(siteID),
			ca.topAcceptingPages(siteID, { limit: 10 })
		]);
		const [fRes, pRes, tRes, eRes, topRes] = settled;
		if (fRes.status === 'fulfilled') funnel = fRes.value.kpis;
		if (pRes.status === 'fulfilled') preConsent = pRes.value;
		if (tRes.status === 'fulfilled') timeToConsent = tRes.value;
		if (eRes.status === 'fulfilled') engaged = eRes.value;
		if (topRes.status === 'fulfilled') topPages = topRes.value.rows ?? [];
		// If every stitched call failed with 503, surface the banner once.
		if (settled.every((s) => s.status === 'rejected' && s.reason instanceof ApiError && (s.reason as ApiError).status === 503)) {
			stitchedUnavailable = true;
		}
	}

	function fmtPct(v: number | undefined | null): string {
		if (v === undefined || v === null || Number.isNaN(v)) return 'n/a';
		return v.toFixed(1) + '%';
	}

	function fmtSeconds(s: number | undefined | null): string {
		if (s === undefined || s === null || s <= 0) return 'n/a';
		if (s < 60) return s.toFixed(0) + 's';
		if (s < 3600) return (s / 60).toFixed(1) + ' min';
		return (s / 3600).toFixed(1) + ' h';
	}

	$effect(() => {
		void load();
	});

	function pct(part: number, total: number): string {
		if (total === 0) return '0%';
		return Math.round((part / total) * 100) + '%';
	}

	const dailyMax = $derived.by(() => {
		if (!stats || stats.daily.length === 0) return 1;
		let max = 0;
		for (const d of stats.daily) {
			if (d.total > max) max = d.total;
		}
		return Math.max(1, max);
	});

	function methodBarColor(method: string): string {
		switch (method) {
			case 'accept-all':
				return 'var(--t-success)';
			case 'reject-all':
				return 'var(--t-danger)';
			case 'gpc':
				return 'var(--t-info)';
			case 'custom':
				return 'var(--t-warning)';
			default:
				return 'var(--t-text-muted)';
		}
	}
</script>

<div class="mx-auto max-w-7xl px-6 py-8">
	<header class="flex flex-col gap-1.5">
		<h1 class="font-display text-3xl font-extralight tracking-tight text-text-primary">
			Consent analytics
		</h1>
		<p class="text-[13px] text-text-secondary">
			Last 30 days. Aggregate splits by decision method and a daily time-series.
		</p>
	</header>

	{#if loading || !stats}
		<div class="mt-8 flex items-center justify-center py-12">
			<Spinner />
		</div>
	{:else}
		<div class="mt-6 grid grid-cols-2 gap-4 lg:grid-cols-4">
			<Card padding="md">
				<div class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">Total proofs</div>
				<div class="mt-1 font-display text-3xl font-light text-text-primary">{stats.total}</div>
			</Card>
			<Card padding="md">
				<div class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">Accept rate</div>
				<div class="mt-1 font-display text-3xl font-light text-emerald-600">
					{pct(stats.accepts, stats.total)}
				</div>
				<div class="mt-1 text-[11px] text-text-muted">{stats.accepts} accept-all</div>
			</Card>
			<Card padding="md">
				<div class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">Reject rate</div>
				<div class="mt-1 font-display text-3xl font-light text-rose-600">
					{pct(stats.rejects, stats.total)}
				</div>
				<div class="mt-1 text-[11px] text-text-muted">{stats.rejects} reject-all</div>
			</Card>
			<Card padding="md">
				<div class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">GPC share</div>
				<div class="mt-1 font-display text-3xl font-light text-violet-600">
					{pct(stats.gpcs, stats.total)}
				</div>
				<div class="mt-1 text-[11px] text-text-muted">{stats.gpcs} signals honored</div>
			</Card>
		</div>

		<Card padding="md" class="mt-5">
			<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">
				Daily proofs (stacked by method)
			</h2>
			{#if stats.daily.length === 0}
				<p class="mt-4 text-[12px] text-text-muted">No proofs in the selected range.</p>
			{:else}
				<div class="mt-4 flex items-end gap-1.5 overflow-x-auto pb-2">
					{#each stats.daily as d (d.day)}
						<div class="flex min-w-[36px] flex-col items-center gap-1">
							<div class="flex h-32 w-7 flex-col-reverse rounded-t bg-bg-surface" title={`${d.day}: ${d.total}`}>
								{#each Object.entries(d.by_method) as [method, count]}
									<div
										class="w-full"
										style:height="{(count / dailyMax) * 100}%"
										style:background-color={methodBarColor(method)}
										title={`${method}: ${count}`}
									></div>
								{/each}
							</div>
							<div class="font-mono text-[10px] text-text-muted">{d.day.slice(5)}</div>
						</div>
					{/each}
				</div>
				<div class="mt-3 flex flex-wrap gap-3 text-[11px] text-text-muted">
					{#each ['accept-all', 'reject-all', 'custom', 'gpc'] as m}
						<span class="inline-flex items-center gap-1.5">
							<span class="h-2 w-3 rounded" style:background-color={methodBarColor(m)}></span>
							{m}
						</span>
					{/each}
				</div>
			{/if}
		</Card>

		<Card padding="md" class="mt-5">
			<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">
				Method distribution
			</h2>
			<div class="mt-4 flex flex-col gap-2">
				{#each [
					{ label: 'Accept all', count: stats.accepts, method: 'accept-all' },
					{ label: 'Reject all', count: stats.rejects, method: 'reject-all' },
					{ label: 'Custom', count: stats.customs, method: 'custom' },
					{ label: 'GPC', count: stats.gpcs, method: 'gpc' },
					{ label: 'DNS', count: stats.dns, method: 'dns' },
					{ label: 'Do Not Sell', count: stats.do_not_sell, method: 'do-not-sell' }
				] as r (r.method)}
					<div class="flex items-center gap-3">
						<span class="w-24 text-[12px] text-text-secondary">{r.label}</span>
						<div class="flex-1 h-3 rounded bg-bg-surface">
							<div
								class="h-full rounded"
								style:width={pct(r.count, stats.total)}
								style:background-color={methodBarColor(r.method)}
							></div>
						</div>
						<span class="w-12 text-right font-mono text-[11px] text-text-muted">{r.count}</span>
					</div>
				{/each}
			</div>
		</Card>
	{/if}

	<!-- Stitched cookie analytics. These sections live below the existing
	     consent-only stats and pull from the DuckDB ATTACH layer. Each
	     card hides itself if its data didn't load (DuckDB unavailable or
	     empty). -->
	{#if stitchedUnavailable}
		<Card padding="md" class="mt-5 border-amber-300/40 bg-amber-50/40">
			<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-amber-900">
				Stitched analytics unavailable
			</h2>
			<p class="mt-2 text-[12px] text-amber-900/80">
				The DuckDB layer isn't responding. The basic consent counters above are
				still accurate; the cross-table reports require it. Check server logs
				for <code>analyticsdb:</code> messages.
			</p>
		</Card>
	{:else}
		<div class="mt-8 flex items-end justify-between gap-3">
			<div>
				<h2 class="font-display text-xl font-extralight tracking-tight text-text-primary">
					Stitched analytics
				</h2>
				<p class="text-[12px] text-text-secondary">
					Cross-table reports the dashboard can answer because consent and traffic
					live in the same database. Last 30 days.
				</p>
			</div>
		</div>

		{#if funnel}
			<div class="mt-4 grid grid-cols-2 gap-4 lg:grid-cols-4">
				<Card padding="md">
					<div class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">Visitors</div>
					<div class="mt-1 font-display text-3xl font-light text-text-primary">{funnel.unique_visitors}</div>
					<div class="mt-1 text-[11px] text-text-muted">{funnel.consenting_visitors} reached the banner</div>
				</Card>
				<Card padding="md">
					<div class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">Banner abandoned</div>
					<div class="mt-1 font-display text-3xl font-light text-amber-700">
						{funnel.unique_visitors > 0 ? fmtPct(100 * funnel.abandoned_banner_visitors / funnel.unique_visitors) : '.'}
					</div>
					<div class="mt-1 text-[11px] text-text-muted">{funnel.abandoned_banner_visitors} left without choosing</div>
				</Card>
				<Card padding="md">
					<div class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">Identified</div>
					<div class="mt-1 font-display text-3xl font-light text-violet-600">{funnel.identified_visitors}</div>
					<div class="mt-1 text-[11px] text-text-muted">CRM-confirmed contacts</div>
				</Card>
				<Card padding="md">
					<div class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">Custom decisions</div>
					<div class="mt-1 font-display text-3xl font-light text-text-primary">{funnel.custom_visitors}</div>
					<div class="mt-1 text-[11px] text-text-muted">Toggled categories individually</div>
				</Card>
			</div>
		{/if}

		{#if preConsent}
			{@const total = preConsent.total_pageviews}
			<Card padding="md" class="mt-5">
				<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">
					Pre-consent traffic share
				</h2>
				<p class="mt-2 text-[12px] text-text-muted">
					What share of pageviews fired before the visitor consented. Tells you
					how much GA4 / Marketing data you're legally missing.
				</p>
				<div class="mt-4 flex flex-col gap-2">
					<div class="flex items-center gap-3">
						<span class="w-44 text-[12px] text-text-secondary">Before / no consent</span>
						<div class="h-3 flex-1 rounded bg-bg-surface">
							<div class="h-full rounded bg-amber-500"
								style:width={total > 0 ? fmtPct(100 * preConsent.pageviews_before_or_no_consent / total) : '0%'}
							></div>
						</div>
						<span class="w-16 text-right font-mono text-[11px] text-text-muted">{preConsent.pageviews_before_or_no_consent}</span>
					</div>
					<div class="flex items-center gap-3">
						<span class="w-44 text-[12px] text-text-secondary">After consent (accept / custom)</span>
						<div class="h-3 flex-1 rounded bg-bg-surface">
							<div class="h-full rounded bg-emerald-500"
								style:width={total > 0 ? fmtPct(100 * preConsent.pageviews_after_consent / total) : '0%'}
							></div>
						</div>
						<span class="w-16 text-right font-mono text-[11px] text-text-muted">{preConsent.pageviews_after_consent}</span>
					</div>
					<div class="flex items-center gap-3">
						<span class="w-44 text-[12px] text-text-secondary">After reject (incl. GPC)</span>
						<div class="h-3 flex-1 rounded bg-bg-surface">
							<div class="h-full rounded bg-rose-500"
								style:width={total > 0 ? fmtPct(100 * preConsent.pageviews_after_reject / total) : '0%'}
							></div>
						</div>
						<span class="w-16 text-right font-mono text-[11px] text-text-muted">{preConsent.pageviews_after_reject}</span>
					</div>
				</div>
			</Card>
		{/if}

		<div class="mt-5 grid grid-cols-1 gap-5 lg:grid-cols-2">
			{#if timeToConsent}
				<Card padding="md">
					<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">
						Time to consent
					</h2>
					<p class="mt-2 text-[12px] text-text-muted">
						Seconds from first pageview to consent decision. Lower means
						the banner is doing its job.
					</p>
					<div class="mt-4 grid grid-cols-3 gap-3">
						<div>
							<div class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">P50</div>
							<div class="mt-1 font-display text-2xl font-light text-text-primary">{fmtSeconds(timeToConsent.p50_seconds)}</div>
						</div>
						<div>
							<div class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">P90</div>
							<div class="mt-1 font-display text-2xl font-light text-text-primary">{fmtSeconds(timeToConsent.p90_seconds)}</div>
						</div>
						<div>
							<div class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">Mean</div>
							<div class="mt-1 font-display text-2xl font-light text-text-primary">{fmtSeconds(timeToConsent.mean_seconds)}</div>
						</div>
					</div>
					<div class="mt-3 text-[11px] text-text-muted">Sample: {timeToConsent.sample_size} visitors</div>
				</Card>
			{/if}

			{#if engaged}
				<Card padding="md">
					<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">
						Engaged vs not-engaged accept rate
					</h2>
					<p class="mt-2 text-[12px] text-text-muted">
						Visitors who scrolled past 50% or stayed more than 30s vs the rest.
						A higher engaged rate confirms the page is earning trust.
					</p>
					<div class="mt-4 grid grid-cols-2 gap-3">
						<div>
							<div class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">Engaged</div>
							<div class="mt-1 font-display text-2xl font-light text-emerald-600">{fmtPct(engaged.engaged_accept_rate_pct)}</div>
							<div class="mt-1 text-[11px] text-text-muted">{engaged.engaged_accepted} of {engaged.engaged_visitors} accepted</div>
						</div>
						<div>
							<div class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">Not engaged</div>
							<div class="mt-1 font-display text-2xl font-light text-text-secondary">{fmtPct(engaged.not_engaged_accept_rate_pct)}</div>
							<div class="mt-1 text-[11px] text-text-muted">{engaged.not_engaged_accepted} of {engaged.not_engaged_visitors} accepted</div>
						</div>
					</div>
				</Card>
			{/if}
		</div>

		{#if topPages && topPages.length > 0}
			<Card padding="md" class="mt-5">
				<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">
					Top accepting entry pages
				</h2>
				<p class="mt-2 text-[12px] text-text-muted">
					Landing paths with the highest accept-all rate (visitors {'>='} 10).
				</p>
				<div class="mt-3 overflow-x-auto">
					<table class="w-full text-[12px]">
						<thead class="border-b border-border-light text-text-muted">
							<tr>
								<th class="px-2 py-2 text-left font-mono text-[11px] uppercase tracking-wider">Path</th>
								<th class="px-2 py-2 text-right font-mono text-[11px] uppercase tracking-wider">Visitors</th>
								<th class="px-2 py-2 text-right font-mono text-[11px] uppercase tracking-wider">Accepted</th>
								<th class="px-2 py-2 text-right font-mono text-[11px] uppercase tracking-wider">Rate</th>
							</tr>
						</thead>
						<tbody class="divide-y divide-border-light">
							{#each topPages as p (p.path)}
								<tr>
									<td class="px-2 py-2 font-mono text-[11px] text-text-primary">{p.path}</td>
									<td class="px-2 py-2 text-right text-text-secondary">{p.visitors}</td>
									<td class="px-2 py-2 text-right text-text-secondary">{p.accepted}</td>
									<td class="px-2 py-2 text-right font-medium text-emerald-700">{fmtPct(p.accept_rate_pct)}</td>
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
			</Card>
		{:else if topPages !== null}
			<Card padding="md" class="mt-5">
				<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">
					Top accepting entry pages
				</h2>
				<p class="mt-2 text-[12px] text-text-muted">
					No page has 10+ entry visitors yet . keep collecting traffic and this
					report will populate.
				</p>
			</Card>
		{/if}
	{/if}
</div>
