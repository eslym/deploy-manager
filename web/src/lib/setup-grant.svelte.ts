export const setupGrant = $state({ value: '', ready: false, sanitizing: false });

export function rememberSetupGrant(grant: string) {
	setupGrant.value = grant;
	setupGrant.ready = false;
	setupGrant.sanitizing = false;
}

export function getSetupGrant() {
	return setupGrant.value;
}

export function markSetupGrantReady() {
	setupGrant.ready = true;
	setupGrant.sanitizing = false;
}

export function beginSetupGrantSanitizing() {
	if (setupGrant.sanitizing) return false;
	setupGrant.sanitizing = true;
	return true;
}

export function clearSetupGrant() {
	setupGrant.value = '';
	setupGrant.ready = false;
	setupGrant.sanitizing = false;
}
