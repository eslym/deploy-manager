import { browser } from '$app/env';
import { replaceState } from '$app/navigation';
import { page } from '$app/state';
import {
	beginSetupGrantSanitizing,
	clearSetupGrant,
	getSetupGrant,
	markSetupGrantReady,
	rememberSetupGrant
} from '../../lib/setup-grant.svelte.js';
import type { PageLoad } from './$types';

export const load: PageLoad = () => {
	if (browser) {
		const grant = window.location.hash.slice(1);
		if (grant) {
			rememberSetupGrant(grant);
		}

		if (getSetupGrant() && window.location.hash && beginSetupGrantSanitizing()) {
			const cleanURL = new URL(window.location.href);
			cleanURL.hash = '';
			window.requestAnimationFrame(() => {
				window.requestAnimationFrame(() => {
					if (window.location.pathname !== '/recover' || cleanURL.pathname !== '/recover') {
						clearSetupGrant();
						return;
					}
					void replaceState(cleanURL, page.state).then(markSetupGrantReady).catch(clearSetupGrant);
				});
			});
		}
	}

	return { hasGrant: Boolean(getSetupGrant()) };
};
