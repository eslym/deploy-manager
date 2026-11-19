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
						createdAt: '2026-11-19T00:00:00Z'
					}
				]
			})
		);
		const api = createBrowserSDK({ fetch: send });

		const result = await api.repositoryKeys.list();

		expect(result.code).toBe('repository_key.listed');
		expect(result.data[0].id).toBe('key-id');
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
});
