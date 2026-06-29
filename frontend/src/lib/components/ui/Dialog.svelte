<script lang="ts">
	import { Dialog as BitsDialog } from 'bits-ui';
	import type { Snippet } from 'svelte';

	let {
		open = $bindable(false),
		title,
		description,
		size = 'md',
		onOpenChange,
		children,
		footer
	}: {
		open?: boolean;
		title?: string;
		description?: string;
		size?: 'sm' | 'md' | 'lg' | 'xl' | 'full';
		onOpenChange?: (open: boolean) => void;
		children?: Snippet;
		footer?: Snippet;
	} = $props();

	const sizeClasses: Record<string, string> = {
		sm: 'max-w-md',
		md: 'max-w-lg',
		lg: 'max-w-2xl',
		xl: 'max-w-7xl',
		full: 'max-w-[calc(100vw-2rem)]'
	};
</script>

<!--
	Controlled mode for bits-ui Dialog.Root.

	Prior version did `bind:open {onOpenChange}` together. bits-ui v1 in
	Svelte 5 treats that as "fully controlled by parent" and waits for
	onOpenChange callbacks to drive the open state. When the parent only
	uses bind:open and never supplies onOpenChange, X / Esc / overlay
	clicks fired but the resulting close request had nowhere to land,
	so the dialog stayed open. Now we always supply an onOpenChange handler
	that writes back to our $bindable open prop and forwards to the
	parent's optional handler.
-->
<BitsDialog.Root
	{open}
	onOpenChange={(v) => {
		open = v;
		onOpenChange?.(v);
	}}
>
	<BitsDialog.Portal>
		<BitsDialog.Overlay
			class="fixed inset-0 z-50 bg-black/40 backdrop-blur-sm data-[state=open]:animate-fadeIn"
		/>
		<BitsDialog.Content
			class="fixed left-1/2 top-1/2 z-50 flex w-[calc(100%-2rem)] {sizeClasses[
				size
			]} max-h-[calc(100vh-2rem)] -translate-x-1/2 -translate-y-1/2 flex-col overflow-hidden rounded-2xl border border-border bg-bg-surface shadow-modal focus:outline-none data-[state=open]:animate-scaleIn"
		>
			{#if title || description}
				<div class="flex items-start justify-between gap-4 px-6 pt-6 pb-2">
					<div class="flex flex-col gap-1">
						{#if title}
							<BitsDialog.Title
								class="font-display text-xl font-extralight tracking-tight text-text-primary"
							>
								{title}
							</BitsDialog.Title>
						{/if}
						{#if description}
							<BitsDialog.Description class="text-[13px] text-text-secondary">
								{description}
							</BitsDialog.Description>
						{/if}
					</div>
					<BitsDialog.Close
						class="rounded-lg p-1 text-text-muted hover:bg-bg-hover hover:text-text-primary transition-colors"
						aria-label="Close"
					>
						<svg
							class="h-4 w-4"
							fill="none"
							viewBox="0 0 24 24"
							stroke-width="2"
							stroke="currentColor"
						>
							<path stroke-linecap="round" stroke-linejoin="round" d="M6 18L18 6M6 6l12 12" />
						</svg>
					</BitsDialog.Close>
				</div>
			{/if}

			{#if children}
				<div class="flex-1 overflow-y-auto px-6 py-4">
					{@render children()}
				</div>
			{/if}

			{#if footer}
				<div class="flex shrink-0 justify-end gap-2 border-t border-border-light px-6 py-4">
					{@render footer()}
				</div>
			{/if}
		</BitsDialog.Content>
	</BitsDialog.Portal>
</BitsDialog.Root>
