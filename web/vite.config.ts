import tailwindcss from '@tailwindcss/vite';
import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig, loadEnv } from 'vite';

export default defineConfig(({ mode }) => {
	const backendURL = loadEnv(mode, '.', '').GO_BACKEND_URL ?? 'http://127.0.0.1:8080';

	return {
		server: {
			proxy: {
				'/api': backendURL,
				'/opaque': backendURL,
				'/webhooks': backendURL
			}
		},
		plugins: [
			tailwindcss(),
			sveltekit({
				compilerOptions: {
					// Force runes mode for the project, except for libraries. Can be removed in svelte 6.
					runes: ({ filename }) =>
						filename.split(/[/\\]/).includes('node_modules') ? undefined : true
				},

				// Generate a static site for embedding in the Go application.
				adapter: adapter({
					pages: '../lib/web',
					assets: '../lib/web',
					fallback: 'index.html'
				})
			})
		]
	};
});
