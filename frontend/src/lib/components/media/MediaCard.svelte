<script lang="ts">
	import * as mediaApi from '$lib/api/media';
	import type { Medium } from '$lib/api/types';

	let {
		media,
		siteID,
		showFolder = false,
		onOpen,
		onSelect,
		onEdit,
		onMove,
		onCopyUrl,
		onDelete
	}: {
		media: Medium;
		siteID: string;
		showFolder?: boolean;
		onOpen?: (media: Medium) => void;
		onSelect?: (media: Medium) => void;
		onEdit?: (media: Medium) => void;
		onMove?: (media: Medium) => void;
		onCopyUrl?: (media: Medium) => void;
		onDelete?: (media: Medium) => void;
	} = $props();

	let loaded = $state(false);

	const variants = $derived(mediaApi.parseMediaVariants(media.variants_json));

	const previewSrc = $derived.by(() => {
		const has320Webp = variants.some(
			(v) => v.name === '320' && v.format === 'webp'
		);
		if (has320Webp) {
			return mediaApi.mediaUrl(siteID, media.id, '320.webp');
		}
		const hasOriginal = variants.some((v) => v.name === 'original');
		return mediaApi.mediaUrl(
			siteID,
			media.id,
			hasOriginal ? 'original' : '320.webp'
		);
	});

	function activate() {
		if (onSelect) {
			onSelect(media);
		} else {
			onOpen?.(media);
		}
	}

	function onKey(e: KeyboardEvent) {
		if (e.key === 'Enter' || e.key === ' ') {
			e.preventDefault();
			activate();
		}
	}
</script>

<div class="group relative">
	<button
		type="button"
		onclick={activate}
		onkeydown={onKey}
		class="relative block w-full aspect-square overflow-hidden rounded-xl border border-border-light bg-bg-elevated transition-all duration-200 hover:-translate-y-0.5 hover:border-border focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
		aria-label={media.alt_text || media.filename}
	>
		<img
			src={previewSrc}
			alt={media.alt_text || media.filename}
			loading="lazy"
			class="absolute inset-0 h-full w-full object-cover transition-opacity duration-300 {loaded
				? 'opacity-100'
				: 'opacity-0'}"
			onload={() => (loaded = true)}
			onerror={() => (loaded = true)}
		/>
	</button>

	{#if showFolder && media.folder}
		<span
			class="pointer-events-none absolute left-1.5 top-1.5 inline-flex items-center gap-1 rounded-md bg-bg-surface/90 px-1.5 py-0.5 text-[10px] font-medium text-text-secondary backdrop-blur"
		>
			<svg class="h-3 w-3" fill="none" viewBox="0 0 24 24" stroke-width="1.75" stroke="currentColor">
				<path stroke-linecap="round" stroke-linejoin="round" d="M2.25 12.75V12A2.25 2.25 0 014.5 9.75h15A2.25 2.25 0 0121.75 12v.75m-8.69-6.44l-2.12-2.12a1.5 1.5 0 00-1.061-.44H4.5A2.25 2.25 0 002.25 6v12a2.25 2.25 0 002.25 2.25h15A2.25 2.25 0 0021.75 18V9a2.25 2.25 0 00-2.25-2.25h-5.379a1.5 1.5 0 01-1.06-.44z" />
			</svg>
			{media.folder}
		</span>
	{/if}

	{#if onEdit || onMove || onCopyUrl || onDelete}
		<div
			class="row-actions pointer-events-none absolute right-1.5 top-1.5 flex items-center gap-1"
		>
			{#if onMove}
				<button
					type="button"
					onclick={(e) => {
						e.stopPropagation();
						onMove?.(media);
					}}
					class="pointer-events-auto inline-flex h-7 w-7 items-center justify-center rounded-md border border-border bg-bg-surface/90 text-text-secondary backdrop-blur transition-colors hover:bg-bg-hover hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1 focus-visible:ring-offset-bg"
					aria-label="Move to folder"
				>
					<svg class="h-3.5 w-3.5" fill="none" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor">
						<path stroke-linecap="round" stroke-linejoin="round" d="M2.25 12.75V12A2.25 2.25 0 014.5 9.75h15A2.25 2.25 0 0121.75 12v.75m-8.69-6.44l-2.12-2.12a1.5 1.5 0 00-1.061-.44H4.5A2.25 2.25 0 002.25 6v12a2.25 2.25 0 002.25 2.25h15A2.25 2.25 0 0021.75 18V9a2.25 2.25 0 00-2.25-2.25h-5.379a1.5 1.5 0 01-1.06-.44z" />
					</svg>
				</button>
			{/if}
			{#if onEdit}
				<button
					type="button"
					onclick={(e) => {
						e.stopPropagation();
						onEdit?.(media);
					}}
					class="pointer-events-auto inline-flex h-7 w-7 items-center justify-center rounded-md border border-border bg-bg-surface/90 text-text-secondary backdrop-blur transition-colors hover:bg-bg-hover hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1 focus-visible:ring-offset-bg"
					aria-label="Edit alt text"
				>
					<svg
						class="h-3.5 w-3.5"
						fill="none"
						viewBox="0 0 24 24"
						stroke-width="2"
						stroke="currentColor"
					>
						<path
							stroke-linecap="round"
							stroke-linejoin="round"
							d="M16.862 4.487l1.687-1.688a1.875 1.875 0 112.652 2.652L10.582 16.07a4.5 4.5 0 01-1.897 1.13L6 18l.8-2.685a4.5 4.5 0 011.13-1.897l8.932-8.931z"
						/>
					</svg>
				</button>
			{/if}
			{#if onCopyUrl}
				<button
					type="button"
					onclick={(e) => {
						e.stopPropagation();
						onCopyUrl?.(media);
					}}
					class="pointer-events-auto inline-flex h-7 w-7 items-center justify-center rounded-md border border-border bg-bg-surface/90 text-text-secondary backdrop-blur transition-colors hover:bg-bg-hover hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1 focus-visible:ring-offset-bg"
					aria-label="Copy URL"
				>
					<svg
						class="h-3.5 w-3.5"
						fill="none"
						viewBox="0 0 24 24"
						stroke-width="2"
						stroke="currentColor"
					>
						<path
							stroke-linecap="round"
							stroke-linejoin="round"
							d="M13.19 8.688a4.5 4.5 0 011.242 7.244l-4.5 4.5a4.5 4.5 0 01-6.364-6.364l1.757-1.757m13.35-.622l1.757-1.757a4.5 4.5 0 00-6.364-6.364l-4.5 4.5a4.5 4.5 0 001.242 7.244"
						/>
					</svg>
				</button>
			{/if}
			{#if onDelete}
				<button
					type="button"
					onclick={(e) => {
						e.stopPropagation();
						onDelete?.(media);
					}}
					class="pointer-events-auto inline-flex h-7 w-7 items-center justify-center rounded-md border border-danger/40 bg-bg-surface/90 text-danger backdrop-blur transition-colors hover:bg-danger/10 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-danger focus-visible:ring-offset-1 focus-visible:ring-offset-bg"
					aria-label="Delete"
				>
					<svg
						class="h-3.5 w-3.5"
						fill="none"
						viewBox="0 0 24 24"
						stroke-width="2"
						stroke="currentColor"
					>
						<path
							stroke-linecap="round"
							stroke-linejoin="round"
							d="M14.74 9l-.346 9m-4.788 0L9.26 9m9.968-3.21c.342.052.682.107 1.022.166m-1.022-.165L18.16 19.673a2.25 2.25 0 01-2.244 2.077H8.084a2.25 2.25 0 01-2.244-2.077L4.772 5.79m14.456 0a48.108 48.108 0 00-3.478-.397m-12 .562c.34-.059.68-.114 1.022-.165m0 0a48.11 48.11 0 013.478-.397m7.5 0v-.916c0-1.18-.91-2.164-2.09-2.201a51.964 51.964 0 00-3.32 0c-1.18.037-2.09 1.022-2.09 2.201v.916m7.5 0a48.667 48.667 0 00-7.5 0"
						/>
					</svg>
				</button>
			{/if}
		</div>
	{/if}

	<p class="mt-1.5 truncate text-[12px] text-text-secondary" title={media.alt_text || media.filename}>
		{media.alt_text || media.filename}
	</p>
</div>
