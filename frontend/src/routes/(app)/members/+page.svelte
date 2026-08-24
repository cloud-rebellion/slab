<script lang="ts">
	import { ArrowLeft, Copy, Trash2, UserPlus } from 'lucide-svelte';
	import Badge from '$lib/components/ui/Badge.svelte';
	import Button from '$lib/components/ui/Button.svelte';
	import Card from '$lib/components/ui/Card.svelte';
	import Dialog from '$lib/components/ui/Dialog.svelte';
	import Input from '$lib/components/ui/Input.svelte';
	import Select from '$lib/components/ui/Select.svelte';
	import Spinner from '$lib/components/ui/Spinner.svelte';
	import { ApiError } from '$lib/api/client';
	import * as membersApi from '$lib/api/members';
	import type { Invite, Member } from '$lib/api/members';
	import { confirm } from '$lib/stores/confirm.svelte';
	import { auth } from '$lib/stores/auth.svelte';
	import { toast } from '$lib/stores/toast.svelte';

	let members = $state<Member[]>([]);
	let invites = $state<Invite[]>([]);
	let loading = $state(true);

	let inviteOpen = $state(false);
	let inviteEmail = $state('');
	let inviteRole = $state<'admin' | 'editor'>('editor');
	let creating = $state(false);
	let lastInviteUrl = $state<string | null>(null);

	const me = $derived(auth.value);
	const isAdmin = $derived(me?.role === 'admin');

	const roleOptions = [
		{ value: 'editor', label: 'Editor (access via workspace membership)' },
		{ value: 'admin', label: 'Admin (full access to every workspace)' }
	];

	async function load() {
		loading = true;
		try {
			const [m, i] = await Promise.all([
				membersApi.listMembers(),
				isAdmin ? membersApi.listInvites() : Promise.resolve([] as Invite[])
			]);
			members = m;
			invites = i;
		} catch (err) {
			toast.error(err instanceof ApiError ? err.message : 'Failed to load members.');
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		void load();
	});

	function openInvite() {
		inviteEmail = '';
		inviteRole = 'editor';
		lastInviteUrl = null;
		inviteOpen = true;
	}

	async function createInvite(e?: SubmitEvent) {
		e?.preventDefault();
		if (creating) return;
		creating = true;
		try {
			const inv = await membersApi.createInvite(inviteEmail.trim(), inviteRole);
			invites = [inv, ...invites];
			lastInviteUrl = inv.url;
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
			toast.success('Invite URL copied.');
		} catch {
			toast.error('Could not copy. Select and copy manually.');
		}
	}

	async function revokeInvite(inv: Invite) {
		const ok = await confirm({
			title: 'Revoke invite?',
			message: `The signup link for ${inv.email} will stop working immediately.`,
			confirmText: 'Revoke',
			variant: 'danger'
		});
		if (!ok) return;
		try {
			await membersApi.deleteInvite(inv.id);
			invites = invites.filter((i) => i.id !== inv.id);
			toast.success('Invite revoked.');
		} catch (err) {
			toast.error(err instanceof ApiError ? err.message : 'Failed to revoke invite.');
		}
	}

	async function removeMember(m: Member) {
		const ok = await confirm({
			title: `Remove ${m.email}?`,
			message: 'They lose access immediately. Their existing sessions are revoked.',
			confirmText: 'Remove',
			variant: 'danger'
		});
		if (!ok) return;
		try {
			await membersApi.deleteMember(m.id);
			members = members.filter((x) => x.id !== m.id);
			toast.success('Member removed.');
		} catch (err) {
			toast.error(err instanceof ApiError ? err.message : 'Failed to remove member.');
		}
	}

	async function changeRole(m: Member, newRole: 'admin' | 'editor') {
		try {
			await membersApi.updateMemberRole(m.id, newRole);
			members = members.map((x) => (x.id === m.id ? { ...x, role: newRole } : x));
			toast.success(`${m.email} is now ${newRole}.`);
		} catch (err) {
			toast.error(err instanceof ApiError ? err.message : 'Failed to update role.');
		}
	}
</script>

<div class="mx-auto max-w-5xl px-6 py-8">
	<a
		href="/account"
		class="inline-flex items-center gap-1.5 text-[12px] text-text-muted transition-colors hover:text-text-primary"
	>
		<ArrowLeft size={14} strokeWidth={1.75} />
		Account settings
	</a>
	<header class="mt-3 flex items-start justify-between gap-4">
		<div>
			<h1 class="font-display text-3xl font-extralight tracking-tight text-text-primary">
				Platform users
			</h1>
			<p class="mt-1 text-[13px] text-text-secondary">
				Everyone with an account on this instance. To give someone access to a set of sites, invite
				them into a <a href="/workspaces" class="text-accent hover:underline">workspace</a> instead:
				an account created here alone belongs to no workspace and sees nothing.
			</p>
		</div>
		{#if isAdmin}
			<Button variant="primary" onclick={openInvite}>
				{#snippet icon()}<UserPlus size={14} strokeWidth={1.75} />{/snippet}
				Invite teammate
			</Button>
		{/if}
	</header>

	{#if loading}
		<div class="mt-12 flex items-center justify-center"><Spinner /></div>
	{:else}
		<section class="mt-8">
			<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">Members</h2>
			<Card padding="none" class="mt-2">
				<div class="divide-y divide-border-light">
					{#each members as m (m.id)}
						<div class="flex items-center gap-4 px-4 py-3">
							<div class="min-w-0 flex-1">
								<p class="truncate text-[13px] font-medium text-text-primary">
									{m.name || m.email}
									{#if m.is_self}
										<span class="ml-2 text-[11px] font-normal text-text-muted">you</span>
									{/if}
								</p>
								{#if m.name}
									<p class="truncate text-[12px] text-text-muted">{m.email}</p>
								{/if}
							</div>
							{#if isAdmin && !m.is_self}
								<div class="w-44">
									<Select
										options={[
											{ value: 'editor', label: 'Editor' },
											{ value: 'admin', label: 'Admin' }
										]}
										value={m.role}
										onchange={(v: string) => changeRole(m, v as 'admin' | 'editor')}
									/>
								</div>
								<button
									type="button"
									aria-label="Remove member"
									class="inline-flex h-8 w-8 items-center justify-center rounded-md text-text-muted transition-colors hover:bg-danger/10 hover:text-danger"
									onclick={() => removeMember(m)}
								>
									<Trash2 size={14} strokeWidth={1.75} />
								</button>
							{:else}
								<Badge variant={m.role === 'admin' ? 'accent' : 'default'}>{m.role}</Badge>
							{/if}
						</div>
					{/each}
				</div>
			</Card>
		</section>

		{#if isAdmin}
			<section class="mt-8">
				<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">Pending invites</h2>
				{#if invites.length === 0}
					<Card padding="md" class="mt-2">
						<p class="text-[13px] text-text-secondary">No pending invites.</p>
					</Card>
				{:else}
					<Card padding="none" class="mt-2">
						<div class="divide-y divide-border-light">
							{#each invites as inv (inv.id)}
								<div class="flex items-center gap-3 px-4 py-3">
									<div class="min-w-0 flex-1">
										<p class="truncate text-[13px] font-medium text-text-primary">{inv.email}</p>
										<p class="truncate font-mono text-[11px] text-text-muted">{inv.url}</p>
									</div>
									<Badge variant={inv.role === 'admin' ? 'accent' : 'default'}>{inv.role}</Badge>
									<button
										type="button"
										aria-label="Copy invite URL"
										class="inline-flex h-8 w-8 items-center justify-center rounded-md text-text-muted transition-colors hover:bg-bg-hover hover:text-text-primary"
										onclick={() => copyUrl(inv.url)}
									>
										<Copy size={14} strokeWidth={1.75} />
									</button>
									<button
										type="button"
										aria-label="Revoke invite"
										class="inline-flex h-8 w-8 items-center justify-center rounded-md text-text-muted transition-colors hover:bg-danger/10 hover:text-danger"
										onclick={() => revokeInvite(inv)}
									>
										<Trash2 size={14} strokeWidth={1.75} />
									</button>
								</div>
							{/each}
						</div>
					</Card>
				{/if}
			</section>
		{/if}
	{/if}
</div>

<Dialog bind:open={inviteOpen} title="Invite a teammate" size="md">
	<form onsubmit={createInvite} class="flex flex-col gap-4">
		<Input
			label="Email"
			type="email"
			required
			bind:value={inviteEmail}
			placeholder="teammate@example.com"
			hint="Used to identify the invite. No email is sent - you'll copy the link to share."
		/>
		<div class="flex flex-col gap-1.5">
			<span class="text-[12px] font-medium text-text-secondary">Role</span>
			<Select options={roleOptions} bind:value={inviteRole} />
		</div>
		{#if lastInviteUrl}
			<div class="rounded-lg border border-border-light bg-bg-elevated p-3">
				<p class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">
					One-time signup URL
				</p>
				<p class="mt-1 break-all font-mono text-[12px] text-text-primary">{lastInviteUrl}</p>
				<p class="mt-2 text-[11px] text-text-muted">
					Expires in 7 days. Single-use. Anyone with this link can claim the account.
				</p>
				<div class="mt-2 flex justify-end">
					<Button variant="secondary" onclick={() => copyUrl(lastInviteUrl!)}>Copy URL</Button>
				</div>
			</div>
		{/if}
	</form>
	{#snippet footer()}
		<Button variant="secondary" onclick={() => (inviteOpen = false)}>
			{lastInviteUrl ? 'Done' : 'Cancel'}
		</Button>
		{#if !lastInviteUrl}
			<Button
				variant="primary"
				loading={creating}
				onclick={(e) => {
					e.preventDefault();
					void createInvite();
				}}
			>
				Create invite
			</Button>
		{/if}
	{/snippet}
</Dialog>
