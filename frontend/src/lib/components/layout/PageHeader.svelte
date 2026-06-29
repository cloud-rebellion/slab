<script lang="ts">
	import type { Snippet } from 'svelte';

	let {
		title,
		subtitle,
		eyebrow,
		actions
	}: {
		title: string;
		subtitle?: string;
		eyebrow?: string;
		actions?: Snippet;
	} = $props();
</script>

<!--
	Canonical page header. Uses the .page-title / .eyebrow tokens (app.css) so
	every page shares one type treatment instead of re-declaring text-3xl/4xl +
	tracking by hand. Title left, optional actions right.
-->
<div class="mb-6 flex items-start justify-between gap-4">
	<div class="min-w-0">
		{#if eyebrow}
			<p class="eyebrow mb-2">{eyebrow}</p>
		{/if}
		<h1 class="page-title">{title}</h1>
		{#if subtitle}
			<p class="mt-1 text-[13px] text-text-secondary">{subtitle}</p>
		{/if}
	</div>
	{#if actions}
		<div class="flex shrink-0 items-center gap-2">{@render actions()}</div>
	{/if}
</div>
