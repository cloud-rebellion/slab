import type { DeploymentMode } from '$lib/api/mode';

// Defaults are the SAFE reading: assume a self-hosted OSS install until the
// server says otherwise, so a failed probe hides billing UI rather than
// showing a checkout that cannot work.
let current = $state<DeploymentMode>({ mode: 'single', ee: false });

export const deploymentMode = {
	get value(): DeploymentMode {
		return current;
	},
	get isCloud(): boolean {
		return current.mode === 'cloud';
	},
	// Billing is only meaningful on a cloud build: StartCheckout answers 503
	// "Billing not configured (cloud build only)" everywhere else.
	get billingAvailable(): boolean {
		return current.mode === 'cloud' && current.ee;
	}
};

export function setDeploymentMode(next: DeploymentMode): void {
	current = next;
}
