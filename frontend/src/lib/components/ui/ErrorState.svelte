<script lang="ts">
	import { AlertTriangle } from 'lucide-svelte';
	import type { Snippet } from 'svelte';
	import Button from './Button.svelte';

	let {
		message,
		title = 'Something went wrong',
		onRetry,
		retryLabel = 'Try again',
		class: className = '',
		children
	}: {
		message?: string;
		title?: string;
		onRetry?: () => void;
		retryLabel?: string;
		class?: string;
		children?: Snippet;
	} = $props();
</script>

<!--
	Composed error state: a danger-tinted surface, a clear message, and an
	optional retry. Use this instead of a bare <p class="text-danger"> so failure
	paths read as deliberate UI, not leftover debug text. role="alert" so the
	error is announced to assistive tech.
-->
<div
	role="alert"
	class="flex flex-col items-center justify-center rounded-xl border border-danger/20 bg-danger/5 px-6 py-12 text-center {className}"
>
	<div
		class="mb-4 flex h-14 w-14 items-center justify-center rounded-full bg-danger/10 text-danger"
	>
		<AlertTriangle class="h-6 w-6" strokeWidth={1.75} />
	</div>
	<h3 class="font-display text-lg font-extralight tracking-tight text-text-primary">{title}</h3>
	{#if message}
		<p class="mt-1 max-w-sm text-[13px] text-text-secondary">{message}</p>
	{/if}
	{#if children}
		<div class="mt-1 max-w-sm text-[13px] text-text-secondary">{@render children()}</div>
	{/if}
	{#if onRetry}
		<div class="mt-5">
			<Button variant="secondary" size="sm" onclick={onRetry}>{retryLabel}</Button>
		</div>
	{/if}
</div>
