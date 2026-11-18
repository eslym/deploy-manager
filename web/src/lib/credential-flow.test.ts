import { afterEach, describe, expect, it, vi } from 'vitest';
import { client } from '@serenity-kit/opaque';
import { clearSetupGrant, rememberSetupGrant, setupGrant } from './setup-grant.svelte.js';
import { createCredentialFlow } from './credential-flow.svelte.js';

vi.mock('@serenity-kit/opaque', () => ({
	ready: Promise.resolve(),
	client: {
		startRegistration: vi.fn(() => ({
			registrationRequest: 'request',
			clientRegistrationState: 'client-state'
		})),
		finishRegistration: vi.fn(() => ({ registrationRecord: 'record' }))
	}
}));

afterEach(() => {
	clearSetupGrant();
	vi.unstubAllGlobals();
});

describe('registration flow', () => {
	it('intakes the fragment through the flow-owned interface', () => {
		createCredentialFlow('registration').intakeGrant('grant-value');
		expect(setupGrant.value).toBe('grant-value');
		expect(setupGrant.ready).toBe(false);
	});

	it('reports a missing grant without submitting or retaining a password', async () => {
		const flow = createCredentialFlow('registration');
		const field = { value: 'long-enough-password' } as HTMLInputElement;
		await flow.submit(field);
		expect(field.value).toBe('');
		expect(flow.state.statusMessage).toContain('incomplete');
	});

	it('rejects a short password and clears it', async () => {
		rememberSetupGrant('grant');
		const flow = createCredentialFlow('registration');
		const field = { value: 'short' } as HTMLInputElement;
		await flow.submit(field);
		expect(field.value).toBe('');
		expect(flow.state.statusMessage).toContain('12 characters');
	});

	it('handles an expired grant and settles pending state', async () => {
		rememberSetupGrant('grant');
		vi.stubGlobal(
			'fetch',
			vi.fn(async () => new Response('', { status: 410 }))
		);
		const flow = createCredentialFlow('registration');
		const field = { value: 'a-long-password-value' } as HTMLInputElement;
		await flow.submit(field);
		expect(flow.state.pending).toBe(false);
		expect(field.value).toBe('');
		expect(flow.state.statusMessage).toContain('expired');
	});

	it('fails closed on a malformed start response', async () => {
		rememberSetupGrant('grant');
		vi.stubGlobal(
			'fetch',
			vi.fn(async () => new Response('null', { status: 200 }))
		);
		const flow = createCredentialFlow('registration');
		const field = { value: 'a-long-password-value' } as HTMLInputElement;
		await flow.submit(field);
		expect(flow.state.pending).toBe(false);
		expect(flow.state.complete).toBe(false);
		expect(flow.state.statusMessage).toContain('new setup link');
		expect(setupGrant.value).toBe('grant');
	});

	it('fails closed when secure registration initialization fails', async () => {
		rememberSetupGrant('grant');
		vi.mocked(client.startRegistration).mockImplementationOnce(() => {
			throw new Error('initialization failed');
		});
		const flow = createCredentialFlow('registration');
		const field = { value: 'a-long-password-value' } as HTMLInputElement;
		await flow.submit(field);
		expect(flow.state.pending).toBe(false);
		expect(flow.state.complete).toBe(false);
		expect(field.value).toBe('');
		expect(flow.state.statusMessage).toContain('could not be initialized');
		expect(setupGrant.value).toBe('grant');
	});

	it('cleans a grant only after successful completion', async () => {
		rememberSetupGrant('grant');
		let requestCount = 0;
		const paths: string[] = [];
		vi.stubGlobal(
			'fetch',
			vi.fn(async (input: RequestInfo | URL) => {
				paths.push(String(input));
				requestCount++;
				if (requestCount === 2) expect(setupGrant.value).toBe('grant');
				return requestCount === 1
					? new Response(JSON.stringify({ value: 'server-response', transaction: 'transaction' }), {
							status: 200,
							headers: { 'Content-Type': 'application/json' }
						})
					: new Response(null, { status: 204 });
			})
		);
		const flow = createCredentialFlow('registration');
		const field = { value: 'a-long-password-value' } as HTMLInputElement;
		const fetchMock = vi.mocked(fetch);
		await flow.submit(field);
		expect(flow.state.complete).toBe(true);
		expect(field.value).toBe('');
		expect(setupGrant.value).toBe('');
		expect(fetchMock).toHaveBeenCalledTimes(2);
		expect(paths).toEqual(['/opaque/register/start', '/opaque/register/finish']);
		expect(client.finishRegistration).toHaveBeenCalledOnce();
	});
});
