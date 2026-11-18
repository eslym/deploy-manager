import { browser } from '$app/env';
import { createCredentialFlow } from '../../lib/credential-flow.svelte.js';
import type { PageLoad } from './$types';

export const load: PageLoad = () => {
	const flow = createCredentialFlow('recovery');
	if (browser) {
		flow.intakeGrant(window.location.hash.slice(1));
	}

	return { hasGrant: flow.hasGrant() };
};
