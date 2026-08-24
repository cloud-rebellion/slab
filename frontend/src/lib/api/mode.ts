import { apiGet } from './client';

// Deployment mode, from GET /api/mode. The endpoint has existed since Phase
// 30.6 with a comment naming exactly this use, and nothing had ever called
// it. `mode` is "single" for a self-hosted install and "cloud" for the
// multi-tenant hosted product; `ee` reports whether the binary carries the
// enterprise build tag.
//
// This gates BILLING affordances only. The workspace UI itself is
// unconditional: agency mode is the open-core product, so hiding it on a
// self-hosted install would hide the feature from the people it is for.
export interface DeploymentMode {
	mode: 'single' | 'cloud';
	ee: boolean;
}

export function get(): Promise<DeploymentMode> {
	return apiGet<DeploymentMode>('/mode');
}
