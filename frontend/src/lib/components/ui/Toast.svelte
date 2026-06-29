<script lang="ts">
	import { toasts, dismiss, type ToastKind } from '$lib/stores/toast.svelte';

	const kindClasses: Record<ToastKind, string> = {
		success: 'border-success/30 bg-success/10 text-success',
		error: 'border-danger/30 bg-danger/10 text-danger',
		info: 'border-info/30 bg-info/10 text-info'
	};

	const icons: Record<ToastKind, string> = {
		success: 'M4.5 12.75l6 6 9-13.5',
		error: 'M6 18L18 6M6 6l12 12',
		info: 'M11.25 11.25l.041-.02a.75.75 0 011.063.852l-.708 2.836a.75.75 0 001.063.853l.041-.021M21 12a9 9 0 11-18 0 9 9 0 0118 0zm-9-3.75h.008v.008H12V8.25z'
	};
</script>

<div
	class="pointer-events-none fixed bottom-4 right-4 z-[200] flex w-full max-w-sm flex-col gap-2"
	aria-live="polite"
	aria-atomic="true"
>
	{#each toasts.value as t (t.id)}
		<div
			class="pointer-events-auto flex items-start gap-3 rounded-xl border bg-bg-surface px-4 py-3 shadow-popover animate-fadeIn {kindClasses[
				t.kind
			]}"
			role="status"
		>
			<svg
				class="h-4 w-4 mt-0.5 shrink-0"
				fill="none"
				viewBox="0 0 24 24"
				stroke-width="2"
				stroke="currentColor"
			>
				<path stroke-linecap="round" stroke-linejoin="round" d={icons[t.kind]} />
			</svg>
			<p class="flex-1 text-[13px] text-text-primary">{t.message}</p>
			<button
				type="button"
				class="text-text-muted hover:text-text-primary transition-colors"
				onclick={() => dismiss(t.id)}
				aria-label="Dismiss"
			>
				<svg
					class="h-3.5 w-3.5"
					fill="none"
					viewBox="0 0 24 24"
					stroke-width="2"
					stroke="currentColor"
				>
					<path stroke-linecap="round" stroke-linejoin="round" d="M6 18L18 6M6 6l12 12" />
				</svg>
			</button>
		</div>
	{/each}
</div>
