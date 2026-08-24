import type { Workspace } from '$lib/api/workspaces';

const STORAGE_KEY = 'atomicsite_last_workspace_id';

// The active workspace is a CLIENT-side concept only. There is no
// server-side "current workspace": site access is derived from membership
// on every request, so this store decides what the UI shows by default and
// which workspace a new site is created in, never what the API permits.
let active = $state<Workspace | null>(null);
let all = $state<Workspace[]>([]);

export const currentWorkspace = {
	get value(): Workspace | null {
		return active;
	},
	// The full list the switcher and the site wizard share, so neither has
	// to re-fetch on open.
	get all(): Workspace[] {
		return all;
	}
};

function persistID(id: string | null): void {
	if (typeof window === 'undefined') return;
	try {
		if (id) {
			window.localStorage.setItem(STORAGE_KEY, id);
		} else {
			window.localStorage.removeItem(STORAGE_KEY);
		}
	} catch {
		// localStorage may be blocked; ignore.
	}
}

export function setWorkspace(workspace: Workspace): void {
	active = workspace;
	persistID(workspace.id);
}

export function clearWorkspace(): void {
	active = null;
	persistID(null);
}

export function lastWorkspaceID(): string | null {
	if (typeof window === 'undefined') return null;
	try {
		return window.localStorage.getItem(STORAGE_KEY);
	} catch {
		return null;
	}
}

// setWorkspaces refreshes the shared list and re-resolves the active one:
// the remembered workspace if it is still in the list, otherwise the first.
// A workspace that was deleted or that the user was removed from therefore
// stops being active on the next load rather than lingering as a stale
// selection that 404s.
export function setWorkspaces(workspaces: Workspace[]): void {
	all = workspaces;
	if (workspaces.length === 0) {
		clearWorkspace();
		return;
	}
	const remembered = lastWorkspaceID();
	const match = remembered ? workspaces.find((w) => w.id === remembered) : undefined;
	const next = match ?? workspaces[0];
	if (next) setWorkspace(next);
}

// upsertWorkspace keeps the shared list in step after a create or a rename
// without a refetch, matching how the list pages mutate locally.
export function upsertWorkspace(workspace: Workspace): void {
	const idx = all.findIndex((w) => w.id === workspace.id);
	all = idx === -1 ? [...all, workspace] : all.map((w) => (w.id === workspace.id ? workspace : w));
	if (active?.id === workspace.id) {
		active = workspace;
	}
}

export function removeWorkspaceFromList(workspaceID: string): void {
	all = all.filter((w) => w.id !== workspaceID);
	if (active?.id === workspaceID) {
		const next = all[0];
		if (next) {
			setWorkspace(next);
		} else {
			clearWorkspace();
		}
	}
}
