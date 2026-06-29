<script lang="ts">
	import type { HTMLTextareaAttributes } from 'svelte/elements';

	let {
		label,
		error,
		hint,
		id,
		rows = 3,
		value = $bindable(),
		class: className = '',
		...rest
	}: HTMLTextareaAttributes & {
		label?: string;
		error?: string;
		hint?: string;
		class?: string;
	} = $props();

	const textareaId = $derived(
		id || (label ? label.toLowerCase().replace(/\s+/g, '-') : undefined)
	);
</script>

<div class="flex flex-col gap-1.5 {className}">
	{#if label}
		<label for={textareaId} class="text-[12px] font-medium text-text-secondary">{label}</label>
	{/if}
	<textarea
		id={textareaId}
		{rows}
		bind:value
		style="field-sizing: content;"
		class="w-full rounded-lg border bg-bg-elevated px-3 py-2 text-[13px] text-text-primary placeholder:text-text-muted transition-colors resize-y focus-visible:outline-none focus-visible:border-accent focus-visible:ring-2 focus-visible:ring-accent disabled:cursor-not-allowed disabled:opacity-50 {error
			? 'border-danger'
			: 'border-border'}"
		{...rest}
	></textarea>
	{#if error}
		<p class="text-[12px] text-danger">{error}</p>
	{:else if hint}
		<p class="text-[12px] text-text-muted">{hint}</p>
	{/if}
</div>
