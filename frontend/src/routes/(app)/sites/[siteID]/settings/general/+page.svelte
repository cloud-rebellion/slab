<script lang="ts">
	import { invalidateAll } from '$app/navigation';
	import * as sitesApi from '$lib/api/sites';
	import * as settingsApi from '$lib/api/settings';
	import { ApiError } from '$lib/api/client';
	import Input from '$lib/components/ui/Input.svelte';
	import Select from '$lib/components/ui/Select.svelte';
	import Switch from '$lib/components/ui/Switch.svelte';
	import Button from '$lib/components/ui/Button.svelte';
	import Card from '$lib/components/ui/Card.svelte';
	import Spinner from '$lib/components/ui/Spinner.svelte';
	import { setSite } from '$lib/stores/currentSite.svelte';
	import { toast } from '$lib/stores/toast.svelte';
	import { categoryMap } from '$lib/settings/nginxPreview';
	import type { Site } from '$lib/api/types';

	let { data }: { data: { site: Site } } = $props();

	const siteID = $derived(data.site.id);

	const langOptions = [
		{ value: 'en', label: 'English (en)' },
		{ value: 'sv', label: 'Swedish (sv)' },
		{ value: 'de', label: 'German (de)' },
		{ value: 'fr', label: 'French (fr)' },
		{ value: 'es', label: 'Spanish (es)' },
		{ value: 'it', label: 'Italian (it)' },
		{ value: 'no', label: 'Norwegian (no)' },
		{ value: 'da', label: 'Danish (da)' },
		{ value: 'fi', label: 'Finnish (fi)' },
		{ value: 'nl', label: 'Dutch (nl)' }
	];

	let loading = $state(true);
	let saving = $state(false);

	let name = $state('');
	let domain = $state('');
	let lang = $state('en');

	let metaTitleTemplate = $state('');
	let metaDescriptionTemplate = $state('');
	let canonicalBase = $state('');
	let additionalLangs = $state('');
	let hreflangStrategy = $state<'path' | 'subdomain' | 'off'>('path');
	let strictDesignLint = $state(true);

	type DesignFidelity = 'performance' | 'balanced' | 'showcase';
	let designFidelity = $state<DesignFidelity>('balanced');

	const fidelityOptions: { value: DesignFidelity; label: string; tagline: string }[] = [
		{ value: 'performance', label: 'Performance', tagline: 'Perfect scores. Static heroes, zero perpetual motion.' },
		{ value: 'balanced', label: 'Balanced', tagline: 'The standard rulebook and budgets. Default.' },
		{ value: 'showcase', label: 'Showcase', tagline: 'Jaw-dropping design. Trades some speed for craft.' }
	];

	const fidelityHint = $derived.by(() => {
		switch (designFidelity) {
			case 'performance':
				return 'The agent is steered to zero decorative weight: static hero graphics, logo strips instead of marquees, no canvas. Target: A+ on every category. Grading budgets are the standard ones; the discipline is in what gets authored.';
			case 'balanced':
				return 'Today’s behavior. The full taste rulebook applies and the Inspector grades with the standard budgets (200KB per page, one perpetual animation per viewport, canonical design tokens).';
			case 'showcase':
				return 'Unlocks expressive design: fx utility classes (scroll-driven reveals, parallax, ambient motion, gradient text, aurora bands), view transitions, bespoke tokens, a 4-animation motion budget, and custom blocks as a first-class path. Performance grades against showcase budgets (400KB per page, halved speed-metric weights) and every evaluation records the fidelity it was graded under. Security, privacy, accessibility, self-hosted fonts and slop lint stay at full strength.';
		}
	});

	function toBool(v: string | undefined, fallback: boolean): boolean {
		if (v === undefined || v === '') return fallback;
		const s = v.toLowerCase();
		return !(s === '0' || s === 'false' || s === 'off' || s === 'no');
	}

	const hreflangOptions = [
		{ value: 'path', label: 'Path-based (/sv/about, /de/about)' },
		{ value: 'subdomain', label: 'Subdomain (sv.example.com/about)' },
		{ value: 'off', label: 'Off (no hreflang emission)' }
	];

	const strategyHint = $derived.by(() => {
		switch (hreflangStrategy) {
			case 'path':
				return 'Recommended. Each language sits at /<lang>/<slug>; default language at root. Atomicsite emits hreflang automatically when a page has counterparts in other locales (e.g. /about + /sv/about both published). One domain, one cert, no DNS work.';
			case 'subdomain':
				return 'For sites already running sv.example.com / de.example.com separately. Atomicsite trusts your additional_langs list; you handle the DNS + TLS per subdomain. Hreflang URLs use the <lang>.<host> pattern.';
			case 'off':
				return 'Disable hreflang emission entirely. Use only if you manage hreflang from a custom layout or your site is single-language and you want to be explicit.';
		}
	});

	let initial = $state({
		name: '',
		domain: '',
		lang: 'en',
		metaTitleTemplate: '',
		metaDescriptionTemplate: '',
		canonicalBase: '',
		additionalLangs: '',
		hreflangStrategy: 'path' as 'path' | 'subdomain' | 'off',
		strictDesignLint: true,
		designFidelity: 'balanced' as DesignFidelity
	});

	async function load() {
		loading = true;
		try {
			name = data.site.name;
			domain = data.site.domain;
			lang = data.site.lang || 'en';

			const [seo, general, design] = await Promise.all([
				settingsApi.listByCategory(siteID, 'seo'),
				settingsApi.listByCategory(siteID, 'general'),
				settingsApi.listByCategory(siteID, 'design')
			]);
			const seoMap = categoryMap(seo);
			const genMap = categoryMap(general);
			const designMap = categoryMap(design);

			metaTitleTemplate = seoMap.meta_title_template || '';
			metaDescriptionTemplate = seoMap.meta_description_template || '';
			canonicalBase = seoMap.canonical_base || '';
			additionalLangs = genMap.additional_langs || '';
			const strat = (seoMap.hreflang_strategy || 'path') as
				| 'path'
				| 'subdomain'
				| 'off';
			hreflangStrategy = ['path', 'subdomain', 'off'].includes(strat) ? strat : 'path';
			strictDesignLint = toBool(designMap.strict_lint, true);
			const fid = (designMap.fidelity || 'balanced') as DesignFidelity;
			designFidelity = ['performance', 'balanced', 'showcase'].includes(fid) ? fid : 'balanced';

			initial = {
				name,
				domain,
				lang,
				metaTitleTemplate,
				metaDescriptionTemplate,
				canonicalBase,
				additionalLangs,
				hreflangStrategy,
				strictDesignLint,
				designFidelity
			};
		} catch (err) {
			toast.error(err instanceof Error ? err.message : 'Failed to load settings.');
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		void load();
	});

	const dirty = $derived(
		name !== initial.name ||
			domain !== initial.domain ||
			lang !== initial.lang ||
			metaTitleTemplate !== initial.metaTitleTemplate ||
			metaDescriptionTemplate !== initial.metaDescriptionTemplate ||
			canonicalBase !== initial.canonicalBase ||
			additionalLangs !== initial.additionalLangs ||
			hreflangStrategy !== initial.hreflangStrategy ||
			strictDesignLint !== initial.strictDesignLint ||
			designFidelity !== initial.designFidelity
	);

	function discard() {
		name = initial.name;
		domain = initial.domain;
		lang = initial.lang;
		metaTitleTemplate = initial.metaTitleTemplate;
		metaDescriptionTemplate = initial.metaDescriptionTemplate;
		canonicalBase = initial.canonicalBase;
		additionalLangs = initial.additionalLangs;
		hreflangStrategy = initial.hreflangStrategy;
		strictDesignLint = initial.strictDesignLint;
		designFidelity = initial.designFidelity;
	}

	// Live preview of how the meta-title template renders with a sample
	// page. Uses the same {token} substitution the builder applies; empty
	// tokens collapse along with adjacent separators.
	function expandPreview(tpl: string, fallback: string): string {
		if (!tpl.trim()) return fallback;
		const vars: Record<string, string> = {
			'{page_title}': 'About us',
			'{page_description}': 'Who we are and what we do',
			'{site_name}': name || 'Site name',
			'{lang}': lang || 'en',
			'{separator}': '|'
		};
		let out = tpl;
		for (const [k, v] of Object.entries(vars)) {
			out = out.split(k).join(v);
		}
		// Collapse empty separator runs left over by missing tokens.
		out = out.replace(/\s*\|\s*\|\s*/g, ' | ');
		out = out.replace(/^\s*\|\s*|\s*\|\s*$/g, '');
		out = out.replace(/\s+/g, ' ').trim();
		return out || fallback;
	}

	const titlePreview = $derived(expandPreview(metaTitleTemplate, 'About us'));
	const descPreview = $derived(
		expandPreview(metaDescriptionTemplate, 'Who we are and what we do')
	);

	async function save() {
		if (saving || !dirty) return;
		saving = true;
		try {
			const sitePatch: Record<string, string> = {};
			if (name !== initial.name) sitePatch.name = name;
			if (domain !== initial.domain) sitePatch.domain = domain;
			if (lang !== initial.lang) sitePatch.lang = lang;

			let updatedSite: Site | null = null;
			if (Object.keys(sitePatch).length > 0) {
				updatedSite = await sitesApi.update(siteID, sitePatch);
				setSite(updatedSite);
				// The [siteID] layout load is memoized by SvelteKit, so the
				// site header keeps rendering the OLD name/domain until a
				// hard reload without this.
				await invalidateAll();
			}

			const items: settingsApi.SettingUpsertInput[] = [
				{ category: 'seo', key: 'meta_title_template', value: metaTitleTemplate },
				{ category: 'seo', key: 'meta_description_template', value: metaDescriptionTemplate },
				{ category: 'seo', key: 'canonical_base', value: canonicalBase },
				{ category: 'seo', key: 'hreflang_strategy', value: hreflangStrategy },
				{ category: 'general', key: 'additional_langs', value: additionalLangs },
				{ category: 'general', key: 'default_lang', value: lang },
				{ category: 'design', key: 'strict_lint', value: strictDesignLint ? '1' : '0' },
				{ category: 'design', key: 'fidelity', value: designFidelity }
			];
			await settingsApi.bulkUpsert(siteID, items);

			initial = {
				name,
				domain,
				lang,
				metaTitleTemplate,
				metaDescriptionTemplate,
				canonicalBase,
				additionalLangs,
				hreflangStrategy,
				strictDesignLint,
				designFidelity
			};
			toast.success('General settings saved.');
		} catch (err) {
			const msg = err instanceof ApiError ? err.message : 'Failed to save settings.';
			toast.error(msg);
		} finally {
			saving = false;
		}
	}
</script>

<div class="mx-auto max-w-7xl px-6 py-8">
	<header class="flex flex-col gap-1.5">
		<h1 class="font-display text-3xl font-extralight tracking-tight text-text-primary">
			General
		</h1>
		<p class="text-[13px] text-text-secondary">
			Site name, languages, and meta-tag defaults. Custom hostnames + TLS live in
			<a href="/sites/{siteID}/settings/domains" class="text-accent underline-offset-2 hover:underline">Domains</a>;
			where the built artifact ships to is in
			<a href="/sites/{siteID}/settings/deployment" class="text-accent underline-offset-2 hover:underline">Deployment</a>.
		</p>
	</header>

	{#if loading}
		<div class="mt-8 flex items-center justify-center py-12">
			<Spinner />
		</div>
	{:else}
		<div class="mt-8 flex flex-col gap-5">
			<Card padding="md">
				<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">Identity</h2>
				<div class="mt-4 grid grid-cols-1 gap-4 sm:grid-cols-2">
					<Input label="Site name" bind:value={name} />
					<Input
						label="Default canonical hostname"
						placeholder="example.com"
						hint="Legacy fallback. If you've added a hostname in Domains and marked it canonical, that wins."
						bind:value={domain}
					/>
				</div>
			</Card>

			<Card padding="md">
				<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">
					Languages and hreflang
				</h2>
				<p class="mt-2 text-[12px] text-text-muted">
					Atomicsite optimizes for path-based multi-language by default
					(/sv/about, /de/about). Subdomain mode is opt-in for sites that
					already run sv.example.com. TLD-per-locale (example.se,
					example.de) is handled as separate sites linked together; that
					setup lives outside this page.
				</p>
				<div class="mt-4 grid grid-cols-1 gap-4 sm:grid-cols-2">
					<div class="flex flex-col gap-1.5">
						<label for="lang" class="text-[12px] font-medium text-text-secondary">
							Default language
						</label>
						<Select options={langOptions} bind:value={lang} />
					</div>
					<Input
						label="Additional languages"
						placeholder="sv,de,fr"
						hint="Comma-separated language codes (sv, de, fr). Pages whose slug starts with /<lang>/ are treated as that language's locale."
						bind:value={additionalLangs}
					/>
					<div class="flex flex-col gap-1.5 sm:col-span-2">
						<span class="text-[12px] font-medium text-text-secondary">
							Hreflang strategy
						</span>
						<Select
							options={hreflangOptions}
							value={hreflangStrategy}
							onchange={(v) => (hreflangStrategy = v as 'path' | 'subdomain' | 'off')}
						/>
						<p class="text-[11px] text-text-muted">{strategyHint}</p>
					</div>
				</div>
			</Card>

			<Card padding="md">
				<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">
					Meta defaults
				</h2>
				<p class="mt-2 text-[12px] text-text-muted">
					Templates apply to every page that doesn't override its own meta.
					Leave empty to use the page-level value verbatim. Tokens:
					<code class="font-mono text-[11px]">{'{page_title}'}</code>,
					<code class="font-mono text-[11px]">{'{site_name}'}</code>,
					<code class="font-mono text-[11px]">{'{lang}'}</code>,
					<code class="font-mono text-[11px]">{'{page_description}'}</code>,
					<code class="font-mono text-[11px]">{'{separator}'}</code>. Empty
					tokens drop with adjacent
					<code class="font-mono text-[11px]">|</code>
					so a missing site_name doesn't leave a trailing pipe.
				</p>
				<div class="mt-4 flex flex-col gap-4">
					<div class="flex flex-col gap-1.5">
						<Input
							label="Meta title template"
							placeholder={'{page_title} | {site_name}'}
							bind:value={metaTitleTemplate}
						/>
						<p class="rounded-md border border-border-light bg-bg-elevated/50 px-3 py-2 font-mono text-[11.5px] text-text-secondary">
							Preview: <span class="text-text-primary">{titlePreview}</span>
						</p>
					</div>
					<div class="flex flex-col gap-1.5">
						<Input
							label="Meta description template"
							placeholder={'{page_description}'}
							bind:value={metaDescriptionTemplate}
						/>
						<p class="rounded-md border border-border-light bg-bg-elevated/50 px-3 py-2 font-mono text-[11.5px] text-text-secondary">
							Preview: <span class="text-text-primary">{descPreview}</span>
						</p>
					</div>
					<Input
						label="Canonical base"
						placeholder="https://example.com"
						hint="Override the default URL prefix (https://your primary domain). Useful when the public URL differs from the build's domain (CDN, sub-path proxy, staging vs prod)."
						bind:value={canonicalBase}
					/>
				</div>
			</Card>

			<Card padding="md">
				<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">
					Design fidelity
				</h2>
				<p class="mt-2 text-[12px] text-text-muted">
					The design-freedom dial. It adapts the playbook your AI agent reads
					AND the rubric the Inspector grades with, so an ambitious build is
					never punished by rules it couldn't see. Change it before
					authoring, then rebuild; every evaluation records the fidelity it
					was graded under. Security, privacy, accessibility and content
					honesty are identical in all three.
				</p>
				<!-- ARIA radio pattern: roving tabindex (one tab stop for the
				     whole group) + ArrowLeft/Up and ArrowRight/Down move the
				     selection with wrap and follow focus. -->
				<div class="mt-4 grid grid-cols-1 gap-3 sm:grid-cols-3" role="radiogroup" aria-label="Design fidelity">
					{#each fidelityOptions as opt, i (opt.value)}
						<button
							type="button"
							role="radio"
							aria-checked={designFidelity === opt.value}
							tabindex={designFidelity === opt.value ? 0 : -1}
							onclick={() => (designFidelity = opt.value)}
							onkeydown={(e) => {
								const dir =
									e.key === 'ArrowRight' || e.key === 'ArrowDown'
										? 1
										: e.key === 'ArrowLeft' || e.key === 'ArrowUp'
											? -1
											: 0;
								if (dir === 0) return;
								e.preventDefault();
								const nextIdx = (i + dir + fidelityOptions.length) % fidelityOptions.length;
								const next = fidelityOptions[nextIdx];
								if (!next) return;
								designFidelity = next.value;
								const group = (e.currentTarget as HTMLElement).closest('[role="radiogroup"]');
								group?.querySelectorAll<HTMLElement>('[role="radio"]')[nextIdx]?.focus();
							}}
							class="flex flex-col gap-1 rounded-lg border px-4 py-3 text-left transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent
								{designFidelity === opt.value
								? 'border-accent bg-bg-elevated ring-1 ring-accent'
								: 'border-border-light bg-bg-elevated/40 hover:border-border-strong'}"
						>
							<span class="text-[13px] font-medium text-text-primary">{opt.label}</span>
							<span class="text-[11.5px] leading-snug text-text-muted">{opt.tagline}</span>
						</button>
					{/each}
				</div>
				<p class="mt-3 rounded-md border border-border-light bg-bg-elevated/50 px-3 py-2 text-[11.5px] leading-relaxed text-text-secondary">
					{fidelityHint}
				</p>
			</Card>

			<Card padding="md">
				<h2 class="text-[11px] font-mono uppercase tracking-[0.2em] text-text-muted">
					Design quality
				</h2>
				<p class="mt-2 text-[12px] text-text-muted">
					Strict design lint refuses agent writes that hit the playbook's hard
					rules: slop terms ("elevate", "unleash", "synergy"), placeholder
					testimonial names, placeholder company names, suspicious round
					numbers, and hero graphics that drift from a locked page
					archetype. The agent gets a structured error with fix hints and
					must revise before the block ships. Soft warnings (long headline,
					generic hero) stay non-blocking. Independent of design fidelity:
					fidelity changes which taste rules apply; this toggle changes
					whether hard violations block writes (slop always blocks while it
					is on, in every fidelity; showcase only unblocks archetype
					drift). Turn this off only if you trust the source of the copy.
				</p>
				<div class="mt-4 flex items-center justify-between gap-4 rounded-lg border border-border-light bg-bg-elevated/40 px-4 py-3">
					<div class="flex flex-col gap-0.5">
						<span class="text-[13px] font-medium text-text-primary">
							Block agent writes on critical lint findings
						</span>
						<span class="text-[11.5px] text-text-muted">
							Default ON. Off = warnings only, agent ships anyway.
						</span>
					</div>
					<Switch
						bind:checked={strictDesignLint}
						ariaLabel="Enable strict design lint"
					/>
				</div>
			</Card>

			<div
				class="sticky bottom-0 -mx-6 flex items-center justify-between gap-3 border-t border-border-light bg-bg-surface/85 px-6 py-3 backdrop-blur"
			>
				<span class="text-[12px] text-text-muted">
					{dirty ? 'Unsaved changes.' : 'Up to date.'}
				</span>
				<div class="flex items-center gap-2">
					<Button variant="ghost" onclick={discard} disabled={!dirty || saving}>Discard</Button>
					<Button variant="primary" onclick={save} loading={saving} disabled={!dirty}>
						Save
					</Button>
				</div>
			</div>
		</div>
	{/if}
</div>
