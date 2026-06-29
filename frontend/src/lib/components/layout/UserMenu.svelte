<script lang="ts">
	import { DropdownMenu } from 'bits-ui';
	import { goto } from '$app/navigation';
	import { BookOpen, ChevronDown, LogOut, Settings, Users } from 'lucide-svelte';
	import * as authApi from '$lib/api/auth';
	import { auth, clearUser } from '$lib/stores/auth.svelte';

	function initials(name: string | undefined, email: string | undefined): string {
		const source = (name && name.trim()) || email || '';
		if (!source) return '?';
		const parts = source.split(/[\s@.]+/).filter(Boolean);
		const first = parts[0];
		if (!first) return '?';
		if (parts.length === 1) return first.slice(0, 2).toUpperCase();
		const second = parts[1];
		return ((first[0] ?? '') + (second?.[0] ?? '')).toUpperCase() || '?';
	}

	const label = $derived(initials(auth.value?.name, auth.value?.email));
	const displayName = $derived(auth.value?.name || auth.value?.email || 'Account');
	const isAdmin = $derived(auth.value?.role === 'admin');

	async function handleLogout() {
		try {
			await authApi.logout();
		} catch {
			// ignore, still clear local state
		}
		clearUser();
		await goto('/login');
	}
</script>

<DropdownMenu.Root>
	<DropdownMenu.Trigger
		class="inline-flex h-9 items-center gap-1.5 rounded-md px-1.5 text-[13px] text-text-primary transition-colors hover:bg-bg-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
		aria-label="Account menu"
	>
		<span
			class="inline-flex h-7 w-7 items-center justify-center rounded-full bg-bg-elevated border border-border text-[11px] font-medium text-text-primary"
		>
			{label}
		</span>
		<ChevronDown class="h-3.5 w-3.5 text-text-muted" />
	</DropdownMenu.Trigger>
	<DropdownMenu.Portal>
		<DropdownMenu.Content
			sideOffset={6}
			align="end"
			class="z-[110] w-56 rounded-xl border border-border bg-bg-surface p-1 shadow-popover focus:outline-none data-[state=open]:animate-fadeIn"
		>
			<div class="px-2.5 py-2">
				<p class="truncate text-[13px] font-medium text-text-primary">{displayName}</p>
				{#if auth.value?.email && auth.value.email !== displayName}
					<p class="truncate text-[11px] text-text-muted">{auth.value.email}</p>
				{/if}
			</div>
			<div class="my-1 h-px bg-border-light"></div>
			<DropdownMenu.Item
				class="flex cursor-pointer items-center gap-2 rounded-md px-2.5 py-2 text-[13px] text-text-secondary outline-none transition-colors data-[highlighted]:bg-bg-hover data-[highlighted]:text-text-primary"
				onSelect={() => goto('/account')}
			>
				<Settings class="h-3.5 w-3.5" />
				Account settings
			</DropdownMenu.Item>
			<DropdownMenu.Item
				class="flex cursor-pointer items-center gap-2 rounded-md px-2.5 py-2 text-[13px] text-text-secondary outline-none transition-colors data-[highlighted]:bg-bg-hover data-[highlighted]:text-text-primary"
				onSelect={() => goto('/docs')}
			>
				<BookOpen class="h-3.5 w-3.5" />
				Documentation
			</DropdownMenu.Item>
			{#if isAdmin}
				<DropdownMenu.Item
					class="flex cursor-pointer items-center gap-2 rounded-md px-2.5 py-2 text-[13px] text-text-secondary outline-none transition-colors data-[highlighted]:bg-bg-hover data-[highlighted]:text-text-primary"
					onSelect={() => goto('/members')}
				>
					<Users class="h-3.5 w-3.5" />
					Workspace members
				</DropdownMenu.Item>
			{/if}
			<DropdownMenu.Separator class="my-1 h-px bg-border-light" />
			<DropdownMenu.Item
				class="flex cursor-pointer items-center gap-2 rounded-md px-2.5 py-2 text-[13px] text-danger outline-none transition-colors data-[highlighted]:bg-danger/10"
				onSelect={handleLogout}
			>
				<LogOut class="h-3.5 w-3.5" />
				Sign out
			</DropdownMenu.Item>
		</DropdownMenu.Content>
	</DropdownMenu.Portal>
</DropdownMenu.Root>
