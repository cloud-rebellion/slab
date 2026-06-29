<script lang="ts">
	import { Tabs as BitsTabs } from 'bits-ui';
	import type { Snippet } from 'svelte';

	let {
		tabs,
		value = $bindable(),
		class: className = '',
		children
	}: {
		tabs: { id: string; label: string; icon?: Snippet }[];
		value?: string;
		class?: string;
		children?: Snippet;
	} = $props();
</script>

<BitsTabs.Root bind:value class={className}>
	<BitsTabs.List
		class="flex items-center gap-1 border-b border-border-light"
	>
		{#each tabs as tab (tab.id)}
			<BitsTabs.Trigger
				value={tab.id}
				class="relative -mb-px inline-flex items-center gap-2 rounded-md px-3 py-2 text-[13px] text-text-muted transition-colors hover:text-text-secondary data-[state=active]:text-text-primary data-[state=active]:border-b-2 data-[state=active]:border-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
			>
				{#if tab.icon}
					{@render tab.icon()}
				{/if}
				{tab.label}
			</BitsTabs.Trigger>
		{/each}
	</BitsTabs.List>
	{#if children}
		{@render children()}
	{/if}
</BitsTabs.Root>
