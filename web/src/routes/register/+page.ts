import { browser } from '$app/env';
import { getSetupGrant, rememberSetupGrant } from '../../lib/setup-grant.svelte.js';
import type { PageLoad } from './$types';

export const load: PageLoad = () => {
	if (browser) {
		const grant = window.location.hash.slice(1);
		if (grant) rememberSetupGrant(grant);
	}

	return { hasGrant: Boolean(getSetupGrant()) };
};
