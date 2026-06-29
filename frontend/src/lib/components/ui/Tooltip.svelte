<script lang="ts">
	import { Tooltip as BitsTooltip } from 'bits-ui';
	import type { Snippet } from 'svelte';

	let {
		content,
		side = 'top',
		delayDuration = 250,
		sideOffset = 6,
		disabled = false,
		trigger
	}: {
		content: string;
		side?: 'top' | 'bottom' | 'left' | 'right';
		delayDuration?: number;
		sideOffset?: number;
		disabled?: boolean;
		trigger: Snippet<[{ props: Record<string, unknown> }]>;
	} = $props();
</script>

<BitsTooltip.Provider {delayDuration}>
	<BitsTooltip.Root {disabled}>
		<BitsTooltip.Trigger>
			{#snippet child({ props })}
				{@render trigger({ props })}
			{/snippet}
		</BitsTooltip.Trigger>
		<BitsTooltip.Portal>
			<BitsTooltip.Content
				{side}
				{sideOffset}
				class="z-[120] rounded-md border border-border bg-bg-elevated px-2 py-1 text-[11px] font-medium text-text-primary shadow-popover select-none pointer-events-none animate-fadeIn"
			>
				{content}
			</BitsTooltip.Content>
		</BitsTooltip.Portal>
	</BitsTooltip.Root>
</BitsTooltip.Provider>
