<script lang="ts" module>
	import type { Evaluation } from '$lib/api/types';

	export interface BuildHistoryEntry {
		build_id: string;
		created_at: string;
		evaluations: Evaluation[];
	}
</script>

<script lang="ts">
	import { pctTone, toneCSSColor } from '$lib/evaluations/grade';
	import { CATEGORIES, CATEGORY_LABEL, type CategoryKey } from '$lib/evaluations/categories';

	let {
		builds
	}: {
		builds: BuildHistoryEntry[];
		// height kept for backwards compatibility; no longer used by the
		// small-multiples layout, which sizes itself to its rows.
		height?: number;
	} = $props();

	// Sparkline canvas. One per row. Stays compact so the whole panel
	// fits 5 rows in roughly the same vertical space the old chart used.
	const SPARK_W = 360;
	const SPARK_H = 36;
	const SPARK_PAD = 4;
	// Y range. Most scores live in 50-100; a 0-100 axis flattens the
	// trend into invisibility.
	const Y_MIN = 50;
	const Y_MAX = 100;

	const sorted = $derived(
		[...builds].sort(
			(a, b) => new Date(a.created_at).getTime() - new Date(b.created_at).getTime()
		)
	);

	function categoryPercent(entry: BuildHistoryEntry, cat: CategoryKey): number | null {
		const ev = entry.evaluations.find((e) => e.category === cat);
		if (!ev) return null;
		if (ev.max_score <= 0) return null;
		return Math.max(0, Math.min(100, (ev.score / ev.max_score) * 100));
	}

	interface Point {
		x: number;
		y: number;
		v: number;
	}

	function pointsFor(cat: CategoryKey): Point[] {
		const n = sorted.length;
		if (n === 0) return [];
		const innerW = SPARK_W - SPARK_PAD * 2;
		const innerH = SPARK_H - SPARK_PAD * 2;
		const step = n === 1 ? 0 : innerW / (n - 1);
		const range = Y_MAX - Y_MIN;
		const pts: Point[] = [];
		for (let i = 0; i < n; i++) {
			const entry = sorted[i];
			if (!entry) continue;
			const v = categoryPercent(entry, cat);
			if (v === null) continue;
			const x = n === 1 ? SPARK_PAD + innerW / 2 : SPARK_PAD + step * i;
			const clamped = Math.max(Y_MIN, Math.min(Y_MAX, v));
			const y = SPARK_PAD + innerH - ((clamped - Y_MIN) / range) * innerH;
			pts.push({ x, y, v });
		}
		return pts;
	}

	function smoothPath(pts: Point[]): string {
		if (pts.length === 0) return '';
		const first = pts[0];
		if (!first) return '';
		if (pts.length === 1) return `M ${first.x} ${first.y}`;
		let d = `M ${first.x} ${first.y}`;
		for (let i = 1; i < pts.length; i++) {
			const prev = pts[i - 1];
			const cur = pts[i];
			if (!prev || !cur) continue;
			const cx = (prev.x + cur.x) / 2;
			d += ` Q ${cx} ${prev.y} ${cx} ${(prev.y + cur.y) / 2}`;
			d += ` Q ${cx} ${cur.y} ${cur.x} ${cur.y}`;
		}
		return d;
	}

	interface Row {
		cat: CategoryKey;
		label: string;
		pts: Point[];
		latest: number | null;
		previous: number | null;
		delta: number | null;
		color: string;
	}

	const rows = $derived.by<Row[]>(() => {
		return CATEGORIES.map((cat) => {
			const pts = pointsFor(cat);
			const latest = pts.length > 0 ? (pts[pts.length - 1]?.v ?? null) : null;
			const previous = pts.length > 1 ? (pts[pts.length - 2]?.v ?? null) : null;
			const delta = latest !== null && previous !== null ? latest - previous : null;
			// Tone follows the latest value so each row's colour reads in
			// the same vocabulary as the donut + grade badge.
			const color =
				latest !== null
					? toneCSSColor(pctTone(latest))
					: 'var(--t-text-muted, #a3a3a3)';
			return {
				cat,
				label: CATEGORY_LABEL[cat],
				pts,
				latest,
				previous,
				delta,
				color
			};
		});
	});

	function formatPct(v: number | null): string {
		if (v === null) return '-';
		return Math.round(v).toString();
	}

	function formatDelta(d: number | null): string {
		if (d === null) return '';
		const r = Math.round(d * 10) / 10;
		if (r === 0) return '';
		const sign = r > 0 ? '+' : '';
		return `${sign}${r}`;
	}

	function deltaTone(d: number | null): string {
		if (d === null || Math.abs(d) < 0.5) return 'var(--t-text-muted, #a3a3a3)';
		return d > 0 ? 'var(--t-success, #15803d)' : 'var(--t-danger, #dc2626)';
	}

	function formatDateRange(): string {
		if (sorted.length < 2) return '';
		const first = sorted[0];
		const last = sorted[sorted.length - 1];
		if (!first || !last) return '';
		const f = new Date(first.created_at);
		const l = new Date(last.created_at);
		if (Number.isNaN(f.getTime()) || Number.isNaN(l.getTime())) return '';
		const fmt = (d: Date) =>
			d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
		return `${fmt(f)} → ${fmt(l)}`;
	}
</script>

<div class="w-full">
	{#if sorted.length === 0}
		<div
			class="flex h-32 items-center justify-center rounded-lg border border-border-light bg-bg-surface text-[12px] text-text-muted"
		>
			No build history yet.
		</div>
	{:else if sorted.length === 1}
		<div
			class="flex h-32 flex-col items-center justify-center gap-2 rounded-lg border border-dashed border-border-light bg-bg-surface px-6 text-center"
		>
			<p class="text-[12px] text-text-muted">
				One build so far. Trigger another to plot the trend.
			</p>
		</div>
	{:else}
		<div class="flex flex-col">
			{#each rows as row, i (row.cat)}
				<div
					class="grid grid-cols-[8.5rem_1fr_3rem_2.25rem] items-center gap-4 px-2 py-2.5 {i <
					rows.length - 1
						? 'border-b border-border-light/60'
						: ''}"
				>
					<!-- Category label, mono uppercase, restrained -->
					<span
						class="font-mono text-[10.5px] uppercase tracking-[0.18em] text-text-secondary"
					>
						{row.label}
					</span>

					<!-- Sparkline. Tone-coloured to match the donut rings. -->
					<svg
						viewBox="0 0 {SPARK_W} {SPARK_H}"
						preserveAspectRatio="none"
						class="block h-9 w-full"
						aria-hidden="true"
					>
						{#if row.pts.length >= 2}
							<path
								d={smoothPath(row.pts)}
								fill="none"
								stroke={row.color}
								stroke-width="1.5"
								stroke-linecap="round"
								stroke-linejoin="round"
								vector-effect="non-scaling-stroke"
							/>
							{@const last = row.pts[row.pts.length - 1]}
							{#if last}
								<circle
									cx={last.x}
									cy={last.y}
									r="2.5"
									fill="var(--color-bg-surface, #ffffff)"
									stroke={row.color}
									stroke-width="1.5"
									vector-effect="non-scaling-stroke"
								/>
							{/if}
						{:else if row.pts.length === 1}
							{@const only = row.pts[0]}
							{#if only}
								<circle
									cx={only.x}
									cy={only.y}
									r="2.5"
									fill={row.color}
								/>
							{/if}
						{/if}
					</svg>

					<!-- Latest value, big monospace numerals -->
					<span
						class="text-right font-display text-[18px] font-extralight tracking-tight tabular-nums leading-none text-text-primary"
					>
						{formatPct(row.latest)}<span class="text-[10px] text-text-muted">%</span>
					</span>

					<!-- Delta vs previous build, sign-coloured, tabular-nums -->
					<span
						class="text-right font-mono text-[10.5px] tabular-nums"
						style="color: {deltaTone(row.delta)};"
						title={row.delta !== null ? `${formatDelta(row.delta)} vs previous build` : ''}
					>
						{formatDelta(row.delta)}
					</span>
				</div>
			{/each}
		</div>

		<p class="mt-3 px-2 font-mono text-[10px] uppercase tracking-[0.18em] text-text-muted">
			{sorted.length} builds · {formatDateRange()}
		</p>
	{/if}
</div>
