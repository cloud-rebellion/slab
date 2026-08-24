<script lang="ts">
	import Input from '$lib/components/ui/Input.svelte';
	import Select from '$lib/components/ui/Select.svelte';
	import { deriveSlug, transliterateSlugChars, updateInfo, wizard } from '$lib/stores/wizard.svelte';
	import { currentWorkspace } from '$lib/stores/currentWorkspace.svelte';

	const SLUG_RE = /^[a-z0-9-]+$/;
	const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

	// The picker only earns its place when the operator actually has more than
	// one workspace. At n=1 the site silently lands in the only one there is,
	// and at n=0 the server refuses the create with a clear message anyway.
	const workspaceOptions = $derived(
		currentWorkspace.all.map((w) => ({ value: w.id, label: w.name }))
	);
	const showWorkspacePicker = $derived(workspaceOptions.length > 1);
	let workspaceValue = $state(
		wizard.value.info.workspace_id || currentWorkspace.value?.id || ''
	);
	$effect(() => {
		updateInfo({ workspace_id: workspaceValue });
	});

	const langOptions = [
		{ value: 'sv', label: 'Swedish' },
		{ value: 'en', label: 'English' },
		{ value: 'de', label: 'German' },
		{ value: 'fr', label: 'French' },
		{ value: 'es', label: 'Spanish' },
		{ value: 'no', label: 'Norwegian' },
		{ value: 'da', label: 'Danish' },
		{ value: 'fi', label: 'Finnish' },
		{ value: 'nl', label: 'Dutch' },
		{ value: 'it', label: 'Italian' }
	];

	const countryOptions = [
		{ value: 'SE', label: 'Sweden' },
		{ value: 'NO', label: 'Norway' },
		{ value: 'DK', label: 'Denmark' },
		{ value: 'FI', label: 'Finland' },
		{ value: 'DE', label: 'Germany' },
		{ value: 'NL', label: 'Netherlands' },
		{ value: 'FR', label: 'France' },
		{ value: 'ES', label: 'Spain' },
		{ value: 'IT', label: 'Italy' },
		{ value: 'GB', label: 'United Kingdom' },
		{ value: 'IE', label: 'Ireland' },
		{ value: 'CH', label: 'Switzerland' },
		{ value: 'AT', label: 'Austria' },
		{ value: 'BE', label: 'Belgium' },
		{ value: 'PT', label: 'Portugal' },
		{ value: 'PL', label: 'Poland' },
		{ value: 'US', label: 'United States' }
	];

	let nameValue = $state(wizard.value.info.name);
	let slugValue = $state(wizard.value.info.slug);
	let domainValue = $state(wizard.value.info.domain);
	let businessValue = $state(wizard.value.info.business_name);
	let emailValue = $state(wizard.value.info.contact_email);
	let langValue = $state(wizard.value.info.lang);
	let countryValue = $state(wizard.value.info.country);

	$effect(() => {
		updateInfo({ lang: langValue });
	});

	$effect(() => {
		updateInfo({ country: countryValue });
	});

	let slugTouched = $state(wizard.value.info.slug.length > 0);
	let slugDebounce: ReturnType<typeof setTimeout> | null = null;

	function commitName(value: string): void {
		nameValue = value;
		updateInfo({ name: value });
		if (slugTouched) return;
		if (slugDebounce) clearTimeout(slugDebounce);
		slugDebounce = setTimeout(() => {
			// Re-check slugTouched at fire time. If the user typed into slug
			// during the 200ms debounce, don't clobber their value.
			if (slugTouched) return;
			const next = deriveSlug(value);
			slugValue = next;
			updateInfo({ slug: next });
		}, 200);
	}

	function commitSlug(value: string): void {
		slugTouched = true;
		// Transliterate first so ÅÄÖÜ etc. become A/A/O/U instead of being stripped.
		const sanitized = transliterateSlugChars(value)
			.replace(/[^a-z0-9-]/g, '')
			.slice(0, 50);
		slugValue = sanitized;
		updateInfo({ slug: sanitized });
	}

	const errors = $derived.by(() => {
		const e: Partial<Record<'name' | 'slug' | 'email' | 'business' | 'country', string>> = {};
		if (nameValue.trim().length > 0 && nameValue.trim().length < 2) {
			e.name = 'Name must be at least 2 characters';
		}
		if (slugValue.length > 0) {
			if (!SLUG_RE.test(slugValue) || slugValue.length > 50) {
				e.slug = 'Slug must match a-z, 0-9, hyphens (max 50)';
			}
		}
		if (emailValue.trim().length > 0 && !EMAIL_RE.test(emailValue.trim())) {
			e.email = 'Enter a valid email address';
		}
		return e;
	});
</script>

<div class="flex flex-col gap-6">
	<div class="flex flex-col gap-1">
		<h2 class="font-display text-2xl font-extralight tracking-tight text-text-primary">
			Site information
		</h2>
		<p class="text-[13px] text-text-secondary">
			Used in your project, URLs, and the legal footer.
		</p>
	</div>

	<div class="grid grid-cols-1 gap-4 md:grid-cols-2">
		<Input
			label="Site name"
			placeholder="Acme Legal"
			value={nameValue}
			oninput={(e) => commitName((e.currentTarget as HTMLInputElement).value)}
			error={errors.name}
		/>
		<Input
			label="Slug"
			placeholder="acme-legal"
			value={slugValue}
			oninput={(e) => commitSlug((e.currentTarget as HTMLInputElement).value)}
			error={errors.slug}
			hint="Used in admin URLs. Auto-derived from name."
		/>
		<Input
			label="Domain (optional)"
			placeholder="acmelegal.se"
			value={domainValue}
			oninput={(e) => {
				domainValue = (e.currentTarget as HTMLInputElement).value;
				updateInfo({ domain: domainValue });
			}}
		/>
		<Input
			label="Business name"
			placeholder="Acme Legal AB"
			value={businessValue}
			oninput={(e) => {
				businessValue = (e.currentTarget as HTMLInputElement).value;
				updateInfo({ business_name: businessValue });
			}}
		/>
		<Input
			type="email"
			label="Contact email"
			placeholder="hello@acmelegal.se"
			value={emailValue}
			oninput={(e) => {
				emailValue = (e.currentTarget as HTMLInputElement).value;
				updateInfo({ contact_email: emailValue });
			}}
			error={errors.email}
		/>
		{#if showWorkspacePicker}
			<div class="flex flex-col gap-1.5">
				<span class="text-[12px] font-medium text-text-secondary">Workspace</span>
				<Select options={workspaceOptions} bind:value={workspaceValue} />
			</div>
		{/if}
		<div class="flex flex-col gap-1.5">
			<span class="text-[12px] font-medium text-text-secondary">Primary language</span>
			<Select options={langOptions} bind:value={langValue} />
		</div>
		<div class="flex flex-col gap-1.5">
			<span class="text-[12px] font-medium text-text-secondary">Country</span>
			<Select options={countryOptions} bind:value={countryValue} />
		</div>
	</div>
</div>
