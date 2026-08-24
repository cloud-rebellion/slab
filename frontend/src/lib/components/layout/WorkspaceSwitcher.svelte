<script lang="ts">
	import { Popover } from 'bits-ui';
	import { goto } from '$app/navigation';
	import { ChevronsUpDown, Check, Plus, Search, Settings } from 'lucide-svelte';
	import type { Workspace } from '$lib/api/workspaces';
	import { currentWorkspace, setWorkspace } from '$lib/stores/currentWorkspace.svelte';
	import { clearSite } from '$lib/stores/currentSite.svelte';

	// Mirrors SiteSwitcher, with two deliberate differences.
	//
	// 1. No lazy load. The workspace list arrives with the layout loader, so
	//    there is nothing to fetch on open and no loading branch to render.
	// 2. Switching does NOT try to preserve the current URL. A site belongs to
	//    exactly one workspace, so every /sites/{id}/* path is meaningless
	//    after a switch: clear the active site and go to the list instead.
	let open = $state(false);
	let query = $state('');
	let listEl = $state<HTMLDivElement | null>(null);

	const workspaces = $derived(currentWorkspace.all);
	const activeId = $derived(currentWorkspace.value?.id ?? null);
	const activeName = $derived(currentWorkspace.value?.name ?? null);

	const filtered = $derived(
		query.trim() === ''
			? workspaces
			: workspaces.filter((w) => {
					const q = query.toLowerCase();
					return w.name.toLowerCase().includes(q) || w.slug.toLowerCase().includes(q);
				})
	);

	function selectWorkspace(workspace: Workspace) {
		open = false;
		query = '';
		if (workspace.id === activeId) return;
		setWorkspace(workspace);
		// The previously-active site lives in the workspace we just left.
		clearSite();
		void goto('/sites');
	}

	function manageWorkspaces() {
		open = false;
		query = '';
		void goto('/workspaces');
	}

	function handleOpenChange(next: boolean) {
		open = next;
		if (!next) query = '';
	}

	function handleListKeydown(e: KeyboardEvent) {
		if (!listEl) return;
		const buttons = Array.from(
			listEl.querySelectorAll<HTMLButtonElement>('button[data-workspace-item]')
		);
		if (buttons.length === 0) return;
		const idx = buttons.indexOf(document.activeElement as HTMLButtonElement);
		if (e.key === 'ArrowDown') {
			e.preventDefault();
			const next = buttons[(idx + 1) % buttons.length];
			next?.focus();
		} else if (e.key === 'ArrowUp') {
			e.preventDefault();
			const prev = buttons[(idx - 1 + buttons.length) % buttons.length];
			prev?.focus();
		}
	}
</script>

<!--
  Rendered even with a single workspace: the popover is still the only route
  to workspace settings, members and invites, so hiding it at n=1 would make
  the whole feature undiscoverable on exactly the installs that start there.
-->
<Popover.Root bind:open onOpenChange={handleOpenChange}>
	<Popover.Trigger
		class="inline-flex h-9 max-w-[200px] items-center gap-2 rounded-md border border-border bg-bg-elevated px-3 text-[13px] text-text-primary transition-colors hover:bg-bg-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
		aria-label="Switch workspace"
	>
		<span class="truncate">
			{activeName ?? 'No workspace'}
		</span>
		<ChevronsUpDown class="h-3.5 w-3.5 shrink-0 text-text-muted" />
	</Popover.Trigger>
	<Popover.Portal>
		<Popover.Content
			sideOffset={6}
			align="start"
			class="z-[110] w-72 rounded-xl border border-border bg-bg-surface p-2 shadow-popover focus:outline-none data-[state=open]:animate-fadeIn"
		>
			{#if workspaces.length === 0}
				<div class="px-3 py-6 text-center">
					<p class="text-[13px] text-text-secondary">You are not in a workspace yet.</p>
					<p class="mt-1 text-[11px] text-text-muted">
						An administrator creates one and invites you into it.
					</p>
				</div>
			{:else}
				{#if workspaces.length > 5}
					<div class="flex items-center gap-2 rounded-md border border-border bg-bg-elevated px-2.5">
						<Search class="h-3.5 w-3.5 text-text-muted" />
						<input
							type="text"
							placeholder="Search workspaces."
							bind:value={query}
							class="h-8 w-full bg-transparent text-[12px] text-text-primary placeholder:text-text-muted focus:outline-none"
						/>
					</div>
				{/if}

				<div
					bind:this={listEl}
					class="mt-2 max-h-64 overflow-y-auto"
					role="listbox"
					tabindex="-1"
					onkeydown={handleListKeydown}
				>
					{#if filtered.length === 0}
						<p class="px-3 py-4 text-center text-[12px] text-text-muted">No matches.</p>
					{:else}
						{#each filtered as workspace (workspace.id)}
							<button
								type="button"
								data-workspace-item
								class="flex w-full items-center justify-between gap-2 rounded-md px-2.5 py-2 text-left text-[13px] text-text-primary transition-colors hover:bg-bg-hover focus:bg-bg-hover focus:outline-none"
								onclick={() => selectWorkspace(workspace)}
							>
								<span class="flex min-w-0 flex-col">
									<span class="truncate font-medium">{workspace.name}</span>
									<span class="truncate text-[11px] text-text-muted">
										{workspace.slug}{workspace.role ? ` · ${workspace.role}` : ''}
									</span>
								</span>
								{#if workspace.id === activeId}
									<Check class="h-3.5 w-3.5 shrink-0 text-accent" />
								{/if}
							</button>
						{/each}
					{/if}
				</div>
			{/if}

			<div class="my-2 h-px bg-border-light"></div>
			<button
				type="button"
				class="flex w-full items-center gap-2 rounded-md px-2.5 py-2 text-[13px] text-text-secondary transition-colors hover:bg-bg-hover hover:text-text-primary focus:bg-bg-hover focus:outline-none"
				onclick={manageWorkspaces}
			>
				{#if workspaces.length === 0}
					<Plus class="h-3.5 w-3.5" />
					Workspaces
				{:else}
					<Settings class="h-3.5 w-3.5" />
					Manage workspaces
				{/if}
			</button>
		</Popover.Content>
	</Popover.Portal>
</Popover.Root>
