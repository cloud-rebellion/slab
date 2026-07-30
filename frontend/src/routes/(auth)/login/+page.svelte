<script lang="ts">
	import { goto } from '$app/navigation';
	import { onMount } from 'svelte';
	import Button from '$lib/components/ui/Button.svelte';
	import Input from '$lib/components/ui/Input.svelte';
	import { ApiError } from '$lib/api/client';
	import * as authApi from '$lib/api/auth';
	import { setUser } from '$lib/stores/auth.svelte';
	import { page } from '$app/state';

	let email = $state('');
	let password = $state('');
	let totpCode = $state('');
	let totpRequired = $state(false);
	let error = $state('');
	let loading = $state(false);
	let oidcEnabled = $state(false);
	// SSO sign-in that still owes a second factor. The OIDC callback sends
	// TOTP-enrolled users here instead of issuing a session, holding a
	// short-lived pending cookie. In this mode email and password are
	// irrelevant (the issuer already authenticated them) so we ask for the
	// code and nothing else.
	let ssoMfaPending = $state(false);

	const ssoErrorMessages: Record<string, string> = {
		sso_denied: 'SSO sign-in was cancelled.',
		sso_invalid: 'SSO session expired. Please try again.',
		sso_no_email: 'SSO provider did not return an email address.',
		sso_no_account: 'No matching account for this SSO email.',
		sso_email_unverified: 'Your SSO provider reports this email address as unverified.',
		sso_inactive: 'Account is inactive.',
		sso_error: 'SSO sign-in failed. Please try again.'
	};

	onMount(async () => {
		ssoMfaPending = page.url.searchParams.get('mfa') === 'oidc';
		const ssoError = page.url.searchParams.get('error');
		if (ssoError && ssoErrorMessages[ssoError]) {
			error = ssoErrorMessages[ssoError];
		}
		try {
			const r = await fetch('/api/auth/oidc/config');
			if (r.ok) {
				const data = await r.json();
				oidcEnabled = !!data.oidc_enabled;
			}
		} catch {
			// SSO probe failure is non-fatal; password login still works.
		}
	});

	async function handleSubmit(e: SubmitEvent) {
		e.preventDefault();
		if (loading) return;
		error = '';
		loading = true;
		try {
			const res = ssoMfaPending
				? await authApi.completeOidcMfa(totpCode)
				: await authApi.login(email, password, totpRequired ? totpCode : undefined);
			setUser(res.user);
			await goto('/');
		} catch (err) {
			if (err instanceof ApiError && err.code === 'totp_required') {
				totpRequired = true;
				error = '';
			} else if (err instanceof ApiError) {
				error = err.message;
			} else {
				error = 'Sign in failed. Try again.';
			}
		} finally {
			loading = false;
		}
	}
</script>

<div class="rounded-2xl border border-border-light bg-bg-surface p-8">
	<div class="mb-6 space-y-1">
		<h1 class="font-display text-2xl font-extralight tracking-tight text-text-primary">
			{ssoMfaPending ? 'Two-factor authentication' : 'Admin sign in'}
		</h1>
		<p class="text-[13px] text-text-secondary">
			{ssoMfaPending
				? 'Your SSO provider signed you in. Enter your authentication code to finish.'
				: 'Atomicsite is invite-only. Ask your workspace admin for an invite link.'}
		</p>
	</div>

	<form class="space-y-4" onsubmit={handleSubmit}>
		{#if !ssoMfaPending}
			<Input
				label="Email"
				type="email"
				autocomplete="email"
				required
				bind:value={email}
				disabled={loading}
				placeholder="you@company.com"
			/>
			<Input
				label="Password"
				type="password"
				autocomplete="current-password"
				required
				bind:value={password}
				disabled={loading || totpRequired}
			/>
		{/if}

		{#if totpRequired || ssoMfaPending}
			<Input
				label="Authentication code"
				type="text"
				inputmode="numeric"
				pattern="[0-9 ]*"
				autocomplete="one-time-code"
				required
				bind:value={totpCode}
				disabled={loading}
				placeholder="6-digit code or recovery code"
			/>
			<p class="text-[12px] text-text-muted">
				Open your authenticator app and enter the 6-digit code, or paste a recovery code if you've lost the device.
			</p>
		{/if}

		{#if error}
			<p class="text-[12px] text-danger" role="alert">{error}</p>
		{/if}

		<Button type="submit" variant="primary" {loading} disabled={loading} class="w-full">
			{loading ? 'Signing in.' : totpRequired || ssoMfaPending ? 'Verify' : 'Sign in'}
		</Button>

		<p class="text-center text-[12px] text-text-muted">
			{#if ssoMfaPending}
				<a href="/login" class="text-accent underline-offset-2 hover:underline">
					Start over
				</a>
			{:else}
				<a href="/forgot-password" class="text-accent underline-offset-2 hover:underline">
					Forgot password?
				</a>
			{/if}
		</p>
	</form>

	{#if oidcEnabled && !ssoMfaPending}
		<div class="mt-6 flex items-center gap-3">
			<div class="h-px flex-1 bg-border-light"></div>
			<span class="text-[11px] uppercase tracking-wider text-text-muted">or</span>
			<div class="h-px flex-1 bg-border-light"></div>
		</div>
		<a
			href="/auth/oidc/login"
			class="mt-4 inline-flex w-full items-center justify-center rounded-lg border border-border-light bg-bg-elevated px-4 py-2.5 text-[13px] font-medium text-text-primary transition-colors hover:bg-bg-hover"
		>
			Sign in with SSO
		</a>
	{/if}
</div>
