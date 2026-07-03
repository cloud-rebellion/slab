import { apiGet, apiPatch, apiPost } from './client';
import type { AuthUser } from './types';

export interface AuthResponse {
	user: AuthUser;
	/**
	 * Set to true by login + /auth/me when the deployment's MFA
	 * enforcement policy demands enrollment for this user's role and
	 * the user has not yet enrolled in TOTP. The session cookie is
	 * still issued (so the user can complete enrollment); writes to
	 * everything except the TOTP setup + change-password + logout
	 * paths return 403 + error_code=mfa_enroll_required until the
	 * user enrolls.
	 */
	enroll_required?: boolean;
}

export interface StatusResponse {
	status: string;
}

export function login(email: string, password: string, totpCode?: string): Promise<AuthResponse> {
	const body: Record<string, string> = { email, password };
	if (totpCode) body.totp_code = totpCode;
	return apiPost<AuthResponse>('/auth/login', body);
}

export function logout(): Promise<StatusResponse> {
	return apiPost<StatusResponse>('/auth/logout');
}

export function me(): Promise<AuthResponse> {
	return apiGet<AuthResponse>('/auth/me');
}

export function changePassword(currentPassword: string, newPassword: string): Promise<StatusResponse> {
	return apiPost<StatusResponse>('/auth/change-password', {
		current_password: currentPassword,
		new_password: newPassword
	});
}

export function updateProfile(name: string): Promise<AuthResponse> {
	return apiPatch<AuthResponse>('/auth/me', { name });
}

export function signOutEverywhere(): Promise<StatusResponse> {
	return apiPost<StatusResponse>('/auth/sign-out-everywhere');
}

export interface InviteInfo {
	email: string;
	role: string;
}

export function inviteInfo(token: string): Promise<InviteInfo> {
	return apiGet<InviteInfo>(`/auth/signup/${token}`);
}

export function redeemInvite(token: string, name: string, password: string): Promise<AuthResponse> {
	return apiPost<AuthResponse>(`/auth/signup/${token}`, { name, password });
}

// Forgot-password flow. Always returns 200 (the bare ack defeats
// account enumeration). Operator log surfaces the link when no
// MailSender is wired.
export interface ForgotPasswordResponse {
	status: string;
	message?: string;
}

export function forgotPassword(email: string): Promise<ForgotPasswordResponse> {
	return apiPost<ForgotPasswordResponse>('/auth/forgot-password', { email });
}

export interface ResetTokenInfo {
	email: string; // masked, e.g. a***@example.com
}

export function resetPasswordInfo(token: string): Promise<ResetTokenInfo> {
	return apiGet<ResetTokenInfo>(`/auth/reset-password/${token}`);
}

export function resetPassword(token: string, password: string): Promise<StatusResponse> {
	return apiPost<StatusResponse>(`/auth/reset-password/${token}`, { password });
}

export interface TotpStatus {
	enrolled: boolean;
	enrolled_at: string;
	has_staged: boolean;
}

export interface TotpSetupResponse {
	secret: string;
	otpauth_uri: string;
	hint: string;
}

export interface TotpVerifyResponse {
	status: string;
	recovery_codes: string[];
	hint: string;
}

export function totpStatus(): Promise<TotpStatus> {
	return apiGet<TotpStatus>('/auth/totp/status');
}

export function totpSetup(): Promise<TotpSetupResponse> {
	return apiPost<TotpSetupResponse>('/auth/totp/setup');
}

export function totpVerify(code: string): Promise<TotpVerifyResponse> {
	return apiPost<TotpVerifyResponse>('/auth/totp/verify', { code });
}

export function totpDisable(currentPassword: string): Promise<StatusResponse> {
	return apiPost<StatusResponse>('/auth/totp/disable', { current_password: currentPassword });
}

// Workspace invite redemption (the /cloud/signup/{token} flow). Unlike
// platform invites, the invitee may already have an account: the info
// payload says so, and redeeming then grants the membership without a
// session (the invitee logs in normally).
export interface WorkspaceInviteInfo {
	email: string;
	role: string;
	workspace_name: string;
	existing_account: boolean;
}

export interface WorkspaceRedeemResponse {
	status: 'account_created' | 'membership_added';
	existing_account: boolean;
	message?: string;
	user?: AuthResponse['user'];
}

export function workspaceInviteInfo(token: string): Promise<WorkspaceInviteInfo> {
	return apiGet<WorkspaceInviteInfo>(`/cloud/signup/${token}`);
}

export function redeemWorkspaceInvite(
	token: string,
	name: string,
	password: string
): Promise<WorkspaceRedeemResponse> {
	return apiPost<WorkspaceRedeemResponse>(`/cloud/signup/${token}`, { name, password });
}
