<script lang="ts">
	import '../app.css';
	import { onMount } from 'svelte';
	import Toast from '$lib/components/ui/Toast.svelte';
	import { installFlareBrowserCapture } from '$lib/flare-client';
	let { children } = $props();

	onMount(() => installFlareBrowserCapture({
		dsn: import.meta.env.PUBLIC_FLARE_DSN,
		environment: import.meta.env.PUBLIC_FLARE_ENVIRONMENT || 'production',
		release: import.meta.env.PUBLIC_FLARE_RELEASE
	}));
</script>

{@render children()}
<Toast />
