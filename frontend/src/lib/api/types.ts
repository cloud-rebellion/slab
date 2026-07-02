export type BoolInt = 0 | 1;
export type Timestamp = string;
export type JsonText = string;

export interface User {
	id: string;
	email: string;
	name: string;
	role: string;
	created_at: Timestamp;
	updated_at: Timestamp;
}

export interface AuthUser {
	id: string;
	email: string;
	name: string;
	role: string;
	created_at?: Timestamp;
	updated_at?: Timestamp;
}

export interface Site {
	id: string;
	name: string;
	slug: string;
	domain: string;
	status: string;
	primary_color: string;
	secondary_color: string;
	bg_color: string;
	text_color: string;
	// Phase 12.9 colour slots.
	surface_color: string;
	border_color: string;
	muted_color: string;
	accent_color: string;
	on_primary_color: string;
	font_heading: string;
	font_body: string;
	meta_title: string;
	meta_description: string;
	og_image_id: string;
	favicon_id: string;
	ga4_id: string;
	umami_id: string;
	umami_url: string;
	cookieproof_domain: string;
	lang: string;
	last_build_at: Timestamp;
	last_build_status: string;
	last_build_error: string;
	last_deploy_at: Timestamp;
	created_at: Timestamp;
	updated_at: Timestamp;
}

export interface Page {
	id: string;
	site_id: string;
	title: string;
	slug: string;
	status: string;
	meta_title: string;
	meta_description: string;
	og_image_id: string;
	layout: string;
	sort_order: number;
	show_in_nav: BoolInt;
	nav_label: string;
	no_index: BoolInt;
	canonical_url: string;
	hide_global_blocks: BoolInt;
	created_at: Timestamp;
	updated_at: Timestamp;
}

export interface Block {
	id: string;
	page_id: string;
	block_type: string;
	name: string;
	sort_order: number;
	data_json: JsonText;
	style_json: JsonText;
	is_visible: BoolInt;
	template_version: number;
	created_at: Timestamp;
	updated_at: Timestamp;
}

export type BlockFieldKind =
	| 'text'
	| 'textarea'
	| 'richtext'
	| 'url'
	| 'image_id'
	| 'select'
	| 'bool'
	| 'number'
	| 'array'
	| 'object';

export interface BlockSchemaField {
	key: string;
	label: string;
	kind: BlockFieldKind;
	placeholder?: string;
	help?: string;
	required?: boolean;
	options?: { value: string; label: string }[];
	item_schema?: BlockSchemaField[];
	fields?: BlockSchemaField[];
}

export interface BlockSchema {
	type: string;
	label: string;
	description: string;
	category: string;
	fields: BlockSchemaField[];
}

export interface BlockPreviewHTML {
	block_id: string;
	block_type: string;
	html: string;
	css: string;
	font_face: string;
}

export interface HeroBlockData {
	headline?: string;
	subheadline?: string;
	cta_label?: string;
	cta_href?: string;
	image_id?: string;
	alignment?: 'left' | 'center' | 'right';
}

export interface TextBlockData {
	content?: string;
	html?: string;
	alignment?: 'left' | 'center' | 'right';
}

export interface ImageBlockData {
	image_id?: string;
	alt?: string;
	caption?: string;
	width?: number;
	height?: number;
}

export interface CtaBlockData {
	headline?: string;
	body?: string;
	primary_label?: string;
	primary_href?: string;
	secondary_label?: string;
	secondary_href?: string;
}

export interface FeatureGridItem {
	icon?: string;
	title?: string;
	body?: string;
	href?: string;
}

export interface FeatureGridBlockData {
	headline?: string;
	subheadline?: string;
	columns?: number;
	items?: FeatureGridItem[];
}

export interface BlockDataMap {
	hero: HeroBlockData;
	text: TextBlockData;
	image: ImageBlockData;
	cta: CtaBlockData;
	feature_grid: FeatureGridBlockData;
}

export type BlockData = BlockDataMap[keyof BlockDataMap] | Record<string, unknown>;

export interface BlockStyle {
	bg_color?: string;
	text_color?: string;
	padding_y?: string;
	padding_x?: string;
	max_width?: string;
	[k: string]: unknown;
}

export interface GlobalBlock {
	id: string;
	site_id: string;
	name: string;
	slot: string;
	block_type: string;
	data_json: JsonText;
	style_json: JsonText;
	is_active: BoolInt;
	sort_order: number;
	template_version: number;
	created_at: Timestamp;
	updated_at: Timestamp;
}

export interface Component {
	id: string;
	site_id: string;
	name: string;
	category: string;
	template: string;
	props_schema: JsonText;
	css_classes: JsonText;
	usage_note: string;
	created_at: Timestamp;
	updated_at: Timestamp;
}

export interface CssClass {
	id: string;
	site_id: string;
	name: string;
	category: string;
	css: string;
	usage_note: string;
	sort_order: number;
	created_at: Timestamp;
	updated_at: Timestamp;
}

export interface KnowledgebaseEntry {
	id: string;
	site_id: string;
	category: string;
	title: string;
	content: string;
	is_active: BoolInt;
	sort_order: number;
	created_at: Timestamp;
	updated_at: Timestamp;
}

export interface GuardrailRule {
	id: string;
	site_id: string;
	rule_type: string;
	target: string;
	value: string;
	severity: string;
	is_active: BoolInt;
	created_at: Timestamp;
}

export interface MediaVariant {
	name: string;
	path: string;
	width: number;
	height: number;
	format: string;
	size?: number;
}

export interface Medium {
	id: string;
	site_id: string;
	filename: string;
	alt_text: string;
	mime_type: string;
	file_size: number;
	width: number;
	height: number;
	blurhash: string;
	variants_json: JsonText;
	original_path: string;
	folder: string;
	focal_x: number;
	focal_y: number;
	created_at: Timestamp;
	updated_at: Timestamp;
}

export interface MediaFolder {
	name: string;
	is_system: boolean;
	created_at: Timestamp;
	item_count: number;
}

export interface MediaFolderListResponse {
	folders: MediaFolder[];
	total_count: number;
	unfiled_count: number;
}

export interface AgentKey {
	id: string;
	site_id: string;
	name: string;
	key_hash: string;
	capabilities: JsonText;
	is_active: BoolInt;
	last_used_at: Timestamp;
	created_at: Timestamp;
}

export type AllowedScriptKind = 'script' | 'frame' | 'image' | 'media' | 'connect' | 'all';

export interface AllowedScript {
	id: string;
	site_id: string;
	domain: string;
	purpose: string;
	kind: AllowedScriptKind;
	is_active: BoolInt;
	created_at: Timestamp;
}

export interface Deployment {
	id: string;
	site_id: string;
	status: string;
	build_log: string;
	deploy_target: string;
	deploy_config: string;
	pages_built: number;
	duration_ms: number;
	error: string;
	target_id?: string;
	deploy_url?: string;
	deployed_at?: Timestamp;
	created_at: Timestamp;
	completed_at: Timestamp;
}

export type DeployKind = 'local' | 'rsync' | 'dockyard';

export interface DeployTargetLocalConfig {
	path?: string;
	public_url?: string;
}

export interface DeployTargetRsyncConfig {
	host?: string;
	user?: string;
	port?: number;
	path?: string;
	public_url?: string;
	private_key_pem?: string;
}

export interface DeployTargetDockyardConfig {
	dockyard_url?: string;
	api_key?: string;
	server_id?: string;
	domain?: string;
	host?: string;
	user?: string;
	port?: number;
	host_path?: string;
	private_key_pem?: string;
}

export type DeployTargetConfig =
	| DeployTargetLocalConfig
	| DeployTargetRsyncConfig
	| DeployTargetDockyardConfig
	| Record<string, unknown>;

export interface DeployTarget {
	id: string;
	site_id: string;
	name: string;
	kind: DeployKind;
	config: DeployTargetConfig;
	is_default: boolean;
	created_at: Timestamp;
	updated_at: Timestamp;
}

export interface DeployResult {
	deploy_url: string;
	deployment_id: string;
}

export interface Evaluation {
	id: string;
	build_id: string;
	site_id: string;
	category: string;
	score: number;
	max_score: number;
	grade: string;
	checks_json: JsonText;
	/** design.fidelity active when this row was graded (performance |
	 * balanced | showcase). Empty for rows graded before the dial
	 * existed (balanced semantics). */
	profile?: string;
	created_at: Timestamp;
}

export interface EvaluationCheck {
	id?: string;
	name: string;
	// null means informational (no scoring impact). The Go eval package
	// emits Pass / Fail / Info checks; preserving null in the union keeps
	// the UI able to group informational rows separately from failures.
	passed: boolean | null;
	score?: number;
	max_score?: number;
	severity?: 'info' | 'warning' | 'error' | 'critical';
	message?: string;
	details?: string;
	url?: string;
}

export interface SiteProfile {
	id: string;
	site_id: string;
	business_name: string;
	registration_nr: string;
	country: string;
	contact_email: string;
	contact_phone: string;
	privacy_email: string;
	security_email: string;
	address_line1: string;
	address_line2: string;
	city: string;
	postal_code: string;
	created_at: Timestamp;
	updated_at: Timestamp;
}

export interface SiteArchitecture {
	id: string;
	site_id: string;
	structure_type: string;
	max_depth: number;
	created_at: Timestamp;
	updated_at: Timestamp;
}

export interface SiteSilo {
	id: string;
	site_id: string;
	name: string;
	slug_prefix: string;
	sort_order: number;
	created_at: Timestamp;
}

export interface SiteSetting {
	id: string;
	site_id: string;
	category: string;
	key: string;
	value: string;
	updated_at: Timestamp;
}

export interface Form {
	id: string;
	site_id: string;
	name: string;
	fields_json: JsonText;
	action: string;
	action_config: string;
	created_at: Timestamp;
	updated_at: Timestamp;
}

export interface FormSubmission {
	id: string;
	form_id: string;
	site_id: string;
	data_json: JsonText;
	ip_hash: string;
	created_at: Timestamp;
}

export interface Redirect {
	id: string;
	site_id: string;
	from_path: string;
	to_path: string;
	status_code: number;
	is_auto: BoolInt;
	created_at: Timestamp;
}

export interface ListResponse<T> {
	items: T[];
	total: number;
	limit: number;
	offset: number;
}
