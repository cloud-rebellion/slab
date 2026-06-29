<script lang="ts">
	import { onMount } from 'svelte';
	import { Popover } from 'bits-ui';
	import {
		Plus,
		Sparkles,
		Type,
		Image as ImageIcon,
		MousePointerClick,
		LayoutGrid,
		BarChart3,
		Quote,
		HelpCircle,
		Tag,
		Code2,
		ListOrdered,
		Receipt,
		Repeat,
		ImagePlay,
		User,
		FormInput,
		Frame,
		FileCode2,
		Wand2,
		MessageSquareQuote,
		PlayCircle,
		PanelTop,
		ChevronsDown,
		Menu
	} from 'lucide-svelte';
	import * as blocksApi from '$lib/api/blocks';
	import type { BlockSchema } from '$lib/api/types';

	let {
		onPick,
		disabled = false
	}: {
		onPick: (type: string) => void;
		disabled?: boolean;
	} = $props();

	let open = $state(false);
	let schemas = $state<BlockSchema[]>([]);
	let loaded = $state(false);
	let error = $state<string | null>(null);

	onMount(() => {
		blocksApi
			.schemas()
			.then((all) => {
				schemas = all
					.filter((s) => s.type !== 'header' && s.type !== 'footer')
					.sort((a, b) => {
						if (a.category === b.category) return a.label.localeCompare(b.label);
						return a.category.localeCompare(b.category);
					});
				loaded = true;
			})
			.catch((err) => {
				error = err instanceof Error ? err.message : 'Failed to load block types';
				loaded = true;
			});
	});

	function pick(type: string) {
		open = false;
		onPick(type);
	}

	function iconFor(type: string) {
		switch (type) {
			case 'hero':
				return Sparkles;
			case 'split_hero':
				return Wand2;
			case 'text':
				return Type;
			case 'image':
				return ImageIcon;
			case 'cta':
				return MousePointerClick;
			case 'feature_grid':
				return LayoutGrid;
			case 'stat_grid':
				return BarChart3;
			case 'quote':
				return Quote;
			case 'accordion_faq':
				return HelpCircle;
			case 'pricing':
				return Tag;
			case 'code_block':
				return Code2;
			case 'process_steps':
				return ListOrdered;
			case 'replacement_grid':
				return Repeat;
			case 'video_hero':
				return PlayCircle;
			case 'tabs':
				return PanelTop;
			case 'scroll_reveal':
				return ChevronsDown;
			case 'mega_menu':
				return Menu;
			case 'testimonial_wall':
				return MessageSquareQuote;
			case 'logo_strip':
				return Receipt;
			case 'logo_carousel':
				return ImagePlay;
			case 'about_split':
				return User;
			case 'form':
				return FormInput;
			case 'embed':
				return Frame;
			case 'raw_astro':
			case 'custom':
				return FileCode2;
			default:
				return Plus;
		}
	}

	type Group = { name: string; items: BlockSchema[] };

	const grouped = $derived<Group[]>(
		schemas.reduce<Group[]>((acc, s) => {
			const cat = s.category || 'other';
			let g = acc.find((x) => x.name === cat);
			if (!g) {
				g = { name: cat, items: [] };
				acc.push(g);
			}
			g.items.push(s);
			return acc;
		}, [])
	);
</script>

<Popover.Root bind:open>
	<Popover.Trigger
		{disabled}
		class="inline-flex h-9 items-center justify-center gap-2 rounded-lg border border-dashed border-border bg-bg-elevated px-4 text-[13px] text-text-secondary transition-colors hover:bg-bg-hover hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-2 focus-visible:ring-offset-bg disabled:cursor-not-allowed disabled:opacity-50"
	>
		<Plus class="h-3.5 w-3.5" />
		Add block
	</Popover.Trigger>
	<Popover.Portal>
		<Popover.Content
			sideOffset={8}
			align="center"
			class="z-[110] max-h-[36rem] w-[34rem] overflow-y-auto rounded-xl border border-border bg-bg-surface p-3 shadow-popover focus:outline-none data-[state=open]:animate-fadeIn"
		>
			<p class="px-1 pb-2 text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">
				Choose block type
			</p>
			{#if !loaded}
				<p class="px-1 py-3 text-[12px] text-text-muted">Loading…</p>
			{:else if error}
				<p class="px-1 py-3 text-[12px] text-danger">{error}</p>
			{:else}
				{#each grouped as group (group.name)}
					<p class="mt-2 px-1 pb-1 text-[10.5px] font-mono uppercase tracking-[0.18em] text-text-muted">
						{group.name}
					</p>
					<div class="grid grid-cols-2 gap-2">
						{#each group.items as opt (opt.type)}
							{@const Icon = iconFor(opt.type)}
							<button
								type="button"
								aria-label={opt.label}
								class="flex items-start gap-3 rounded-lg border border-border-light bg-bg p-3 text-left transition-colors hover:bg-bg-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent"
								onclick={() => pick(opt.type)}
							>
								<span
									class="mt-0.5 inline-flex h-7 w-7 shrink-0 items-center justify-center rounded-md bg-bg-elevated text-text-secondary"
								>
									<Icon class="h-3.5 w-3.5" />
								</span>
								<span class="flex min-w-0 flex-col gap-0.5">
									<span class="text-[13px] font-medium text-text-primary">{opt.label}</span>
									<span class="text-[11px] leading-snug text-text-muted">{opt.description}</span>
								</span>
							</button>
						{/each}
					</div>
				{/each}
			{/if}
		</Popover.Content>
	</Popover.Portal>
</Popover.Root>
