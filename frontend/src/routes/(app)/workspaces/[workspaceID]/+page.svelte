<script lang="ts">
	import { untrack } from 'svelte';
	import { ArrowLeft, Copy, Trash2, UserPlus } from 'lucide-svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import Badge from '$lib/components/ui/Badge.svelte';
	import Button from '$lib/components/ui/Button.svelte';
	import Card from '$lib/components/ui/Card.svelte';
	import Dialog from '$lib/components/ui/Dialog.svelte';
	import ErrorState from '$lib/components/ui/ErrorState.svelte';
	import Input from '$lib/components/ui/Input.svelte';
	import Select from '$lib/components/ui/Select.svelte';
	import Spinner from '$lib/components/ui/Spinner.svelte';
	import { ApiError } from '$lib/api/client';
	import * as workspacesApi from '$lib/api/workspaces';
	import type {
		Workspace,
		WorkspaceInvite,
		WorkspaceMember,
		WorkspaceRole
	} from '$lib/api/workspaces';
	import { auth } from '$lib/stores/auth.svelte';
	import { confirm } from '$lib/stores/confirm.svelte';
	import { deploymentMode } from '$lib/stores/mode.svelte';
	import { removeWorkspaceFromList, upsertWorkspace } from '$lib/stores/currentWorkspace.svelte';
	import { toast } from '$lib/stores/toast.svelte';

	const workspaceID = $derived(page.params.workspaceID ?? '');

	let loading = $state(true);
	let loadError = $state<string | null>(null);
	let workspace = $state<Workspace | null>(null);
	let members = $state<WorkspaceMember[]>([]);
	let invites = $state<WorkspaceInvite[]>([]);

	let inviteOpen = $state(false);
	let inviteEmail = $state('');
	let inviteRole = $state<WorkspaceRole>('member');
	let creating = $state(false);
	let lastInviteUrl = $state<string | null>(null);

	let name = $state('');
	let billingEmail = $state('');
	let saving = $state(false);

	const me = $derived(auth.value);
	// Gate on the WORKSPACE role, not the platform role: a platform editor can
	// be a workspace owner, and a platform admin can be looking at a workspace
	// they are not a member of.
	const myRole = $derived(workspace?.role ?? null);
	const isOwner = $derived(myRole === 'owner');
	const isWsAdmin = $derived(myRole === 'owner' || myRole === 'admin');

	const roleOptions = [
		{ value: 'member', label: 'Member (work on this workspace’s sites)' },
		{ value: 'admin', label: 'Admin (can also invite and remove people)' },
		{ value: 'owner', label: 'Owner (full control, including billing)' }
	];

	// An admin cannot mint an owner invite: the server refuses it so that
	// escalation stays monotone, so the option is not offered either.
	const availableInviteRoles = $derived(
		isOwner ? roleOptions : roleOptions.filter((r) => r.value !== 'owner')
	);

	async function load(id: string) {
		if (!id) return;
		loading = true;
		loadError = null;
		try {
			const ws = await workspacesApi.get(id);
			workspace = ws;
			name = ws.name;
			billingEmail = ws.billing_email;
			upsertWorkspace(ws);

			const wsAdmin = ws.role === 'owner' || ws.role === 'admin';
			const [m, i] = await Promise.all([
				workspacesApi.listMembers(id),
				wsAdmin
					? workspacesApi.listInvites(id)
					: Promise.resolve({ invites: [] as WorkspaceInvite[] })
			]);
			members = m.members ?? [];
			invites = i.invites ?? [];
		} catch (err) {
			if (err instanceof ApiError && (err.status === 403 || err.status === 404)) {
				// A bookmarked link to a workspace you were removed from should
				// land somewhere useful, not on an error page.
				toast.error('That workspace is not available to you.');
				void goto('/workspaces');
				return;
			}
			loadError = err instanceof ApiError ? err.message : 'Failed to load workspace.';
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		const id = workspaceID;
		untrack(() => void load(id));
	});

	function openInvite() {
		inviteEmail = '';
		inviteRole = 'member';
		lastInviteUrl = null;
		inviteOpen = true;
	}

	async function createInvite(e?: SubmitEvent) {
		e?.preventDefault();
		if (creating) return;
		creating = true;
		try {
			const inv = await workspacesApi.createInvite(workspaceID, inviteEmail.trim(), inviteRole);
			lastInviteUrl = inv.url;
			// Re-read the pending list rather than grafting the create response
			// into it: the two shapes differ (no expires_at on create), so the
			// optimistic row would render undefined fields.
			const refreshed = await workspacesApi.listInvites(workspaceID);
			invites = refreshed.invites ?? [];
			toast.success(`Invite created for ${inv.email}.`);
		} catch (err) {
			toast.error(err instanceof ApiError ? err.message : 'Failed to create invite.');
		} finally {
			creating = false;
		}
	}

	async function copyUrl(url: string) {
		try {
			await navigator.clipboard.writeText(url);
			toast.success('Invite link copied.');
		} catch {
			toast.error('Could not copy. Select the text and copy manually.');
		}
	}

	async function revokeInvite(inv: WorkspaceInvite) {
		const ok = await confirm({
			title: 'Revoke invite',
			message: `The link sent to ${inv.email} will stop working.`,
			confirmText: 'Revoke',
			variant: 'danger'
		});
		if (!ok) return;
		try {
			await workspacesApi.deleteInvite(workspaceID, inv.id);
			invites = invites.filter((i) => i.id !== inv.id);
			toast.success('Invite revoked.');
		} catch (err) {
			toast.error(err instanceof ApiError ? err.message : 'Failed to revoke invite.');
		}
	}

	async function changeRole(m: WorkspaceMember, role: WorkspaceRole) {
		try {
			await workspacesApi.updateMemberRole(workspaceID, m.user_id, role);
			members = members.map((x) => (x.user_id === m.user_id ? { ...x, role } : x));
			toast.success(`${m.email} is now ${role}.`);
		} catch (err) {
			toast.error(err instanceof ApiError ? err.message : 'Failed to change role.');
			// The server refused, so put the row back the way it was.
			members = [...members];
		}
	}

	async function removeMember(m: WorkspaceMember) {
		const ok = await confirm({
			title: 'Remove member',
			message: `${m.email} will lose access to every site in this workspace.`,
			confirmText: 'Remove',
			variant: 'danger'
		});
		if (!ok) return;
		try {
			await workspacesApi.removeMember(workspaceID, m.user_id);
			members = members.filter((x) => x.user_id !== m.user_id);
			toast.success('Member removed.');
		} catch (err) {
			toast.error(err instanceof ApiError ? err.message : 'Failed to remove member.');
		}
	}

	async function saveSettings(e?: SubmitEvent) {
		e?.preventDefault();
		if (saving) return;
		saving = true;
		try {
			const ws = await workspacesApi.update(workspaceID, {
				name: name.trim(),
				billing_email: billingEmail.trim()
			});
			workspace = ws;
			upsertWorkspace(ws);
			toast.success('Workspace saved.');
		} catch (err) {
			toast.error(err instanceof ApiError ? err.message : 'Failed to save workspace.');
		} finally {
			saving = false;
		}
	}

	async function deleteWorkspace() {
		const ok = await confirm({
			title: 'Delete workspace',
			message:
				'Everyone loses access to it and it disappears from the switcher. Its sites are not deleted.',
			confirmText: 'Delete',
			variant: 'danger'
		});
		if (!ok) return;
		try {
			await workspacesApi.remove(workspaceID);
			removeWorkspaceFromList(workspaceID);
			toast.success('Workspace deleted.');
			void goto('/workspaces');
		} catch (err) {
			toast.error(err instanceof ApiError ? err.message : 'Failed to delete workspace.');
		}
	}
</script>

<div class="mx-auto max-w-5xl px-6 py-8">
	<a
		href="/workspaces"
		class="inline-flex items-center gap-1.5 text-[12px] text-text-muted transition-colors hover:text-text-secondary"
	>
		<ArrowLeft size={14} strokeWidth={1.75} />
		All workspaces
	</a>

	{#if loading}
		<div class="mt-12 flex items-center justify-center"><Spinner /></div>
	{:else if loadError}
		<div class="mt-8">
			<ErrorState message={loadError} onRetry={() => void load(workspaceID)} />
		</div>
	{:else if workspace}
		<div class="mt-4 flex items-start justify-between gap-4">
			<div>
				<p class="eyebrow">workspace</p>
				<h1 class="font-display text-3xl font-extralight tracking-tight text-text-primary">
					{workspace.name}
				</h1>
				<p class="mt-1 flex items-center gap-2 text-[13px] text-text-secondary">
					<span class="font-mono text-[11px]">{workspace.slug}</span>
					{#if myRole}<Badge variant={isOwner ? 'accent' : 'default'}>{myRole}</Badge>{/if}
					{#if workspace.status !== 'active'}
						<Badge variant="warning">{workspace.status}</Badge>
					{/if}
				</p>
			</div>
			{#if isWsAdmin}
				<Button variant="primary" onclick={openInvite}>
					{#snippet icon()}<UserPlus size={14} strokeWidth={1.75} />{/snippet}
					Invite to workspace
				</Button>
			{/if}
		</div>

		<!-- Members -->
		<section class="mt-10">
			<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">Members</h2>
			<Card padding="none" class="mt-2">
				<div class="divide-y divide-border-light">
					{#each members as m (m.user_id)}
						{@const isSelf = m.user_id === me?.id}
						<div class="flex items-center gap-4 px-4 py-3">
							<div class="min-w-0 flex-1">
								<p class="truncate text-[13px] text-text-primary">
									{m.name || m.email}
									{#if isSelf}<span class="text-text-muted"> (you)</span>{/if}
								</p>
								<p class="truncate text-[11px] text-text-muted">{m.email}</p>
							</div>
							{#if isOwner && !isSelf}
								<div class="w-40">
									<Select
										options={roleOptions.map((r) => ({ value: r.value, label: r.value }))}
										value={m.role}
										onchange={(v: string) => changeRole(m, v as WorkspaceRole)}
									/>
								</div>
								<button
									type="button"
									class="inline-flex h-8 w-8 items-center justify-center rounded-md text-text-muted transition-colors hover:bg-danger/10 hover:text-danger"
									aria-label="Remove member"
									onclick={() => removeMember(m)}
								>
									<Trash2 size={14} strokeWidth={1.75} />
								</button>
							{:else}
								<Badge variant={m.role === 'owner' ? 'accent' : 'default'}>{m.role}</Badge>
							{/if}
						</div>
					{/each}
				</div>
			</Card>
		</section>

		<!-- Pending invites -->
		{#if isWsAdmin}
			<section class="mt-10">
				<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">
					Pending invites
				</h2>
				{#if invites.length === 0}
					<Card padding="md" class="mt-2">
						<p class="text-[13px] text-text-secondary">No pending invites.</p>
					</Card>
				{:else}
					<Card padding="none" class="mt-2">
						<div class="divide-y divide-border-light">
							{#each invites as inv (inv.id)}
								<div class="flex items-center gap-4 px-4 py-3">
									<div class="min-w-0 flex-1">
										<p class="truncate text-[13px] text-text-primary">{inv.email}</p>
										<p class="truncate text-[11px] text-text-muted">
											expires {inv.expires_at?.slice(0, 10) ?? 'soon'}
										</p>
									</div>
									<Badge variant={inv.role === 'owner' ? 'accent' : 'default'}>{inv.role}</Badge>
									<button
										type="button"
										class="inline-flex h-8 w-8 items-center justify-center rounded-md text-text-muted transition-colors hover:bg-danger/10 hover:text-danger"
										aria-label="Revoke invite"
										onclick={() => revokeInvite(inv)}
									>
										<Trash2 size={14} strokeWidth={1.75} />
									</button>
								</div>
							{/each}
						</div>
					</Card>
					<p class="mt-2 text-[11px] text-text-muted">
						The invite link is shown once, when it is created. If it was lost, revoke the invite and
						issue a new one.
					</p>
				{/if}
			</section>
		{/if}

		<!-- Settings -->
		{#if isOwner}
			<section class="mt-10">
				<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">Settings</h2>
				<Card padding="md" class="mt-2">
					<form onsubmit={saveSettings} class="flex flex-col gap-4">
						<Input label="Name" required bind:value={name} />
						<Input
							label="Billing email"
							type="email"
							bind:value={billingEmail}
							hint="Where invoices and plan notices go."
						/>
						<div>
							<Button variant="primary" loading={saving} onclick={() => saveSettings()}>
								Save changes
							</Button>
						</div>
					</form>
				</Card>
			</section>

			{#if deploymentMode.billingAvailable}
				<section class="mt-10">
					<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">Plan</h2>
					<Card padding="md" class="mt-2">
						<p class="text-[13px] text-text-primary">
							Current plan: <span class="font-medium">{workspace.plan}</span>
						</p>
						{#if workspace.trial_ends_at}
							<p class="mt-1 text-[11px] text-text-muted">
								Trial ends {workspace.trial_ends_at.slice(0, 10)}
							</p>
						{/if}
					</Card>
				</section>
			{/if}

			<section class="mt-10">
				<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">Danger zone</h2>
				<Card padding="md" class="mt-2">
					<div class="flex items-center justify-between gap-4">
						<div>
							<p class="text-[13px] text-text-primary">Delete this workspace</p>
							<p class="mt-1 text-[11px] text-text-muted">
								Members lose access immediately. The sites it owns are left in place.
							</p>
						</div>
						<Button variant="danger" onclick={deleteWorkspace}>Delete</Button>
					</div>
				</Card>
			</section>
		{/if}
	{/if}
</div>

<Dialog bind:open={inviteOpen} title="Invite to this workspace" size="md">
	<form onsubmit={createInvite} class="flex flex-col gap-4">
		<Input
			label="Email"
			type="email"
			required
			bind:value={inviteEmail}
			hint="They get a link that creates their account and adds them to this workspace."
		/>
		<div class="flex flex-col gap-1.5">
			<span class="text-[12px] font-medium text-text-secondary">Role</span>
			<Select options={availableInviteRoles} bind:value={inviteRole} />
		</div>
		{#if lastInviteUrl}
			<div class="rounded-lg border border-border-light bg-bg-elevated p-3">
				<p class="text-[12px] font-medium text-text-primary">Send them this link</p>
				<p class="mt-1.5 break-all font-mono text-[11px] text-text-secondary">{lastInviteUrl}</p>
				<div class="mt-2 flex items-center justify-between gap-2">
					<p class="text-[11px] text-text-muted">
						Shown once, and valid for 7 days. Treat it like a password.
					</p>
					<Button variant="secondary" size="sm" onclick={() => copyUrl(lastInviteUrl ?? '')}>
						{#snippet icon()}<Copy size={13} strokeWidth={1.75} />{/snippet}
						Copy
					</Button>
				</div>
			</div>
		{/if}
	</form>
	{#snippet footer()}
		{#if lastInviteUrl}
			<Button variant="primary" onclick={() => (inviteOpen = false)}>Done</Button>
		{:else}
			<Button variant="secondary" onclick={() => (inviteOpen = false)}>Cancel</Button>
			<Button
				variant="primary"
				loading={creating}
				disabled={inviteEmail.trim() === ''}
				onclick={() => createInvite()}
			>
				Create invite
			</Button>
		{/if}
	{/snippet}
</Dialog>
