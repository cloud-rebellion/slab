<script lang="ts">
	import { page } from '$app/state';

	let { siteID }: { siteID: string } = $props();

	type Tab = { id: string; label: string; href: string };

	const tabs = $derived<Tab[]>([
		{ id: 'overview', label: 'Overview', href: `/sites/${siteID}` },
		{ id: 'pages', label: 'Pages', href: `/sites/${siteID}/pages` },
		{ id: 'forms', label: 'Forms', href: `/sites/${siteID}/forms` },
		{ id: 'global-blocks', label: 'Global blocks', href: `/sites/${siteID}/global-blocks` },
		{ id: 'branding', label: 'Branding', href: `/sites/${siteID}/branding` },
		{ id: 'media', label: 'Media', href: `/sites/${siteID}/media` },
		{ id: 'build', label: 'Build', href: `/sites/${siteID}/build` },
		{ id: 'clarifications', label: 'Clarifications', href: `/sites/${siteID}/clarifications` },
		{ id: 'migrations', label: 'Migrations', href: `/sites/${siteID}/migrations` },
		{ id: 'store', label: 'Store', href: `/sites/${siteID}/store` },
		{ id: 'apps', label: 'Apps', href: `/sites/${siteID}/apps` },
		{ id: 'analytics', label: 'Analytics', href: `/sites/${siteID}/analytics` },
		{ id: 'cookies', label: 'Cookies', href: `/sites/${siteID}/cookies` },
		{ id: 'settings', label: 'Settings', href: `/sites/${siteID}/settings` }
	]);

	function isActive(href: string): boolean {
		const path = page.url.pathname;
		if (href === `/sites/${siteID}`) {
			return path === href;
		}
		return path === href || path.startsWith(`${href}/`);
	}
</script>

<nav class="border-b border-border-light bg-bg">
	<div
		class="mx-auto flex max-w-7xl items-center gap-1 overflow-x-auto px-6 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"
	>
		{#each tabs as tab (tab.id)}
			{@const active = isActive(tab.href)}
			<a
				href={tab.href}
				class="relative -mb-px inline-flex shrink-0 items-center whitespace-nowrap px-3 py-3 text-[13px] transition-colors focus-visible:outline-none focus-visible:text-text-primary {active
					? 'text-text-primary border-b-2 border-accent font-medium'
					: 'text-text-muted hover:text-text-primary border-b-2 border-transparent'}"
				aria-current={active ? 'page' : undefined}
			>
				{tab.label}
			</a>
		{/each}
	</div>
</nav>
