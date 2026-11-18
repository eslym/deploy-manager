import { browser } from '$app/env';
import { createRegistrationFlow } from '../../lib/registration-flow.svelte.js';
import type { PageLoad } from './$types';

export const load: PageLoad = () => {
	const flow = createRegistrationFlow();
	if (browser) {
		const grant = window.location.hash.slice(1);
		flow.intakeGrant(grant);
	}

	return { hasGrant: flow.hasGrant() };
};
