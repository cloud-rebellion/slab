<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/stores';
	import Button from '$lib/components/ui/Button.svelte';
	import Input from '$lib/components/ui/Input.svelte';
	import Spinner from '$lib/components/ui/Spinner.svelte';
	import { ApiError } from '$lib/api/client';
	import * as authApi from '$lib/api/auth';
	import type { InviteInfo } from '$lib/api/auth';
	import { setUser } from '$lib/stores/auth.svelte';

	const token: string = $derived($page.params.token ?? '');

	let info = $state<InviteInfo | null>(null);
	let loadError = $state('');
	let loading = $state(true);

	let name = $state('');
	let password = $state('');
	let confirm = $state('');
	let submitError = $state('');
	let submitting = $state(false);

	$effect(() => {
		if (!token) {
			loadError = 'Missing invite token.';
			loading = false;
			info = null;
			return;
		}
		void load(token);
	});

	async function load(t: string) {
		loading = true;
		loadError = '';
		try {
			info = await authApi.inviteInfo(t);
		} catch (err) {
			loadError =
				err instanceof ApiError ? err.message : 'This invite link could not be loaded.';
			info = null;
		} finally {
			loading = false;
		}
	}

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		if (submitting) return;
		submitError = '';
		if (!token) {
			submitError = 'Missing invite token.';
			return;
		}
		if (password.length < 12) {
			submitError = 'Password must be at least 12 characters.';
			return;
		}
		if (password !== confirm) {
			submitError = 'Passwords do not match.';
			return;
		}
		submitting = true;
		try {
			const res = await authApi.redeemInvite(token, name.trim(), password);
			setUser(res.user);
			await goto('/');
		} catch (err) {
			submitError = err instanceof ApiError ? err.message : 'Could not activate account.';
		} finally {
			submitting = false;
		}
	}
</script>

<div class="rounded-2xl border border-border-light bg-bg-surface p-8">
	<div class="mb-6 space-y-1">
		<h1 class="font-display text-2xl font-extralight tracking-tight text-text-primary">
			Activate your account
		</h1>
		<p class="text-[13px] text-text-secondary">
			You've been invited to join an Atomicsite workspace.
		</p>
	</div>

	{#if loading}
		<div class="flex items-center justify-center py-8"><Spinner /></div>
	{:else if loadError}
		<div class="rounded-lg border border-danger/30 bg-danger/5 p-4">
			<p class="text-[13px] text-danger">{loadError}</p>
			<p class="mt-2 text-[12px] text-text-muted">
				Ask your workspace admin to send you a fresh invite.
			</p>
		</div>
	{:else if info}
		<form class="space-y-4" onsubmit={submit}>
			<Input label="Email" value={info.email} disabled />
			<div class="flex flex-col gap-1.5">
				<span class="text-[12px] font-medium text-text-secondary">Role</span>
				<div
					class="rounded-lg border border-border-light bg-bg-elevated px-3 py-2 text-[13px] text-text-primary"
				>
					{info.role}
				</div>
			</div>
			<Input
				label="Display name"
				autocomplete="name"
				bind:value={name}
				placeholder="What should we call you?"
			/>
			<Input
				label="Choose a password"
				type="password"
				autocomplete="new-password"
				required
				bind:value={password}
				hint="Minimum 12 characters."
			/>
			<Input
				label="Confirm password"
				type="password"
				autocomplete="new-password"
				required
				bind:value={confirm}
			/>
			{#if submitError}
				<p class="text-[12px] text-danger" role="alert">{submitError}</p>
			{/if}
			<Button type="submit" variant="primary" loading={submitting} disabled={submitting} class="w-full">
				Activate and sign in
			</Button>
		</form>
	{/if}
</div>
