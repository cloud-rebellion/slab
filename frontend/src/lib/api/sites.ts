import { apiDelete, apiGet, apiPatch, apiPost } from './client';
import type { Site } from './types';

export interface SitesListResponse {
	sites: Site[];
}

export interface CreateSiteInput {
	name: string;
	slug: string;
	domain?: string;
	primary_color?: string;
	secondary_color?: string;
	bg_color?: string;
	text_color?: string;
	font_heading?: string;
	font_body?: string;
	lang?: string;
}

export interface UpdateSitePatch {
	name?: string;
	slug?: string;
	domain?: string;
	status?: string;
	primary_color?: string;
	secondary_color?: string;
	bg_color?: string;
	text_color?: string;
	// Phase 12.9 colour slots (surface, border, muted, accent, on_primary).
	surface_color?: string;
	border_color?: string;
	muted_color?: string;
	accent_color?: string;
	on_primary_color?: string;
	font_heading?: string;
	font_body?: string;
	meta_title?: string;
	meta_description?: string;
	og_image_id?: string;
	favicon_id?: string;
	ga4_id?: string;
	umami_id?: string;
	umami_url?: string;
	cookieproof_domain?: string;
	lang?: string;
}

export function list(): Promise<SitesListResponse> {
	return apiGet<SitesListResponse>('/sites');
}

export function get(siteID: string): Promise<Site> {
	return apiGet<Site>(`/sites/${siteID}`);
}

export function create(input: CreateSiteInput): Promise<Site> {
	return apiPost<Site>('/sites', input);
}

export function update(siteID: string, patch: UpdateSitePatch): Promise<Site> {
	return apiPatch<Site>(`/sites/${siteID}`, patch);
}

export function remove(siteID: string): Promise<{ status: string }> {
	return apiDelete<{ status: string }>(`/sites/${siteID}`);
}

export type SiteType =
	| 'b2b'
	| 'b2c'
	| 'personal'
	| 'blog'
	| 'agency'
	| 'one-pager'
	| 'ecommerce';
export type StructureType = 'one-pager' | 'soft-silo' | 'hard-silo';

// Update site patch type. Phase 12.9 added 5 colour slots (surface, border,
// muted, accent, on_primary) to the site row; the Branding page exposes
// them all so the patch type covers them too.

export interface UpdateSitePatchExtra {
	surface_color?: string;
	border_color?: string;
	muted_color?: string;
	accent_color?: string;
	on_primary_color?: string;
}

export interface SeedSiteInfo {
	name: string;
	slug: string;
	domain?: string;
	lang: string;
	business_name: string;
	contact_email: string;
	country: string;
}

export interface SeedSiteStructure {
	structure_type: StructureType;
	max_depth: number;
}

export type SiloType = 'inherit' | 'soft' | 'hard';

export interface SeedSiteSilo {
	name: string;
	slug_prefix: string;
	silo_type?: SiloType;
}

export interface SeedSiteBranding {
	primary_color: string;
	secondary_color: string;
	bg_color: string;
	text_color: string;
	font_heading: string;
	font_body: string;
}

export interface SeedSiteRequest {
	// Top-level, matching seedSiteRequest in internal/handlers/sites.go: NOT
	// nested inside `info`. Optional: omitted means the caller's oldest
	// workspace, which is what a single-workspace operator always wants.
	workspace_id?: string;
	type: SiteType;
	info: SeedSiteInfo;
	structure: SeedSiteStructure;
	silos: SeedSiteSilo[];
	branding: SeedSiteBranding;
	starter_kit?: string;
}

export interface SeedSiteResponse {
	site_id: string;
}

export function seed(input: SeedSiteRequest): Promise<SeedSiteResponse> {
	return apiPost<SeedSiteResponse>('/sites/seed', input);
}
