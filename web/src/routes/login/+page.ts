import { browser } from '$app/env';
import { redirect } from '@sveltejs/kit';
import type { PageLoad } from './$types';

export const load: PageLoad = async ({ fetch }) => {
	if (browser) {
		const response = await fetch('/api/session', { credentials: 'same-origin' });
		if (response.ok) redirect(302, '/dashboard');
	}
	return {};
};
