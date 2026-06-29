<script lang="ts">
	import type { HTMLInputAttributes } from 'svelte/elements';

	let {
		label,
		error,
		hint,
		id,
		value = $bindable(),
		class: className = '',
		...rest
	}: HTMLInputAttributes & {
		label?: string;
		error?: string;
		hint?: string;
		class?: string;
	} = $props();

	const inputId = $derived(id || (label ? label.toLowerCase().replace(/\s+/g, '-') : undefined));
</script>

<div class="flex flex-col gap-1.5 {className}">
	{#if label}
		<label for={inputId} class="text-[12px] font-medium text-text-secondary">{label}</label>
	{/if}
	<input
		id={inputId}
		bind:value
		class="h-9 w-full rounded-lg border bg-bg-elevated px-3 text-[13px] text-text-primary placeholder:text-text-muted transition-colors focus-visible:outline-none focus-visible:border-accent focus-visible:ring-2 focus-visible:ring-accent disabled:cursor-not-allowed disabled:opacity-50 {error
			? 'border-danger'
			: 'border-border'}"
		{...rest}
	/>
	{#if error}
		<p class="text-[12px] text-danger">{error}</p>
	{:else if hint}
		<p class="text-[12px] text-text-muted">{hint}</p>
	{/if}
</div>
