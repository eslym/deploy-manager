import { browser } from '$app/env';
import { createCredentialFlow } from '../../lib/credential-flow.svelte.js';
import type { PageLoad } from './$types';

export const load: PageLoad = () => {
	const flow = createCredentialFlow('registration');
	if (browser) {
		const grant = window.location.hash.slice(1);
		flow.intakeGrant(grant);
	}

	return { hasGrant: flow.hasGrant() };
};
