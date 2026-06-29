<script lang="ts">
	import type { Grade, Tone } from '$lib/evaluations/grade';
	import { gradeTone } from '$lib/evaluations/grade';

	type Size = 'sm' | 'md' | 'lg';

	let {
		grade,
		size = 'md',
		class: className = ''
	}: {
		grade: Grade;
		size?: Size;
		class?: string;
	} = $props();

	const tone: Tone = $derived(gradeTone(grade));

	// System semantic tokens (self-theme in dark via the [data-theme] ramps),
	// not raw Tailwind palette. The two success / warning tiers vary opacity,
	// not hue, to preserve the monochrome-plus-single-semantic discipline.
	const toneClasses: Record<Tone, string> = {
		success: 'bg-success/15 text-success ring-1 ring-success/30',
		'success-soft': 'bg-success/10 text-success ring-1 ring-success/20',
		warning: 'bg-warning/15 text-warning ring-1 ring-warning/30',
		'warning-strong': 'bg-warning/20 text-warning ring-1 ring-warning/40',
		danger: 'bg-danger/15 text-danger ring-1 ring-danger/30',
		muted: 'bg-bg-elevated text-text-muted ring-1 ring-border-light'
	};

	// 2-character grades ("A+", "B-") need a slightly smaller font so they
	// stay centred inside the same square as single-character "A".
	const isWide = $derived(grade.length === 2);

	const sizeClasses: Record<Size, string> = {
		sm: 'h-9 w-9',
		md: 'h-14 w-14',
		lg: 'h-20 w-20'
	};
	const fontClasses: Record<Size, { wide: string; narrow: string }> = {
		sm: { wide: 'text-sm', narrow: 'text-base' },
		md: { wide: 'text-xl', narrow: 'text-2xl' },
		lg: { wide: 'text-3xl', narrow: 'text-4xl' }
	};
</script>

<span
	class="inline-flex items-center justify-center rounded-2xl font-display font-extralight tracking-tight {toneClasses[
		tone
	]} {sizeClasses[size]} {isWide ? fontClasses[size].wide : fontClasses[size].narrow} {className}"
	aria-label={`Grade ${grade}`}
>
	{grade}
</span>
