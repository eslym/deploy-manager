import { browser } from '$app/env';
import { redirect } from '@sveltejs/kit';
import { sdk } from '../../lib/sdk/index.js';
import type { PageLoad } from './$types';

export const load: PageLoad = async () => {
	if (browser) {
		try {
			await sdk.auth.session();
			redirect(302, '/dashboard');
		} catch {
			// A failed probe leaves the sign-in form available.
		}
	}
	return {};
};
