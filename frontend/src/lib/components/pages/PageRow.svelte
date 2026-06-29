<script lang="ts">
	import { GripVertical, Pencil, Trash2, FolderTree, History } from 'lucide-svelte';
	import Badge from '$lib/components/ui/Badge.svelte';
	import type { Page } from '$lib/api/types';

	let {
		page,
		dragging = false,
		depth = 0,
		isSiloHub = false,
		siloType = '',
		onOpen,
		onDelete,
		onMove,
		onHistory,
		ondragstart,
		ondragover,
		ondragend,
		ondrop
	}: {
		page: Page;
		dragging?: boolean;
		depth?: number;
		isSiloHub?: boolean;
		siloType?: string;
		onOpen: () => void;
		onDelete: () => void;
		onMove?: () => void;
		onHistory?: () => void;
		ondragstart?: (e: DragEvent) => void;
		ondragover?: (e: DragEvent) => void;
		ondragend?: (e: DragEvent) => void;
		ondrop?: (e: DragEvent) => void;
	} = $props();

	function statusVariant(status: string): 'success' | 'warning' | 'default' {
		if (status === 'published') return 'success';
		if (status === 'draft') return 'warning';
		return 'default';
	}

	function relativeTime(ts: string): string {
		if (!ts) return '';
		const d = new Date(ts);
		if (Number.isNaN(d.getTime())) return '';
		const diff = Date.now() - d.getTime();
		const sec = Math.round(diff / 1000);
		if (sec < 60) return 'just now';
		const min = Math.round(sec / 60);
		if (min < 60) return `${min}m ago`;
		const hr = Math.round(min / 60);
		if (hr < 24) return `${hr}h ago`;
		const day = Math.round(hr / 24);
		if (day < 30) return `${day}d ago`;
		return d.toLocaleDateString();
	}
</script>

<div
	role="listitem"
	draggable="true"
	{ondragstart}
	{ondragover}
	{ondragend}
	{ondrop}
	style:margin-left="{depth * 1.5}rem"
	class="group relative flex items-center gap-3 rounded-lg border border-border-light bg-bg-surface px-3 py-2.5 transition-colors hover:bg-bg-elevated/40 {dragging
		? 'ring-1 ring-accent opacity-60'
		: ''} {depth > 0
		? 'border-l-2 border-l-accent/30 pl-4'
		: ''}"
>
	<span
		class="cursor-grab text-text-muted opacity-0 transition-opacity group-hover:opacity-100 active:cursor-grabbing"
		aria-label="Drag handle"
	>
		<GripVertical class="h-4 w-4" />
	</span>

	<button
		type="button"
		class="flex flex-1 min-w-0 items-baseline gap-3 text-left focus-visible:outline-none focus-visible:text-text-primary"
		onclick={onOpen}
	>
		<span class="truncate text-[14px] font-medium text-text-primary group-hover:text-accent">
			{page.title || 'Untitled'}
		</span>
		<span class="truncate font-mono text-[11px] text-text-muted">/{page.slug}</span>
	</button>

	<div class="flex items-center gap-3 text-[11px] text-text-muted">
		{#if isSiloHub}
			<Badge variant="default" dot>silo hub</Badge>
			{#if siloType && siloType !== 'inherit'}
				<span
					class="rounded-full border border-border bg-bg-elevated px-2 py-0.5 text-[10px] uppercase tracking-wide text-text-muted"
					title={siloType === 'hard' ? 'No cross-silo links' : 'Cross-silo links allowed'}
				>
					{siloType}
				</span>
			{/if}
		{/if}
		<Badge variant={statusVariant(page.status)} dot>
			{page.status || 'draft'}
		</Badge>
		{#if page.no_index === 1}
			<span class="inline-flex items-center gap-1" title="Page is set to noindex">
				<span class="h-1.5 w-1.5 rounded-full bg-warning"></span>
				<span>noindex</span>
			</span>
		{/if}
		<span>{relativeTime(page.updated_at)}</span>
	</div>

	<div
		class="row-actions flex items-center gap-1 opacity-0 transition-opacity group-hover:opacity-100 focus-within:opacity-100"
	>
		{#if onMove}
			<button
				type="button"
				class="icon-btn"
				aria-label="Move page"
				title="Move to another silo or top-level"
				onclick={onMove}
			>
				<FolderTree class="h-3.5 w-3.5" />
			</button>
		{/if}
		<button
			type="button"
			class="inline-flex h-7 w-7 items-center justify-center rounded-md text-text-muted transition-colors hover:bg-bg-hover hover:text-text-primary"
			aria-label="Edit page"
			onclick={onOpen}
		>
			<Pencil class="h-3.5 w-3.5" />
		</button>
		{#if onHistory}
			<button
				type="button"
				class="icon-btn"
				aria-label="Version history"
				title="Version history"
				onclick={onHistory}
			>
				<History class="h-3.5 w-3.5" />
			</button>
		{/if}
		<button
			type="button"
			class="inline-flex h-7 w-7 items-center justify-center rounded-md text-text-muted transition-colors hover:bg-bg-hover hover:text-danger focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1 focus-visible:ring-offset-bg"
			aria-label="Delete page"
			onclick={onDelete}
		>
			<Trash2 class="h-3.5 w-3.5" />
		</button>
	</div>
</div>
