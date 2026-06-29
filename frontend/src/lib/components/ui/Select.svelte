<script lang="ts">
	import { Select as BitsSelect } from 'bits-ui';

	let {
		options,
		value = $bindable(),
		placeholder = 'Select an option',
		disabled = false,
		label,
		required = false,
		onchange,
		class: className = ''
	}: {
		options: { value: string; label: string; disabled?: boolean }[];
		value?: string;
		placeholder?: string;
		disabled?: boolean;
		label?: string;
		required?: boolean;
		onchange?: (value: string) => void;
		class?: string;
	} = $props();

	const selectedLabel = $derived(options.find((o) => o.value === value)?.label ?? '');
</script>

<div class="flex flex-col gap-1.5">
	{#if label}
		<span class="text-[12px] font-medium text-text-secondary">
			{label}{required ? ' *' : ''}
		</span>
	{/if}
	<BitsSelect.Root
	type="single"
	bind:value
	items={options}
	{disabled}
	onValueChange={(v: string) => {
		value = v;
		onchange?.(v);
	}}
>
	<BitsSelect.Trigger
		class="h-9 w-full inline-flex items-center justify-between rounded-lg border border-border bg-bg-elevated px-3 text-[13px] text-text-primary transition-colors focus-visible:outline-none focus-visible:border-accent focus-visible:ring-2 focus-visible:ring-accent disabled:cursor-not-allowed disabled:opacity-50 {className}"
	>
		<span class={selectedLabel ? '' : 'text-text-muted'}>
			{selectedLabel || placeholder}
		</span>
		<svg
			class="h-4 w-4 text-text-muted shrink-0"
			fill="none"
			viewBox="0 0 24 24"
			stroke-width="2"
			stroke="currentColor"
		>
			<path stroke-linecap="round" stroke-linejoin="round" d="M19.5 8.25l-7.5 7.5-7.5-7.5" />
		</svg>
	</BitsSelect.Trigger>
	<BitsSelect.Portal>
		<BitsSelect.Content
			sideOffset={6}
			class="z-[100] w-[var(--bits-select-anchor-width)] min-w-[8rem] rounded-xl border border-border bg-bg-surface shadow-popover py-1.5 animate-fadeIn focus:outline-none"
		>
			{#each options as opt (opt.value)}
				<BitsSelect.Item
					value={opt.value}
					label={opt.label}
					disabled={opt.disabled}
					class="flex items-center justify-between px-3 py-1.5 text-[13px] text-text-secondary cursor-pointer hover:bg-bg-hover hover:text-text-primary data-[highlighted]:bg-bg-hover data-[highlighted]:text-text-primary data-[disabled]:opacity-50 data-[disabled]:cursor-not-allowed"
				>
					{#snippet children({ selected })}
						<span>{opt.label}</span>
						{#if selected}
							<svg
								class="h-3.5 w-3.5 text-accent"
								fill="none"
								viewBox="0 0 24 24"
								stroke-width="2.5"
								stroke="currentColor"
							>
								<path stroke-linecap="round" stroke-linejoin="round" d="M4.5 12.75l6 6 9-13.5" />
							</svg>
						{/if}
					{/snippet}
				</BitsSelect.Item>
			{/each}
			</BitsSelect.Content>
		</BitsSelect.Portal>
	</BitsSelect.Root>
</div>
