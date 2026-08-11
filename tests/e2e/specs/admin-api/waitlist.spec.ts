import { test, expect, u } from '../../fixtures/auth';

// Phase 30.5: waitlist (public submit + admin review).
// MAILERSEND_API_TOKEN is unset in the test harness, so the invite
// endpoint mints the token + returns the URL but skips actual email.
// That's the documented degrade-mode for OSS / local dev.

test.describe('Phase 30.5: waitlist', () => {
	// anonApi, not adminApi: this endpoint is posted by an anonymous
	// visitor's browser, so the only test that means anything is one
	// without a session cookie. These two ran on adminApi for months and
	// stayed green while every real submission got 401.
	test('public submit accepts valid email + returns 201', async ({ anonApi }) => {
		const email = `wl-${Date.now()}@example.test`;
		const res = await anonApi.post(u('/api/waitlist'), {
			data: { email, name: 'Test User', use_case: 'Marketing site for my law firm', region: 'eu' }
		});
		expect(res.status(), 'a cookie-less submit must not be rejected by auth').not.toBe(401);
		expect(res.status()).toBe(201);
		const body = await res.json();
		expect(body.status).toBe('ok');
	});

	test('public submit rejects malformed email with 400', async ({ anonApi }) => {
		const res = await anonApi.post(u('/api/waitlist'), {
			data: { email: 'not-an-email', name: 'X' }
		});
		// 400 not 401: the request reached the handler and the handler
		// judged the payload. A 401 here would mean the route moved back
		// inside the authenticated group.
		expect(res.status()).toBe(400);
	});

	test('admin can list waitlist + invite an entry', async ({ adminApi }) => {
		const email = `wl-admin-${Date.now()}@example.test`;
		const sub = await adminApi.post(u('/api/waitlist'), {
			data: { email, name: 'Admin Probe', use_case: 'Test' }
		});
		expect(sub.ok()).toBeTruthy();

		const list = await adminApi.get(u('/api/admin/waitlist'));
		expect(list.ok()).toBeTruthy();
		const body = await list.json();
		const entry = (body.waitlist as Array<{ email: string; id: string }>).find((w) => w.email === email);
		expect(entry, 'submitted entry should appear in admin list').toBeTruthy();

		const invite = await adminApi.post(u(`/api/admin/waitlist/${entry!.id}/invite`));
		expect(invite.ok()).toBeTruthy();
		const inviteBody = await invite.json();
		// MailerSend not configured in the test harness, so we get a
		// status of "invited" with the URL (or "invited-mail-failed"
		// if a partially-configured mailer fails). Both indicate the
		// row was marked invited successfully.
		expect(['invited', 'invited-mail-failed']).toContain(inviteBody.status);
		expect(inviteBody.token).toMatch(/^[a-f0-9]{48}$/);
		expect(inviteBody.url).toContain('/cloud/signup/');
	});

	test('repeat submit for same email is idempotent', async ({ adminApi }) => {
		const email = `wl-dup-${Date.now()}@example.test`;
		const first = await adminApi.post(u('/api/waitlist'), {
			data: { email, name: 'Dupe' }
		});
		expect(first.ok()).toBeTruthy();
		const second = await adminApi.post(u('/api/waitlist'), {
			data: { email, name: 'Dupe again' }
		});
		// Either 201 (insert-or-ignore creates a no-op duplicate
		// safely) or 409 if a future tightening rejects re-submits.
		// 201 is the current behavior.
		expect([200, 201, 409]).toContain(second.status());
	});
});
