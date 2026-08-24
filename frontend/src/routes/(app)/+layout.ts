import { redirect } from '@sveltejs/kit';
import * as authApi from '$lib/api/auth';
import * as modeApi from '$lib/api/mode';
import * as workspacesApi from '$lib/api/workspaces';
import { ApiError } from '$lib/api/client';
import { setUser, setEnrollRequired } from '$lib/stores/auth.svelte';
import { setDeploymentMode } from '$lib/stores/mode.svelte';
import { setWorkspaces } from '$lib/stores/currentWorkspace.svelte';
import type { LayoutLoad } from './$types';

export const load: LayoutLoad = async () => {
	try {
		// Only me() may reject. The workspace and mode probes each swallow
		// their own failures: this loader guards EVERY authenticated page, so
		// letting a secondary call throw would turn one 500 into an app-wide
		// outage that looks like a login problem.
		const [res, workspaces, mode] = await Promise.all([
			authApi.me(),
			workspacesApi
				.list()
				.then((r) => r.workspaces ?? [])
				.catch(() => []),
			modeApi.get().catch(() => ({ mode: 'single' as const, ee: false }))
		]);
		setUser(res.user);
		setEnrollRequired(Boolean(res.enroll_required));
		setWorkspaces(workspaces);
		setDeploymentMode(mode);
		return {
			user: res.user,
			enrollRequired: Boolean(res.enroll_required),
			workspaces
		};
	} catch (err) {
		if (err instanceof ApiError && err.status === 401) {
			throw redirect(303, '/login');
		}
		throw err;
	}
};
