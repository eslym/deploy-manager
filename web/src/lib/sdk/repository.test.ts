import { describe, expect, it, vi } from 'vitest';
import { createBrowserSDK } from './index';

function response(body: unknown, status = 200): Response {
	return new Response(JSON.stringify(body), {
		status,
		headers: { 'Content-Type': 'application/json' }
	});
}

describe('repository management SDK operations', () => {
	it('validates and returns typed repository-key outcomes', async () => {
		const send = vi.fn().mockResolvedValue(
			response({
				code: 'repository_key.listed',
				message: 'Repository keys loaded.',
				data: [
					{
						id: 'key-id',
						name: 'reader',
						algorithm: 'Ed25519',
						bits: 256,
						fingerprint: 'SHA256:public-fingerprint',
						publicKey: 'ssh-ed25519 public-key',
						createdAt: '2026-11-19T00:00:00Z',
						projectCount: 0
					}
				]
			})
		);
		const api = createBrowserSDK({ fetch: send });

		const result = await api.repositoryKeys.list();

		expect(result.code).toBe('repository_key.listed');
		expect(result.data[0].id).toBe('key-id');
		expect(result.data[0].projectCount).toBe(0);
		expect(send).toHaveBeenCalledWith(
			'/api/repository-keys',
			expect.objectContaining({ method: 'GET', credentials: 'same-origin' })
		);
	});

	it('does not retry a secret rotation after an ambiguous transport failure', async () => {
		const send = vi.fn().mockRejectedValue(new TypeError('private transport detail'));
		const api = createBrowserSDK({ fetch: send });

		await expect(api.webhooks.rotate('project/id', 'gitea')).rejects.toMatchObject({
			outcome: { kind: 'network', message: 'The request could not be completed. Try again.' }
		});

		expect(send).toHaveBeenCalledTimes(1);
		expect(send).toHaveBeenCalledWith(
			'/api/projects/project%2Fid/gitea-webhook',
			expect.objectContaining({
				method: 'POST',
				body: JSON.stringify({ rotate: true }),
				credentials: 'same-origin'
			})
		);
	});

	it('keeps the rotated secret only in the authorized rotation result', async () => {
		const send = vi.fn().mockResolvedValue(
			response({
				code: 'webhook.secret_rotated',
				message: 'Webhook secret rotated.',
				data: { url: '/webhooks/github/project-id', secret: 'printable-secret' }
			})
		);
		const api = createBrowserSDK({ fetch: send });

		const result = await api.webhooks.rotate('project-id', 'github');

		expect(result.data.secret).toBe('printable-secret');
		expect(send).toHaveBeenCalledTimes(1);
	});

	it('uses typed outcomes for repository-key creation and deletion', async () => {
		const send = vi
			.fn()
			.mockResolvedValueOnce(
				response({
					code: 'repository_key.created',
					message: 'Repository key created.',
					data: {
						id: 'key-id',
						name: 'reader',
						algorithm: 'Ed25519',
						bits: 256,
						fingerprint: 'SHA256:public-fingerprint',
						publicKey: 'ssh-ed25519 public-key',
						createdAt: '2026-11-19T00:00:00Z',
						projectCount: 0
					}
				})
			)
			.mockResolvedValueOnce(
				response({ code: 'repository_key.deleted', message: 'Repository key deleted.', data: null })
			);
		const api = createBrowserSDK({ fetch: send });

		const created = await api.repositoryKeys.create({ name: 'reader', generate: true });
		const deleted = await api.repositoryKeys.delete(created.data.id);

		expect(created.code).toBe('repository_key.created');
		expect(created.data.publicKey).toBe('ssh-ed25519 public-key');
		expect(deleted).toMatchObject({ code: 'repository_key.deleted', data: null });
		expect(send).toHaveBeenCalledTimes(2);
	});

	it('uses typed outcomes for SSH host approval and revocation', async () => {
		const send = vi
			.fn()
			.mockResolvedValueOnce(
				response({
					code: 'ssh_host.listed',
					message: 'SSH hosts loaded.',
					data: [
						{
							host: 'git.example',
							port: 22,
							fingerprint: 'SHA256:fingerprint',
							warning: '',
							trusted: true
						}
					]
				})
			)
			.mockResolvedValueOnce(
				response({ code: 'ssh_host.approved', message: 'SSH host approved.', data: null })
			)
			.mockResolvedValueOnce(
				response({ code: 'ssh_host.revoked', message: 'SSH host trust revoked.', data: null })
			);
		const api = createBrowserSDK({ fetch: send });
		const observation = { host: 'git.example', port: 22, fingerprint: 'SHA256:fingerprint' };

		const hosts = await api.sshHosts.list();
		const approved = await api.sshHosts.approve({ ...observation, confirm: true });
		const revoked = await api.sshHosts.revoke({ ...observation, confirm: true });

		expect(hosts.data[0].trusted).toBe(true);
		expect(approved.code).toBe('ssh_host.approved');
		expect(revoked.code).toBe('ssh_host.revoked');
		expect(send).toHaveBeenCalledTimes(3);
	});

	it('loads and saves LFS settings through validated envelopes', async () => {
		const send = vi
			.fn()
			.mockResolvedValueOnce(
				response({
					code: 'lfs.settings_loaded',
					message: 'LFS settings loaded.',
					data: {
						settings: { endpoint: '', objectOrigins: [], allowInternal: false },
						suggestedEndpoint: 'https://git.example/info/lfs'
					}
				})
			)
			.mockResolvedValueOnce(
				response({ code: 'lfs.settings_saved', message: 'LFS settings saved.', data: null })
			);
		const api = createBrowserSDK({ fetch: send });

		const loaded = await api.lfs.settings('project/id');
		const saved = await api.lfs.save('project/id', {
			endpoint: 'https://git.example/info/lfs',
			objectOrigins: ['https://objects.example'],
			allowInternal: false,
			confirmation: 'I independently verified these LFS destinations'
		});

		expect(loaded.data.suggestedEndpoint).toBe('https://git.example/info/lfs');
		expect(saved).toMatchObject({ code: 'lfs.settings_saved', data: null });
		expect(send).toHaveBeenCalledWith(
			'/api/projects/project%2Fid/lfs',
			expect.objectContaining({ method: 'POST' })
		);
	});

	it('rejects malformed host data and preserves actionable server codes', async () => {
		const malformed = createBrowserSDK({
			fetch: vi.fn().mockResolvedValue(
				response({
					code: 'ssh_host.listed',
					message: 'SSH hosts loaded.',
					data: [{ host: 'git.example', port: '22' }]
				})
			)
		});
		await expect(malformed.sshHosts.list()).rejects.toMatchObject({
			outcome: { kind: 'invalid_response', code: 'sdk.invalid_response' }
		});

		const conflict = createBrowserSDK({
			fetch: vi.fn().mockResolvedValue(
				response(
					{
						code: 'ssh_host.observation_changed',
						message: 'SSH host details changed. Review them again.',
						data: null
					},
					409
				)
			)
		});
		await expect(
			conflict.sshHosts.approve({
				host: 'git.example',
				port: 22,
				fingerprint: 'SHA256:old',
				confirm: true
			})
		).rejects.toMatchObject({
			outcome: {
				kind: 'server',
				status: 409,
				code: 'ssh_host.observation_changed',
				data: null
			}
		});
	});

	it('preserves the repository-key in-use outcome for delete attempts', async () => {
		const api = createBrowserSDK({
			fetch: vi.fn().mockResolvedValue(
				response(
					{
						code: 'repository_key.in_use',
						message: 'Repository key is assigned to a project.',
						data: null
					},
					409
				)
			)
		});

		await expect(api.repositoryKeys.delete('assigned-key')).rejects.toMatchObject({
			outcome: {
				kind: 'server',
				status: 409,
				code: 'repository_key.in_use',
				data: null
			}
		});
	});
});
