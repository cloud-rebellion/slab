<script lang="ts">
	let {
		primary,
		secondary,
		bg,
		text,
		surface = '#ffffff',
		border = '',
		muted = '',
		accent = '',
		onPrimary = '#ffffff',
		fontHeading,
		fontBody,
		viewport = 'mobile'
	}: {
		primary: string;
		secondary: string;
		bg: string;
		text: string;
		surface?: string;
		border?: string;
		muted?: string;
		accent?: string;
		onPrimary?: string;
		fontHeading: string;
		fontBody: string;
		viewport?: 'mobile' | 'desktop';
	} = $props();

	const effectiveBorder = $derived(
		border && /^#[0-9a-fA-F]{6}$/.test(border)
			? border
			: 'color-mix(in srgb, ' + text + ' 12%, transparent)'
	);
	const effectiveMuted = $derived(
		muted && /^#[0-9a-fA-F]{6}$/.test(muted)
			? muted
			: 'color-mix(in srgb, ' + text + ' 70%, transparent)'
	);
	const effectiveAccent = $derived(
		accent && /^#[0-9a-fA-F]{6}$/.test(accent) ? accent : primary
	);

	const style = $derived(
		[
			`--bp-primary:${primary}`,
			`--bp-secondary:${secondary}`,
			`--bp-bg:${bg}`,
			`--bp-text:${text}`,
			`--bp-surface:${surface}`,
			`--bp-border:${effectiveBorder}`,
			`--bp-muted:${effectiveMuted}`,
			`--bp-accent:${effectiveAccent}`,
			`--bp-on-primary:${onPrimary}`,
			`--bp-font-heading:'${fontHeading}'`,
			`--bp-font-body:'${fontBody}'`
		].join(';') + ';'
	);

	const DESKTOP_BASE_W = 1100;
	let outerWidth = $state(0);
	let innerHeight = $state(0);
	const scale = $derived(
		viewport === 'desktop' && outerWidth > 0 ? outerWidth / DESKTOP_BASE_W : 1
	);
	const desktopWrapHeight = $derived(
		viewport === 'desktop' && innerHeight > 0 ? innerHeight * scale : 0
	);
</script>

<div bind:clientWidth={outerWidth}>
	{#if viewport === 'mobile'}
		<div class="mx-auto w-full max-w-[360px]">
			<div class="rounded-[2rem] border border-border-light bg-bg-elevated p-2 shadow-sm">
				<div class="mb-2 flex items-center justify-center">
					<span class="block h-1 w-12 rounded-full bg-border"></span>
				</div>
				<div
					class="overflow-hidden rounded-2xl border"
					style={style +
						"background:var(--bp-bg);color:var(--bp-text);border-color:var(--bp-border);font-feature-settings:'cv11','ss01';"}
				>
					<div
						class="flex items-center justify-between border-b px-4 py-3"
						style="border-color:var(--bp-border);"
					>
						<span
							class="text-[12px] font-semibold tracking-tight"
							style="font-family:var(--bp-font-heading);"
						>
							Acme
						</span>
						<button
							type="button"
							aria-label="Open menu"
							class="inline-flex h-6 w-6 items-center justify-center"
							style="color:var(--bp-muted);"
						>
							<svg
								xmlns="http://www.w3.org/2000/svg"
								width="16"
								height="16"
								viewBox="0 0 24 24"
								fill="none"
								stroke="currentColor"
								stroke-width="1.75"
								stroke-linecap="round"
								stroke-linejoin="round"
							>
								<line x1="3" y1="6" x2="21" y2="6"></line>
								<line x1="3" y1="12" x2="21" y2="12"></line>
								<line x1="3" y1="18" x2="21" y2="18"></line>
							</svg>
						</button>
					</div>

					<div class="px-5 pb-7 pt-9">
						<span
							class="text-[10px] font-medium uppercase tracking-[0.18em]"
							style="color:var(--bp-secondary);font-family:var(--bp-font-body);"
						>
							Atomic, by design
						</span>
						<h1
							class="mt-3"
							style="font-family:var(--bp-font-heading);font-weight:200;letter-spacing:-0.025em;font-size:26px;line-height:1.1;color:var(--bp-text);"
						>
							A calm site that earns the click.
						</h1>
						<p
							class="mt-3"
							style="font-family:var(--bp-font-body);font-size:12px;line-height:1.6;color:var(--bp-muted);"
						>
							Restrained type, accessible color, real content.
						</p>
						<div class="mt-5">
							<button
								type="button"
								class="inline-flex h-9 w-full items-center justify-center rounded-lg px-4 text-[13px] font-medium"
								style="background:var(--bp-primary);color:var(--bp-on-primary);font-family:var(--bp-font-body);"
							>
								Get started
							</button>
						</div>
					</div>

					<div
						class="grid grid-cols-1 gap-3 px-5 pb-6"
						style="font-family:var(--bp-font-body);"
					>
						<div
							class="rounded-xl border p-4"
							style="border-color:var(--bp-border);background:var(--bp-surface);"
						>
							<h3
								style="font-family:var(--bp-font-heading);font-weight:300;font-size:14px;letter-spacing:-0.01em;color:var(--bp-text);"
							>
								Composable blocks
							</h3>
							<p
								class="mt-1.5"
								style="font-size:11.5px;line-height:1.55;color:var(--bp-muted);"
							>
								Sections snap together cleanly without breaking your grid.
							</p>
							<span
								class="mt-2 inline-block text-[11.5px] font-medium"
								style="color:var(--bp-accent);"
							>
								Learn more
							</span>
						</div>
						<div
							class="rounded-xl border p-4"
							style="border-color:var(--bp-border);background:var(--bp-surface);"
						>
							<h3
								style="font-family:var(--bp-font-heading);font-weight:300;font-size:14px;letter-spacing:-0.01em;color:var(--bp-text);"
							>
								Built for review
							</h3>
							<p
								class="mt-1.5"
								style="font-size:11.5px;line-height:1.55;color:var(--bp-muted);"
							>
								Every page passes WCAG, SEO, and performance gates before it ships.
							</p>
							<span
								class="mt-2 inline-block text-[11.5px] font-medium"
								style="color:var(--bp-accent);"
							>
								See checks
							</span>
						</div>
					</div>

					<div
						class="flex flex-wrap items-center gap-3 border-t px-5 py-3 text-[10.5px]"
						style="border-color:var(--bp-border);font-family:var(--bp-font-body);color:var(--bp-secondary);"
					>
						<span>Privacy</span>
						<span>Terms</span>
						<span>Status</span>
						<span>Contact</span>
					</div>
				</div>
			</div>
		</div>
	{:else}
		<div
			class="overflow-hidden rounded-2xl border border-border-light bg-bg-elevated shadow-sm"
			style="height: {desktopWrapHeight}px;"
		>
			<div
				class="origin-top-left"
				style="width: {DESKTOP_BASE_W}px; transform: scale({scale}); transform-origin: top left;"
			>
				<div
					bind:clientHeight={innerHeight}
					class="overflow-hidden border-b"
					style={style +
						"background:var(--bp-bg);color:var(--bp-text);border-color:var(--bp-border);font-feature-settings:'cv11','ss01';"}
				>
					<div
						class="flex items-center justify-between border-b px-6 py-4"
						style="border-color:var(--bp-border);"
					>
						<span
							class="text-[12px] font-semibold tracking-tight"
							style="font-family:var(--bp-font-heading);"
						>
							Acme
						</span>
						<nav
							class="flex gap-5"
							style="font-family:var(--bp-font-body);font-size:12px;"
						>
							<span style="color:var(--bp-muted);">Product</span>
							<span style="color:var(--bp-muted);">Pricing</span>
							<span style="color:var(--bp-muted);">About</span>
						</nav>
					</div>

					<div class="px-12 pb-12 pt-16">
						<span
							class="text-[10px] font-medium uppercase tracking-[0.18em]"
							style="color:var(--bp-secondary);font-family:var(--bp-font-body);"
						>
							Atomic, by design
						</span>
						<h1
							class="mt-4 max-w-2xl"
							style="font-family:var(--bp-font-heading);font-weight:200;letter-spacing:-0.025em;font-size:48px;line-height:1.05;color:var(--bp-text);"
						>
							A calm site that earns the click.
						</h1>
						<p
							class="mt-5 max-w-xl"
							style="font-family:var(--bp-font-body);font-size:14px;line-height:1.65;color:var(--bp-muted);"
						>
							Restrained type, accessible color, real content. Everything else is decoration.
						</p>
						<div class="mt-7">
							<button
								type="button"
								class="inline-flex h-11 items-center rounded-lg px-5 text-[14px] font-medium"
								style="background:var(--bp-primary);color:var(--bp-on-primary);font-family:var(--bp-font-body);"
							>
								Get started
							</button>
						</div>
					</div>

					<div
						class="grid grid-cols-3 gap-4 px-12 pb-12"
						style="font-family:var(--bp-font-body);"
					>
						<div
							class="rounded-xl border p-5"
							style="border-color:var(--bp-border);background:var(--bp-surface);"
						>
							<h3
								style="font-family:var(--bp-font-heading);font-weight:300;font-size:16px;letter-spacing:-0.01em;color:var(--bp-text);"
							>
								Composable blocks
							</h3>
							<p
								class="mt-2"
								style="font-size:12.5px;line-height:1.6;color:var(--bp-muted);"
							>
								Sections snap together cleanly without breaking your grid.
							</p>
							<span
								class="mt-3 inline-block text-[12px] font-medium"
								style="color:var(--bp-accent);"
							>
								Learn more
							</span>
						</div>
						<div
							class="rounded-xl border p-5"
							style="border-color:var(--bp-border);background:var(--bp-surface);"
						>
							<h3
								style="font-family:var(--bp-font-heading);font-weight:300;font-size:16px;letter-spacing:-0.01em;color:var(--bp-text);"
							>
								Built for review
							</h3>
							<p
								class="mt-2"
								style="font-size:12.5px;line-height:1.6;color:var(--bp-muted);"
							>
								Every page passes WCAG, SEO, and performance gates before it ships.
							</p>
							<span
								class="mt-3 inline-block text-[12px] font-medium"
								style="color:var(--bp-accent);"
							>
								See checks
							</span>
						</div>
						<div
							class="rounded-xl border p-5"
							style="border-color:var(--bp-border);background:var(--bp-surface);"
						>
							<h3
								style="font-family:var(--bp-font-heading);font-weight:300;font-size:16px;letter-spacing:-0.01em;color:var(--bp-text);"
							>
								Honest contrast
							</h3>
							<p
								class="mt-2"
								style="font-size:12.5px;line-height:1.6;color:var(--bp-muted);"
							>
								Every text on every surface meets WCAG AA without you watching.
							</p>
							<span
								class="mt-3 inline-block text-[12px] font-medium"
								style="color:var(--bp-accent);"
							>
								Read the matrix
							</span>
						</div>
					</div>

					<div
						class="flex flex-wrap items-center gap-5 border-t px-12 py-5 text-[11px]"
						style="border-color:var(--bp-border);font-family:var(--bp-font-body);color:var(--bp-secondary);"
					>
						<span>Privacy</span>
						<span>Terms</span>
						<span>Status</span>
						<span>Contact</span>
					</div>
				</div>
			</div>
		</div>
	{/if}
</div>
