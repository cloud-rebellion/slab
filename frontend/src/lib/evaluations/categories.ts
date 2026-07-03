// Single source of truth for the eval category list + display labels.
// Mirrors the category order internal/eval/eval.go persists. The list
// used to be hardcoded independently on the build page, the eval detail
// page, and ScoreHistoryChart; a category added in one place silently
// vanished from the others.
export type CategoryKey = 'security' | 'seo' | 'performance' | 'accessibility' | 'privacy';

export const CATEGORIES: readonly CategoryKey[] = [
	'security',
	'seo',
	'performance',
	'accessibility',
	'privacy'
] as const;

export const CATEGORY_LABEL: Record<CategoryKey, string> = {
	security: 'Security',
	seo: 'SEO',
	performance: 'Performance',
	accessibility: 'Accessibility',
	privacy: 'Privacy'
};
