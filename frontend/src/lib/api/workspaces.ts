import { apiDelete, apiGet, apiPatch, apiPost } from './client';

// Workspace roles are a SEPARATE vocabulary from the platform roles in
// members.ts ('admin' | 'editor'). A user can be a platform editor and a
// workspace owner at the same time; the two never map onto each other.
export type WorkspaceRole = 'owner' | 'admin' | 'member';

export interface Workspace {
	id: string;
	name: string;
	slug: string;
	plan: string;
	region: string;
	billing_email: string;
	status: string;
	trial_ends_at: string;
	stripe_customer_id?: string;
	stripe_subscription_id?: string;
	created_at: string;
	updated_at: string;
	// The caller's own role in this workspace. Present on list/get/create.
	role?: WorkspaceRole;
}

// The member row is keyed on user_id: ListWorkspaceMembers joins users and
// has no `id` column of its own, unlike the platform /admin/members payload.
// It also has no server-side is_self flag, so callers compare against the
// authenticated user themselves.
export interface WorkspaceMember {
	workspace_id: string;
	user_id: string;
	role: WorkspaceRole;
	created_at: string;
	email: string;
	name: string;
}

// The pending-invite row deliberately carries NO token or url. The redeemable
// token is returned exactly once, by createInvite, and is never serialised on
// the list path (pinned by TestListInvites_MemberDeniedAndTokenNeverSerialised).
// So there can be no per-row "copy link" affordance: if the link is lost, the
// invite is revoked and re-issued.
export interface WorkspaceInvite {
	id: string;
	workspace_id: string;
	email: string;
	role: WorkspaceRole;
	created_by: string;
	created_at: string;
	expires_at: string;
}

// NOT a WorkspaceInvite. The create response is its own smaller shape: the
// handler returns id/workspace_id/email/role plus the one-time token and url,
// and deliberately no created_at/created_by/expires_at. Declaring it as an
// extension of the list row makes those fields look present when they are
// undefined, which crashes any render that touches them.
export interface CreatedWorkspaceInvite {
	id: string;
	workspace_id: string;
	email: string;
	role: WorkspaceRole;
	token: string;
	url: string;
}

export interface WorkspaceSite {
	id: string;
	workspace_id: string;
	name: string;
	slug: string;
	status: string;
	created_at: string;
	updated_at: string;
}

export interface CreateWorkspaceInput {
	name: string;
	slug?: string;
	// Only a platform admin may name a plan; the server refuses it from
	// anyone else rather than silently downgrading, so the UI only sends it
	// when it knows the caller is one.
	plan?: string;
	billing_email?: string;
}

export interface UpdateWorkspacePatch {
	name?: string;
	billing_email?: string;
}

export function list(): Promise<{ workspaces: Workspace[] }> {
	return apiGet<{ workspaces: Workspace[] }>('/workspaces');
}

export function get(workspaceID: string): Promise<Workspace> {
	return apiGet<Workspace>(`/workspaces/${workspaceID}`);
}

export function create(input: CreateWorkspaceInput): Promise<Workspace> {
	return apiPost<Workspace>('/workspaces', input);
}

export function update(workspaceID: string, patch: UpdateWorkspacePatch): Promise<Workspace> {
	return apiPatch<Workspace>(`/workspaces/${workspaceID}`, patch);
}

export function remove(workspaceID: string): Promise<{ status: string }> {
	return apiDelete<{ status: string }>(`/workspaces/${workspaceID}`);
}

export function listMembers(workspaceID: string): Promise<{ members: WorkspaceMember[] }> {
	return apiGet<{ members: WorkspaceMember[] }>(`/workspaces/${workspaceID}/members`);
}

export function updateMemberRole(
	workspaceID: string,
	userID: string,
	role: WorkspaceRole
): Promise<{ status: string; role: WorkspaceRole }> {
	return apiPatch<{ status: string; role: WorkspaceRole }>(
		`/workspaces/${workspaceID}/members/${userID}`,
		{ role }
	);
}

export function removeMember(workspaceID: string, userID: string): Promise<{ status: string }> {
	return apiDelete<{ status: string }>(`/workspaces/${workspaceID}/members/${userID}`);
}

export function listInvites(workspaceID: string): Promise<{ invites: WorkspaceInvite[] }> {
	return apiGet<{ invites: WorkspaceInvite[] }>(`/workspaces/${workspaceID}/invites`);
}

export function createInvite(
	workspaceID: string,
	email: string,
	role: WorkspaceRole
): Promise<CreatedWorkspaceInvite> {
	return apiPost<CreatedWorkspaceInvite>(`/workspaces/${workspaceID}/invites`, { email, role });
}

export function deleteInvite(workspaceID: string, inviteID: string): Promise<{ status: string }> {
	return apiDelete<{ status: string }>(`/workspaces/${workspaceID}/invites/${inviteID}`);
}

export function listSites(workspaceID: string): Promise<{ sites: WorkspaceSite[] }> {
	return apiGet<{ sites: WorkspaceSite[] }>(`/workspaces/${workspaceID}/sites`);
}
