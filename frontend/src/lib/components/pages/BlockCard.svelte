<script lang="ts">
	import { untrack } from 'svelte';
	import { GripVertical, ChevronDown, Trash2, Code, Copy, Check, Eye } from 'lucide-svelte';
	import Switch from '$lib/components/ui/Switch.svelte';
	import Input from '$lib/components/ui/Input.svelte';
	import BlockTypeForm from './BlockTypeForm.svelte';
	import BlockPreviewDialog from './BlockPreviewDialog.svelte';
	import { confirm } from '$lib/components/ui/ConfirmDialog.svelte';
	import * as blocksApi from '$lib/api/blocks';
	import { toast } from '$lib/stores/toast.svelte';
	import type { Block } from '$lib/api/types';

	let {
		block,
		siteID,
		pageID,
		expanded = false,
		dragging = false,
		onToggleExpanded,
		onToggleVisibility,
		onDataChange,
		onNameChange,
		onTypeChange,
		onDelete,
		ondragstart,
		ondragover,
		ondragend,
		ondrop
	}: {
		block: Block;
		siteID: string;
		pageID: string;
		expanded?: boolean;
		dragging?: boolean;
		onToggleExpanded: () => void;
		onToggleVisibility: (visible: boolean) => void;
		onDataChange: (nextDataJson: string) => void;
		onNameChange: (nextName: string) => void;
		onTypeChange?: (nextType: string, nextDataJson: string) => void;
		onDelete: () => void;
		ondragstart?: (e: DragEvent) => void;
		ondragover?: (e: DragEvent) => void;
		ondragend?: (e: DragEvent) => void;
		ondrop?: (e: DragEvent) => void;
	} = $props();

	// Code panel state. Lazy: only fetched the first time the </> toggle
	// opens or after a save (which dirties the cached source).
	let codeOpen = $state(false);
	let codeLoading = $state(false);
	let codeError = $state<string | null>(null);
	let codeAstro = $state<string | null>(null);
	let codeBlockType = $state<string>('');
	let codeDraft = $state<string>('');
	let codeDirty = $state(false);
	let copied = $state(false);
	let previewOpen = $state(false);

	// New blocks have no id yet; the source/preview endpoints both 404 in
	// that case. Gate both buttons until the page is saved once.
	const hasID = $derived(Boolean(block.id));

	async function loadCode(): Promise<void> {
		if (!hasID) {
			codeError = 'Save the page once before viewing source. New blocks need an ID.';
			return;
		}
		codeLoading = true;
		codeError = null;
		try {
			const res = await blocksApi.preview(siteID, pageID, block.id);
			codeAstro = res.astro;
			codeBlockType = res.block_type;
			codeDraft = res.astro;
			codeDirty = false;
		} catch (err) {
			codeError = err instanceof Error ? err.message : 'Failed to render block';
		} finally {
			codeLoading = false;
		}
	}

	function toggleCode(): void {
		if (!hasID && !codeOpen) {
			toast.info('Save the page once first, then the source becomes available.');
			return;
		}
		codeOpen = !codeOpen;
		if (codeOpen && codeAstro === null) {
			void loadCode();
		}
	}

	async function refreshCode(): Promise<void> {
		// Don't blow away the user's in-progress edit on a background refresh.
		if (codeDirty) return;
		codeAstro = null;
		await loadCode();
	}

	async function copyCode(): Promise<void> {
		const text = codeDraft || codeAstro;
		if (!text) return;
		try {
			await navigator.clipboard.writeText(text);
			copied = true;
			toast.success('Source copied');
			setTimeout(() => (copied = false), 1500);
		} catch {
			toast.error('Could not copy');
		}
	}

	function resetCode(): void {
		if (codeAstro === null) return;
		codeDraft = codeAstro;
		codeDirty = false;
	}

	async function saveCode(): Promise<void> {
		if (!codeDirty) return;
		const edited = codeDraft;
		const isRawAstro = block.block_type === 'raw_astro';
		if (isRawAstro) {
			// Same block type, just patch the data.code field.
			let prev: Record<string, unknown> = {};
			try {
				const v = JSON.parse(block.data_json || '{}');
				if (v && typeof v === 'object' && !Array.isArray(v))
					prev = v as Record<string, unknown>;
			} catch {
				/* ignore */
			}
			const next = { ...prev, code: edited };
			onDataChange(JSON.stringify(next));
			codeAstro = edited;
			codeDirty = false;
			toast.success('Code updated. Click "Save changes" to persist.');
			return;
		}
		// Typed block. Edit converts the block to raw_astro and drops the
		// typed fields. Confirm before doing the lossy step.
		const ok = await confirm({
			title: 'Switch to raw code?',
			message:
				'Editing the source replaces the typed fields with raw Astro markup. Form fields for this block will be lost on save. Continue?',
			confirmText: 'Convert',
			cancelText: 'Keep fields',
			variant: 'danger'
		});
		if (!ok) return;
		const nextData = JSON.stringify({ code: edited });
		onTypeChange?.('raw_astro', nextData);
		codeBlockType = 'raw_astro';
		codeAstro = edited;
		codeDirty = false;
		toast.success('Converted to raw_astro. Click "Save changes" to persist.');
	}

	// Re-fetch when the underlying data changes after the panel was opened
	// at least once. Otherwise the code would lag behind a fresh edit.
	// CRITICAL: Svelte 5 $effect tracks every reactive read in its body.
	// Reading codeAstro/codeOpen/codeLoading inside the effect would make
	// it self-trigger when those state vars change inside refreshCode
	// (codeAstro flips null -> string -> re-runs effect -> re-fetches -> loop).
	// Only block.data_json should be reactive here; the rest read via
	// untrack so the effect only fires on actual data changes.
	$effect(() => {
		void block.data_json;
		void block.block_type;
		untrack(() => {
			if (codeOpen && codeAstro !== null && !codeLoading && !codeDirty) {
				void refreshCode();
			}
		});
	});

	let visibleBound = $state(false);
	let visibleInit = $state(false);

	$effect(() => {
		const next = block.is_visible === 1;
		if (!visibleInit) {
			visibleInit = true;
			visibleBound = next;
			return;
		}
		if (next !== visibleBound) {
			visibleBound = next;
		}
	});

	$effect(() => {
		const expected = block.is_visible === 1;
		if (visibleInit && visibleBound !== expected) {
			onToggleVisibility(visibleBound);
		}
	});

	// Derive the auto label from block.data_json. Used in two places: as the
	// collapsed-header summary fallback when block.name is empty, and as the
	// placeholder of the Block name input so the user understands what the
	// header is showing when they leave the field blank.
	function autoLabel(): string {
		try {
			const parsed = JSON.parse(block.data_json || '{}') as Record<string, unknown>;
			const candidates = [
				'headline',
				'heading',
				'title',
				'label',
				'name',
				'eyebrow',
				'text',
				'content',
				'alt',
				'quote'
			];
			for (const key of candidates) {
				const v = parsed[key];
				if (typeof v === 'string' && v.trim().length > 0) {
					// Collapse \n and other whitespace runs into single spaces.
					// Headlines often contain "\n" for visual line breaks (the
					// renderer turns them into <br>), but in a one-line label
					// they need to render as proper word separators or you get
					// "Stop rentingyour business.Own it."
					const clean = v
						.replace(/\[\[|\]\]/g, '')
						.replace(/\s+/g, ' ')
						.trim();
					return clean.length > 60 ? `${clean.slice(0, 60)}.` : clean;
				}
			}
			for (const key of ['items', 'tiers', 'paragraphs']) {
				const arr = parsed[key];
				if (Array.isArray(arr) && arr.length > 0) {
					return `${arr.length} ${key}`;
				}
			}
			if (block.block_type === 'raw_astro' && typeof parsed.code === 'string') {
				const firstLine = parsed.code.split('\n').find((l) => l.trim().length > 0) || '';
				if (firstLine) {
					const clean = firstLine.trim().slice(0, 60);
					return clean.length === 60 ? `${clean}.` : clean;
				}
			}
		} catch {
			// ignore
		}
		return '';
	}

	function summary(): string {
		if (block.name && block.name.trim().length > 0) {
			const clean = block.name.trim();
			return clean.length > 60 ? `${clean.slice(0, 60)}.` : clean;
		}
		const auto = autoLabel();
		return auto || 'No content yet.';
	}

	const summaryText = $derived(summary());
	const autoLabelText = $derived(autoLabel());
	const namePlaceholder = $derived(autoLabelText || 'e.g. Hero, homepage');

	function handleCodeInput(e: Event) {
		const value = (e.currentTarget as HTMLTextAreaElement).value;
		codeDraft = value;
		codeDirty = codeAstro !== null && value !== codeAstro;
	}
</script>

<div
	role="listitem"
	class="rounded-xl border bg-bg-surface transition-colors {dragging
		? 'border-accent ring-1 ring-accent opacity-60'
		: expanded
			? 'border-border'
			: 'border-border/50 hover:bg-bg-elevated/40'}"
	{ondragover}
	{ondrop}
	{ondragend}
>
	<div class="flex items-center gap-2 px-3 py-2.5">
		<button
			type="button"
			class="cursor-grab text-text-muted transition-colors hover:text-text-primary active:cursor-grabbing"
			aria-label="Drag to reorder"
			draggable={!expanded}
			{ondragstart}
		>
			<GripVertical class="h-4 w-4" />
		</button>
		<button
			type="button"
			class="flex flex-1 items-center gap-3 text-left focus-visible:outline-none"
			onclick={onToggleExpanded}
		>
			<span
				class="inline-flex h-5 items-center rounded-md bg-bg-elevated px-1.5 font-mono text-[10px] uppercase tracking-wider text-text-secondary"
			>
				{block.block_type}
			</span>
			<span
				class="truncate text-[13px] {block.name && block.name.trim().length > 0
					? 'text-text-secondary'
					: 'text-text-muted'}"
				title={block.name && block.name.trim().length > 0
					? 'Block name'
					: 'Auto-derived from block content. Set a Block name in the expanded view to override.'}
			>
				{summaryText}
			</span>
		</button>
		<div class="flex items-center gap-2">
			<Switch bind:checked={visibleBound} />
			<button
				type="button"
				aria-pressed={previewOpen}
				aria-label={hasID ? 'Open live preview' : 'Save the page once before previewing'}
				title={hasID ? 'Live preview' : 'Save the page once first'}
				class="inline-flex h-7 w-7 items-center justify-center rounded-md transition-colors text-text-muted hover:bg-bg-hover hover:text-text-primary disabled:opacity-40 disabled:cursor-not-allowed focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1 focus-visible:ring-offset-bg"
				disabled={!hasID}
				onclick={() => (previewOpen = true)}
			>
				<Eye class="h-3.5 w-3.5" />
			</button>
			<button
				type="button"
				aria-pressed={codeOpen}
				aria-label={codeOpen ? 'Hide source' : hasID ? 'Show source' : 'Save the page once before viewing source'}
				title={codeOpen ? 'Hide source' : hasID ? 'Show source' : 'Save the page once first'}
				class="inline-flex h-7 w-7 items-center justify-center rounded-md transition-colors {codeOpen
					? 'bg-bg-elevated text-text-primary'
					: 'text-text-muted hover:bg-bg-hover hover:text-text-primary'} disabled:opacity-40 disabled:cursor-not-allowed focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1 focus-visible:ring-offset-bg"
				disabled={!hasID}
				onclick={toggleCode}
			>
				<Code class="h-3.5 w-3.5" />
			</button>
			<button
				type="button"
				class="inline-flex h-7 items-center gap-1 rounded-md px-2 text-[12px] text-text-muted transition-colors hover:bg-bg-hover hover:text-danger focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-danger focus-visible:ring-offset-1 focus-visible:ring-offset-bg"
				onclick={onDelete}
			>
				<Trash2 class="h-3.5 w-3.5" />
				Delete
			</button>
			<button
				type="button"
				class="icon-btn"
				aria-label={expanded ? 'Collapse' : 'Expand'}
				onclick={onToggleExpanded}
			>
				<ChevronDown
					class="h-4 w-4 transition-transform {expanded ? 'rotate-180' : ''}"
				/>
			</button>
		</div>
	</div>
	{#if expanded}
		<div class="border-t border-border-light px-4 py-4">
			<div class="mb-4">
				<Input
					label="Block name"
					hint={autoLabelText
						? `Optional. Defaults to "${autoLabelText}" in the card header.`
						: 'Optional. Used as the card header label.'}
					placeholder={namePlaceholder}
					value={block.name || ''}
					oninput={(e) => onNameChange((e.currentTarget as HTMLInputElement).value)}
				/>
			</div>
			<BlockTypeForm
				blockType={block.block_type}
				dataJson={block.data_json || '{}'}
				onChange={onDataChange}
			/>
		</div>
	{/if}
	{#if codeOpen}
		<div class="border-t border-border-light bg-bg-elevated/40">
			<div class="flex items-center justify-between gap-2 px-4 py-2">
				<div class="flex items-center gap-2">
					<span class="text-[10.5px] font-mono uppercase tracking-[0.18em] text-text-muted">
						Source {block.block_type === 'raw_astro' ? '· editable' : '· typed (editing converts to raw_astro)'}
					</span>
					{#if codeBlockType}
						<span class="font-mono text-[10.5px] text-text-secondary">{codeBlockType}.astro</span>
					{/if}
					{#if codeDirty}
						<span class="text-[10.5px] text-accent">unsaved</span>
					{/if}
				</div>
				<div class="flex items-center gap-1">
					<button
						type="button"
						class="text-[11px] text-text-muted hover:text-text-primary disabled:opacity-50"
						disabled={!codeDirty}
						onclick={resetCode}
						title="Discard code edits"
					>
						Reset
					</button>
					<button
						type="button"
						class="text-[11px] font-medium text-text-primary hover:text-accent disabled:opacity-50"
						disabled={!codeDirty || codeLoading}
						onclick={saveCode}
						title="Apply code edit to this block (still need to click Save changes)"
					>
						Save
					</button>
					<button
						type="button"
						aria-label="Copy source"
						title="Copy"
						class="inline-flex h-6 w-6 items-center justify-center rounded text-text-muted hover:bg-bg-hover hover:text-text-primary disabled:opacity-50"
						disabled={!codeAstro}
						onclick={copyCode}
					>
						{#if copied}
							<Check class="h-3 w-3 text-accent" />
						{:else}
							<Copy class="h-3 w-3" />
						{/if}
					</button>
				</div>
			</div>
			{#if codeLoading}
				<p class="px-4 pb-3 text-[12px] text-text-muted">Loading…</p>
			{:else if codeError}
				<p class="px-4 pb-3 text-[12px] text-danger">{codeError}</p>
			{:else if codeAstro !== null}
				<textarea
					rows={Math.min(40, Math.max(12, codeDraft.split('\n').length + 2))}
					class="block w-full resize-y border-0 bg-transparent px-4 pb-3 font-mono text-[11.5px] leading-relaxed text-text-primary outline-none focus:bg-bg-surface/40"
					value={codeDraft}
					oninput={handleCodeInput}
					spellcheck="false"
				></textarea>
			{/if}
		</div>
	{/if}
</div>

<BlockPreviewDialog
	bind:open={previewOpen}
	{siteID}
	{pageID}
	blockID={block.id}
	blockType={block.block_type}
	dataJson={block.data_json || '{}'}
/>
