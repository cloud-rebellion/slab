import { apiGet } from './client';

// Sprint 5 quick-win (2026-05-24): Core Web Vitals dashboard API.

export interface CWVMetricResult {
	metric: 'LCP' | 'INP' | 'CLS' | 'FCP' | 'TTFB';
	device: '' | 'desktop' | 'mobile' | 'tablet';
	p75: number;
	samples: number;
	rating: '' | 'good' | 'needs-improvement' | 'poor';
	bad_count: number;
	good_count: number;
}

export interface CWVOverview {
	site_id: string;
	since: '1d' | '7d' | '30d';
	metrics: CWVMetricResult[];
}

export function getCWVOverview(siteID: string, since: string = '7d'): Promise<CWVOverview> {
	return apiGet(`/sites/${siteID}/analytics/cwv?since=${encodeURIComponent(since)}`);
}
