<script lang="ts">
	let {
		value,
		onChange,
		minHeight = 12
	}: {
		value: string;
		onChange: (next: string) => void;
		minHeight?: number;
	} = $props();

	function pretty(raw: string): string {
		const trimmed = (raw || '').trim();
		if (!trimmed) return '{}';
		try {
			return JSON.stringify(JSON.parse(trimmed), null, 2);
		} catch {
			return raw;
		}
	}

	let draft = $state('{}');
	let lastValue = $state('');
	let error = $state<string | null>(null);

	$effect(() => {
		if (value !== lastValue) {
			lastValue = value;
			draft = pretty(value);
			error = null;
		}
	});

	function commit(): void {
		const trimmed = draft.trim();
		if (!trimmed) {
			error = null;
			lastValue = '{}';
			onChange('{}');
			return;
		}
		try {
			const parsed = JSON.parse(trimmed);
			const minified = JSON.stringify(parsed);
			error = null;
			lastValue = minified;
			onChange(minified);
		} catch (err) {
			error = err instanceof Error ? err.message : 'Invalid JSON';
		}
	}

	function format(): void {
		const trimmed = draft.trim();
		if (!trimmed) return;
		try {
			draft = JSON.stringify(JSON.parse(trimmed), null, 2);
			error = null;
		} catch (err) {
			error = err instanceof Error ? err.message : 'Invalid JSON';
		}
	}
</script>

<div class="flex flex-col gap-1.5">
	<div class="flex items-center justify-between">
		<label for="json-editor-textarea" class="text-[12px] font-medium text-text-secondary">data_json</label>
		<button
			type="button"
			class="text-[11px] text-text-muted hover:text-text-primary"
			onclick={format}
		>
			Format
		</button>
	</div>
	<textarea
		id="json-editor-textarea"
		bind:value={draft}
		onblur={commit}
		spellcheck={false}
		style="min-height: {minHeight}rem; field-sizing: content;"
		class="w-full rounded-lg border bg-bg-elevated px-3 py-2 font-mono text-[12px] leading-relaxed text-text-primary transition-colors focus-visible:outline-none focus-visible:border-accent focus-visible:ring-2 focus-visible:ring-accent {error ? 'border-danger' : 'border-border'}"
	></textarea>
	{#if error}
		<p class="text-[12px] text-danger">{error}</p>
	{:else}
		<p class="text-[12px] text-text-muted">No schema registered for this block_type. Edit raw JSON; formats on blur.</p>
	{/if}
</div>
