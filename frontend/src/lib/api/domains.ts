import { apiDelete, apiGet, apiPost } from './client';

// Mirrors store.SiteDomain from the Go side. Status moves through:
//
//   pending -> verified -> cert_ready -> live
//                                     \-> error
//
// The admin UI shows status, last_check_at, and last_error verbatim
// so editors can self-debug DNS misconfigs without opening logs.
export interface SiteDomain {
	id: string;
	site_id: string;
	hostname: string;
	status: 'pending' | 'verified' | 'cert_ready' | 'live' | 'error';
	verify_token: string;
	cert_path: string;
	last_check_at: string;
	last_error: string;
	is_canonical: 0 | 1;
	created_at: string;
	updated_at: string;
}

export interface ListDomainsResponse {
	domains: SiteDomain[];
}

// Sprint 4.7.4 (2026-05-09): help payload the admin UI fetches once on
// mount so the "Add an A record" instructions show real values instead
// of "[your edge IP]" placeholder. edge_ip is empty when the server
// boots without ATOMICSITE_EDGE_IP (single-tenant dev runs); UI falls
// back to a "ask your operator" line in that case.
export interface DomainsHelp {
	edge_ip: string;
	cloudflare_zones: string[];
	verify_url_template: string;
	/**
	 * TXT record template a hostname inside cloudflare_zones must carry before
	 * we touch DNS for it. In those zones we create the A record ourselves, so
	 * the HTTP probe would only be verifying our own handiwork; a TXT record
	 * can only be set by whoever already holds the name.
	 */
	dns_proof_name_template: string;
	dns_proof_value: string;
}

export function list(siteID: string): Promise<ListDomainsResponse> {
	return apiGet<ListDomainsResponse>(`/sites/${siteID}/domains`);
}

export function help(siteID: string): Promise<DomainsHelp> {
	return apiGet<DomainsHelp>(`/sites/${siteID}/domains/help`);
}

export function create(
	siteID: string,
	hostname: string,
	isCanonical = false
): Promise<SiteDomain> {
	return apiPost<SiteDomain>(`/sites/${siteID}/domains`, {
		hostname,
		is_canonical: isCanonical
	});
}

export function setCanonical(siteID: string, domainID: string): Promise<SiteDomain> {
	return apiPost<SiteDomain>(`/sites/${siteID}/domains/${domainID}/canonical`, {});
}

export function refresh(siteID: string, domainID: string): Promise<{ status: string }> {
	return apiPost<{ status: string }>(`/sites/${siteID}/domains/${domainID}/refresh`, {});
}

export function remove(siteID: string, domainID: string): Promise<{ status: string }> {
	return apiDelete<{ status: string }>(`/sites/${siteID}/domains/${domainID}`);
}
