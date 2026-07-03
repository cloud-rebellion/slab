import { test, expect } from '../../fixtures/auth';
import { createSite, deleteSite, type Site, u } from '../../fixtures/data';

// 2026-05-22: Sprint 2 slice A catalog (products + variants + inventory + discount codes).
//
// Deeper validation (slug regex, status whitelist, percent value cap,
// cross-tenant guards) is covered by internal/handlers/products_test.go.
// This spec walks the dashboard happy path the operator will hit.

test.describe('Store catalog admin', () => {
	let site: Site;

	test.beforeAll(async ({ adminApi }) => {
		site = await createSite(adminApi);
	});

	test.afterAll(async ({ adminApi }) => {
		if (site) await deleteSite(adminApi, site.id);
	});

	test('empty products list renders empty state', async ({ adminApi, loggedInPage }) => {
		await loggedInPage.goto(`/sites/${site.id}/store/products`);
		await expect(loggedInPage.getByText('No products yet')).toBeVisible({ timeout: 5_000 });
		const resp = await adminApi.get(u(`/api/sites/${site.id}/products`));
		expect(resp.status()).toBe(200);
		const body = await resp.json();
		expect(body.count).toBe(0);
	});

	test('product CRUD via API + UI lands in dashboard', async ({ adminApi, loggedInPage }) => {
		const create = await adminApi.post(u(`/api/sites/${site.id}/products`), {
			data: { name: 'Hoodie', slug: 'hoodie', status: 'active', base_price_cents: 7995, currency: 'EUR' }
		});
		expect(create.status()).toBe(201);
		const product = await create.json();
		expect(product.name).toBe('Hoodie');
		expect(product.status).toBe('active');

		await loggedInPage.goto(`/sites/${site.id}/store/products`);
		// The product name renders in more than one place on the list row, so
		// scope to the first match to avoid a strict-mode violation.
		await expect(loggedInPage.getByText('Hoodie').first()).toBeVisible({ timeout: 5_000 });
	});

	test('variant inventory adjustment writes audit row', async ({ adminApi }) => {
		const p = await (
			await adminApi.post(u(`/api/sites/${site.id}/products`), {
				data: { name: 'T-Shirt', slug: 't-shirt', status: 'active', base_price_cents: 2500 }
			})
		).json();
		const v = await (
			await adminApi.post(u(`/api/sites/${site.id}/products/${p.id}/variants`), {
				data: { name: 'Small', sku: 'TS-S', price_cents: 2500, inventory_count: 20 }
			})
		).json();
		const adj = await adminApi.post(u(`/api/sites/${site.id}/variants/${v.id}/inventory`), {
			data: { delta: -3, reason: 'sale', note: 'pos sale' }
		});
		expect(adj.status()).toBe(200);
		const adjBody = await adj.json();
		expect(adjBody.delta).toBe(-3);

		const list = await adminApi.get(u(`/api/sites/${site.id}/variants/${v.id}/inventory`));
		const body = await list.json();
		expect(body.count).toBe(1);
		expect(body.adjustments[0].reason).toBe('sale');
		expect(body.adjustments[0].new_count).toBe(17);
	});

	test('discount code create + duplicate rejection', async ({ adminApi, loggedInPage }) => {
		const r1 = await adminApi.post(u(`/api/sites/${site.id}/discount-codes`), {
			data: { code: 'WELCOME10', kind: 'percent', value: 1000, is_active: true }
		});
		expect(r1.status()).toBe(201);

		const r2 = await adminApi.post(u(`/api/sites/${site.id}/discount-codes`), {
			data: { code: 'WELCOME10', kind: 'percent', value: 1500 }
		});
		expect(r2.status()).toBeGreaterThanOrEqual(400);

		await loggedInPage.goto(`/sites/${site.id}/store/discount-codes`);
		await expect(loggedInPage.getByText('WELCOME10')).toBeVisible({ timeout: 5_000 });
	});

	test('store SectionNav tab visible on any sites page', async ({ loggedInPage }) => {
		await loggedInPage.goto(`/sites/${site.id}/pages`);
		await expect(loggedInPage.getByRole('link', { name: 'Store' })).toBeVisible();
	});
});
