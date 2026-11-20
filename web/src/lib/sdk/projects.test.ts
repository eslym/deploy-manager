import { describe, expect, it, vi } from 'vitest';
import { createBrowserSDK } from './index';

describe('project SDK operations', () => {
	it('constructs encoded paths and validates project/ref resource data', async () => {
		const fetch = vi
			.fn()
			.mockResolvedValueOnce(
				new Response(
					JSON.stringify({
						code: 'project.refs_listed',
						message: 'Cached refs loaded.',
						data: [{ kind: 'branch', name: 'release/a', commit: 'full-commit-id' }]
					})
				)
			)
			.mockResolvedValueOnce(
				new Response(
					JSON.stringify({
						code: 'project.listed',
						message: 'Projects loaded.',
						data: [
							{
								id: 'full/project-id',
								name: 'site',
								url: 'https://example.test/site.git',
								lastFetched: null,
								gitCacheBytes: 12,
								lfsCacheBytes: 34
							}
						]
					})
				)
			);
		const api = createBrowserSDK({ fetch });

		const refs = await api.projects.refs('full/project-id');
		const projects = await api.projects.list();

		expect(refs.data[0].commit).toBe('full-commit-id');
		expect(projects.data[0].id).toBe('full/project-id');
		expect(fetch).toHaveBeenNthCalledWith(
			1,
			'/api/projects/full%2Fproject-id/refs',
			expect.objectContaining({ method: 'GET', credentials: 'same-origin' })
		);
	});

	it('sends cache clearing as a single explicit mutation and validates null data', async () => {
		const fetch = vi.fn().mockResolvedValue(
			new Response(
				JSON.stringify({
					code: 'project.cache_cleared',
					message: 'Repository caches cleared.',
					data: null
				})
			)
		);
		const api = createBrowserSDK({ fetch });

		await api.projects.clearCache('project-id');

		expect(fetch).toHaveBeenCalledTimes(1);
		expect(fetch).toHaveBeenCalledWith(
			'/api/projects/project-id/cache/recreate',
			expect.objectContaining({ method: 'POST', body: JSON.stringify({ confirm: true }) })
		);
	});

	it('rejects malformed project resources instead of trusting transport JSON', async () => {
		const api = createBrowserSDK({
			fetch: vi.fn().mockResolvedValue(
				new Response(
					JSON.stringify({
						code: 'project.listed',
						message: 'Projects loaded.',
						data: [{ id: 42, name: 'untrusted' }]
					})
				)
			)
		});

		await expect(api.projects.list()).rejects.toMatchObject({
			outcome: { kind: 'invalid_response' }
		});
	});

	it('sends a key filter as an encoded projects-list query parameter', async () => {
		const fetch = vi.fn().mockResolvedValue(
			new Response(
				JSON.stringify({
					code: 'project.listed',
					message: 'Projects loaded.',
					data: []
				})
			)
		);
		const api = createBrowserSDK({ fetch });

		await api.projects.list({ keyId: 'key/id + one' });

		expect(fetch).toHaveBeenCalledWith(
			'/api/projects?keyId=key%2Fid%20%2B%20one',
			expect.objectContaining({ method: 'GET', credentials: 'same-origin' })
		);
	});

	it('passes caller cancellation through project operations without retrying mutations', async () => {
		const controller = new AbortController();
		const fetch = vi.fn().mockResolvedValue(
			new Response(
				JSON.stringify({
					code: 'project.cache_cleared',
					message: 'Repository caches cleared.',
					data: null
				})
			)
		);
		const api = createBrowserSDK({ fetch });

		await api.projects.clearCache('project-id', controller.signal);

		expect(fetch).toHaveBeenCalledTimes(1);
		expect(fetch).toHaveBeenCalledWith(
			'/api/projects/project-id/cache/recreate',
			expect.objectContaining({ signal: controller.signal })
		);
	});

	it('preserves actionable project outcomes for refresh, key assignment, and cache clearing', async () => {
		const fetch = vi
			.fn()
			.mockResolvedValueOnce(
				new Response(
					JSON.stringify({
						code: 'ssh_host.approval_required',
						message: 'Verify and approve the SSH host first.',
						data: null
					}),
					{ status: 502 }
				)
			)
			.mockResolvedValueOnce(
				new Response(
					JSON.stringify({
						code: 'project.key_assigned',
						message: 'Repository key assigned.',
						data: null
					})
				)
			)
			.mockResolvedValueOnce(
				new Response(
					JSON.stringify({
						code: 'project.cache_cleared',
						message: 'Repository caches cleared.',
						data: null
					})
				)
			);
		const api = createBrowserSDK({ fetch });

		await expect(api.projects.refresh('project-id')).rejects.toMatchObject({
			outcome: { kind: 'server', status: 502, code: 'ssh_host.approval_required', data: null }
		});
		await expect(api.projects.assignKey('project-id', { keyId: 'key-id' })).resolves.toMatchObject({
			code: 'project.key_assigned',
			data: null
		});
		await expect(api.projects.clearCache('project-id')).resolves.toMatchObject({
			code: 'project.cache_cleared',
			data: null
		});
		expect(fetch).toHaveBeenCalledTimes(3);
	});
});
