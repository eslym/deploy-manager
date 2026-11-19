import { browser } from '$app/env';
import { redirect } from '@sveltejs/kit';
import { BrowserSDKError, sdk } from '../../lib/sdk/index.js';
import type { LayoutLoad } from './$types';

export const load: LayoutLoad = async ({ url }) => {
	if (browser) {
		try {
			await sdk.auth.session();
		} catch (cause) {
			if (
				cause instanceof BrowserSDKError &&
				cause.outcome.kind === 'server' &&
				cause.outcome.code === 'auth.session_required'
			) {
				const next = `${url.pathname}${url.search}`;
				redirect(302, `/login?next=${encodeURIComponent(next)}`);
			}
			throw new Error('Could not verify your session. Reload and try again.');
		}
	}
	return {};
};
