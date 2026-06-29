<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import * as api from '$lib/api';
	import type { Site } from '$lib/api/types';
	import Button from '$lib/components/ui/Button.svelte';
	import EmptyState from '$lib/components/ui/EmptyState.svelte';
	import Skeleton from '$lib/components/ui/Skeleton.svelte';
	import Tooltip from '$lib/components/ui/Tooltip.svelte';

	let loading = $state(true);
	let sites = $state<Site[]>([]);

	async function load(): Promise<void> {
		loading = true;
		try {
			const resp = await api.sites.list();
			sites = resp.sites ?? [];
		} finally {
			loading = false;
		}
	}

	function staggerClass(i: number): string {
		const idx = (i % 7) + 1;
		return `animate-stagger-${idx}`;
	}

	function statusTone(status: string): string {
		const s = (status || '').toLowerCase();
		if (s === 'published' || s === 'live') return 'status-dot-success';
		if (s === 'archived' || s === 'failed') return 'status-dot-danger';
		return 'status-dot-warning';
	}

	function statusLabel(status: string): string {
		if (!status) return 'draft';
		return status.toLowerCase();
	}

	interface Swatch {
		key: string;
		label: string;
		color: string;
	}

	function swatches(site: Site): Swatch[] {
		const items: Swatch[] = [
			{ key: 'primary', label: 'Primary', color: site.primary_color },
			{ key: 'secondary', label: 'Secondary', color: site.secondary_color },
			{ key: 'bg', label: 'Background', color: site.bg_color },
			{ key: 'text', label: 'Text', color: site.text_color }
		];
		return items.filter((s) => !!s.color);
	}

	onMount(() => {
		void load();
	});
</script>

<div class="px-8 py-12">
	<div class="mx-auto max-w-6xl space-y-10">
		<header class="flex items-end justify-between gap-6">
			<div class="space-y-2">
				<p class="font-mono text-[11px] uppercase tracking-[0.2em] text-text-muted">workspaces</p>
				<h1 class="page-title">
					Sites
				</h1>
			</div>
			<Button variant="primary" onclick={() => goto('/sites/new')}>New site</Button>
		</header>

		{#if loading}
			<section class="grid grid-cols-1 gap-5 md:grid-cols-2 lg:grid-cols-3">
				{#each Array(6) as _, i (i)}
					<div class="card p-6 space-y-4">
						<Skeleton width="60%" height="1.25rem" />
						<Skeleton width="40%" height="0.75rem" />
						<div class="flex gap-1.5">
							{#each Array(4) as _, j (j)}
								<Skeleton width={12} height={12} rounded="full" />
							{/each}
						</div>
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
			<section class="grid grid-cols-1 gap-5 md:grid-cols-2 lg:grid-cols-3">
				{#each sites as site, i (site.id)}
					<a
						href={`/sites/${site.id}`}
						class={`group block ${staggerClass(i)} transition-transform duration-200 hover:translate-y-[-2px]`}
					>
						<div class="card p-6 h-full flex flex-col gap-4">
							<div class="flex items-start justify-between gap-4">
								<div class="min-w-0 flex-1">
									<h3
										class="truncate font-display text-xl font-extralight tracking-tight text-text-primary"
									>
										{site.name}
									</h3>
									<p class="mt-1 truncate font-mono text-[11px] text-text-muted">
										{site.domain || site.slug}
									</p>
								</div>
								<svg
									class="h-4 w-4 shrink-0 text-text-muted transition-transform group-hover:translate-x-0.5"
									fill="none"
									viewBox="0 0 24 24"
									stroke-width="1.5"
									stroke="currentColor"
								>
									<path
										stroke-linecap="round"
										stroke-linejoin="round"
										d="M13.5 4.5l6 6m0 0l-6 6m6-6H3"
									/>
								</svg>
							</div>

							<div class="flex items-center gap-1.5">
								{#each swatches(site) as sw (sw.key)}
									<Tooltip content={`${sw.label} ${sw.color}`}>
										{#snippet trigger({ props })}
											<span
												{...props}
												class="block h-3 w-3 rounded-full ring-1 ring-border-light"
												style={`background-color: ${sw.color}`}
												aria-label={`${sw.label} color ${sw.color}`}
											></span>
										{/snippet}
									</Tooltip>
								{/each}
							</div>

							<div class="mt-auto flex items-center justify-between text-[11px] text-text-muted">
								<span class="inline-flex items-center gap-2">
									<span class={statusTone(site.status)} aria-hidden="true"></span>
									{statusLabel(site.status)}
								</span>
								<span class="opacity-0 transition-opacity group-hover:opacity-100">open</span>
							</div>
						</div>
					</a>
				{/each}
			</section>
		{/if}
	</div>
</div>
