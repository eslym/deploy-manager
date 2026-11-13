import { browser } from '$app/env';
import { redirect } from '@sveltejs/kit';
import type { LayoutLoad } from './$types';

export const load: LayoutLoad = async ({ fetch, url }) => {
	if (browser) {
		const response = await fetch('/api/session', { credentials: 'same-origin' });
		if (response.status === 401) {
			const next = `${url.pathname}${url.search}`;
			redirect(302, `/login?next=${encodeURIComponent(next)}`);
		}
		if (!response.ok) throw new Error('Could not verify your session. Reload and try again.');
	}
	return {};
};
