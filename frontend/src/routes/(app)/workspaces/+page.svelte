<script lang="ts">
	import { ArrowRight, Plus } from 'lucide-svelte';
	import { goto } from '$app/navigation';
	import Badge from '$lib/components/ui/Badge.svelte';
	import Button from '$lib/components/ui/Button.svelte';
	import Card from '$lib/components/ui/Card.svelte';
	import Dialog from '$lib/components/ui/Dialog.svelte';
	import EmptyState from '$lib/components/ui/EmptyState.svelte';
	import Input from '$lib/components/ui/Input.svelte';
	import Select from '$lib/components/ui/Select.svelte';
	import Spinner from '$lib/components/ui/Spinner.svelte';
	import { ApiError } from '$lib/api/client';
	import * as workspacesApi from '$lib/api/workspaces';
	import type { Workspace } from '$lib/api/workspaces';
	import { auth } from '$lib/stores/auth.svelte';
	import { deploymentMode } from '$lib/stores/mode.svelte';
	import {
		currentWorkspace,
		setWorkspace,
		upsertWorkspace
	} from '$lib/stores/currentWorkspace.svelte';
	import { clearSite } from '$lib/stores/currentSite.svelte';
	import { toast } from '$lib/stores/toast.svelte';

	let loading = $state(true);
	let workspaces = $state<Workspace[]>([]);

	let createOpen = $state(false);
	let newName = $state('');
	let newPlan = $state('oss');
	let creating = $state(false);

	const me = $derived(auth.value);
	// Workspace creation is an operator action on a self-hosted install: the
	// server refuses it from a non-admin unless the instance runs in cloud
	// mode, so the button follows the same rule rather than offering a click
	// that 403s.
	const isPlatformAdmin = $derived(me?.role === 'admin');
	const canCreate = $derived(isPlatformAdmin || deploymentMode.isCloud);

	// Only a platform admin may name a plan. Everyone else gets the server
	// default, so they are never shown the choice.
	const planOptions = [
		{ value: 'oss', label: 'Self-hosted (no limits)' },
		{ value: 'solo', label: 'Solo (1 site)' },
		{ value: 'studio', label: 'Studio (5 sites)' },
		{ value: 'agency', label: 'Agency (25 sites)' }
	];

	async function load() {
		loading = true;
		try {
			const res = await workspacesApi.list();
			workspaces = res.workspaces ?? [];
		} catch (err) {
			toast.error(err instanceof ApiError ? err.message : 'Failed to load workspaces.');
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		void load();
	});

	function openCreate() {
		newName = '';
		newPlan = 'oss';
		createOpen = true;
	}

	async function createWorkspace(e?: SubmitEvent) {
		e?.preventDefault();
		if (creating) return;
		creating = true;
		try {
			const ws = await workspacesApi.create({
				name: newName.trim(),
				...(isPlatformAdmin ? { plan: newPlan } : {})
			});
			workspaces = [...workspaces, ws];
			upsertWorkspace(ws);
			createOpen = false;
			toast.success(`Workspace "${ws.name}" created.`);
			void goto(`/workspaces/${ws.id}`);
		} catch (err) {
			toast.error(err instanceof ApiError ? err.message : 'Failed to create workspace.');
		} finally {
			creating = false;
		}
	}

	function openWorkspace(ws: Workspace) {
		setWorkspace(ws);
		clearSite();
		void goto(`/workspaces/${ws.id}`);
	}
</script>

<div class="mx-auto max-w-5xl px-6 py-8">
	<div class="flex items-start justify-between gap-4">
		<div>
			<p class="eyebrow">agency</p>
			<h1 class="font-display text-3xl font-extralight tracking-tight text-text-primary">
				Workspaces
			</h1>
			<p class="mt-1 text-[13px] text-text-secondary">
				A workspace owns a set of sites and the people who can work on them. One per client keeps
				each build isolated from the others.
			</p>
		</div>
		{#if canCreate}
			<Button variant="primary" onclick={openCreate}>
				{#snippet icon()}<Plus size={14} strokeWidth={1.75} />{/snippet}
				New workspace
			</Button>
		{/if}
	</div>

	{#if loading}
		<div class="mt-12 flex items-center justify-center"><Spinner /></div>
	{:else if workspaces.length === 0}
		<div class="mt-8">
			<EmptyState
				title="No workspaces yet"
				description={canCreate
					? 'Create one per client or per builder, then invite people into it.'
					: 'An administrator creates a workspace and invites you into it.'}
			>
				{#snippet action()}
					{#if canCreate}
						<Button variant="primary" onclick={openCreate}>
							{#snippet icon()}<Plus size={14} strokeWidth={1.75} />{/snippet}
							New workspace
						</Button>
					{/if}
				{/snippet}
			</EmptyState>
		</div>
	{:else}
		<div class="mt-8 grid grid-cols-1 gap-4 md:grid-cols-2">
			{#each workspaces as ws (ws.id)}
				<Card padding="none" hoverable>
					<button
						type="button"
						class="flex w-full items-center justify-between gap-4 p-5 text-left"
						onclick={() => openWorkspace(ws)}
					>
						<span class="flex min-w-0 flex-col gap-1">
							<span class="flex items-center gap-2">
								<span class="truncate text-[15px] font-medium text-text-primary">{ws.name}</span>
								{#if ws.id === currentWorkspace.value?.id}
									<Badge variant="accent">active</Badge>
								{/if}
							</span>
							<span class="truncate font-mono text-[11px] text-text-muted">{ws.slug}</span>
							<span class="mt-1 flex items-center gap-2">
								{#if ws.role}
									<Badge variant={ws.role === 'owner' ? 'accent' : 'default'}>{ws.role}</Badge>
								{/if}
								{#if deploymentMode.isCloud}
									<Badge variant="info">{ws.plan}</Badge>
								{/if}
								{#if ws.status !== 'active'}
									<Badge variant="warning">{ws.status}</Badge>
								{/if}
							</span>
						</span>
						<ArrowRight class="h-4 w-4 shrink-0 text-text-muted" />
					</button>
				</Card>
			{/each}
		</div>
	{/if}
</div>

<Dialog bind:open={createOpen} title="New workspace" size="md">
	<form onsubmit={createWorkspace} class="flex flex-col gap-4">
		<Input
			label="Name"
			required
			bind:value={newName}
			placeholder="Acme Ltd"
			hint="Shown in the workspace switcher. The URL slug is derived from it."
		/>
		{#if isPlatformAdmin && deploymentMode.isCloud}
			<div class="flex flex-col gap-1.5">
				<span class="text-[12px] font-medium text-text-secondary">Plan</span>
				<Select options={planOptions} bind:value={newPlan} />
			</div>
		{/if}
	</form>
	{#snippet footer()}
		<Button variant="secondary" onclick={() => (createOpen = false)}>Cancel</Button>
		<Button
			variant="primary"
			loading={creating}
			disabled={newName.trim() === ''}
			onclick={() => createWorkspace()}
		>
			Create workspace
		</Button>
	{/snippet}
</Dialog>
