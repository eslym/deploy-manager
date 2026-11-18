import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
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

beforeEach(() => {
	vi.mocked(client.startRegistration).mockReset().mockReturnValue({
		registrationRequest: 'request',
		clientRegistrationState: 'client-state'
	});
	vi.mocked(client.finishRegistration).mockReset().mockReturnValue({
		registrationRecord: 'record',
		exportKey: 'export-key',
		serverStaticPublicKey: 'server-static-public-key'
	});
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

describe('recovery purpose through the shared credential flow', () => {
	it('keeps recovery grant intake and URL cleanup scoped to /recover', async () => {
		const flow = createCredentialFlow('recovery');
		flow.intakeGrant('recovery-secret');
		const navigate = vi.fn(async (url: URL) => url.href);
		const currentURL = vi.fn(() => 'https://manager.test/recover');

		flow.sanitizeURL(
			new URL('https://manager.test/register#registration-secret'),
			navigate,
			currentURL
		);
		expect(navigate).not.toHaveBeenCalled();

		flow.sanitizeURL(new URL('https://manager.test/recover#recovery-secret'), navigate, currentURL);
		await vi.waitFor(() => expect(setupGrant.ready).toBe(true));
		expect(navigate).toHaveBeenCalledOnce();
		expect(navigate.mock.calls[0][0].hash).toBe('');
		expect(setupGrant.value).toBe('recovery-secret');
	});

	it('reports an incomplete recovery grant without submitting or retaining the password', async () => {
		const flow = createCredentialFlow('recovery');
		const field = { value: 'recovery-password-value' } as HTMLInputElement;
		const fetchMock = vi.fn();
		vi.stubGlobal('fetch', fetchMock);

		await flow.submit(field);

		expect(field.value).toBe('');
		expect(flow.state.statusMessage).toContain('recovery link is incomplete');
		expect(fetchMock).not.toHaveBeenCalled();
	});

	it('uses the recovery message for expired grants and clears the password before the request', async () => {
		rememberSetupGrant('recovery-grant');
		const field = { value: 'recovery-password-value' } as HTMLInputElement;
		const fetchMock = vi.fn<typeof fetch>(async () => {
			expect(field.value).toBe('');
			return new Response('', { status: 410 });
		});
		vi.stubGlobal('fetch', fetchMock);
		const flow = createCredentialFlow('recovery');

		await flow.submit(field);

		expect(fetchMock).toHaveBeenCalledOnce();
		expect(JSON.parse(fetchMock.mock.calls[0][1]?.body as string)).toEqual({
			grant: 'recovery-grant',
			value: 'request'
		});
		expect(flow.state.statusMessage).toContain('recovery link has expired or was already used');
		expect(flow.state.pending).toBe(false);
		expect(field.value).toBe('');
		expect(flow.state.complete).toBe(false);
	});

	it('rejects malformed recovery responses before finishing and retains the grant for a retry', async () => {
		rememberSetupGrant('recovery-grant');
		const fetchMock = vi.fn(
			async () => new Response(JSON.stringify({ value: 'missing transaction' }), { status: 200 })
		);
		vi.stubGlobal('fetch', fetchMock);
		const flow = createCredentialFlow('recovery');
		const field = { value: 'recovery-password-value' } as HTMLInputElement;

		await flow.submit(field);

		expect(fetchMock).toHaveBeenCalledOnce();
		expect(flow.state.statusMessage).toContain('new recovery link');
		expect(flow.state.complete).toBe(false);
		expect(field.value).toBe('');
		expect(setupGrant.value).toBe('recovery-grant');
		expect(client.finishRegistration).not.toHaveBeenCalled();
	});

	it('fails closed when recovery OPAQUE initialization fails without making a request', async () => {
		rememberSetupGrant('recovery-grant');
		vi.mocked(client.startRegistration).mockImplementationOnce(() => {
			throw new Error('initialization failed');
		});
		const fetchMock = vi.fn();
		vi.stubGlobal('fetch', fetchMock);
		const flow = createCredentialFlow('recovery');
		const field = { value: 'recovery-password-value' } as HTMLInputElement;

		await flow.submit(field);

		expect(fetchMock).not.toHaveBeenCalled();
		expect(flow.state.statusMessage).toContain('Secure recovery could not be initialized');
		expect(flow.state.pending).toBe(false);
		expect(field.value).toBe('');
		expect(setupGrant.value).toBe('recovery-grant');
	});

	it('preserves recovery-specific failures while binding finish to the one-use transaction', async () => {
		rememberSetupGrant('recovery-grant');
		const requests: Array<{ path: string; body: Record<string, string> }> = [];
		const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
			requests.push({
				path: String(input),
				body: JSON.parse(String(init?.body))
			});
			if (requests.length === 1) {
				return new Response(
					JSON.stringify({ value: 'server-response', transaction: 'recovery-tx' }),
					{
						status: 200,
						headers: { 'Content-Type': 'application/json' }
					}
				);
			}
			return new Response('', { status: 503 });
		});
		vi.stubGlobal('fetch', fetchMock);
		const flow = createCredentialFlow('recovery');
		const field = { value: 'recovery-password-value' } as HTMLInputElement;

		await flow.submit(field);

		expect(requests.map(({ path }) => path)).toEqual([
			'/opaque/register/start',
			'/opaque/register/finish'
		]);
		expect(requests[0].body).toEqual({ grant: 'recovery-grant', value: 'request' });
		expect(requests[1].body).toEqual({ transaction: 'recovery-tx', value: 'record' });
		expect(
			requests.every(({ body }) => !JSON.stringify(body).includes('recovery-password-value'))
		).toBe(true);
		expect(flow.state.statusMessage).toContain('Recovery could not be completed');
		expect(flow.state.complete).toBe(false);
		expect(flow.state.pending).toBe(false);
		expect(field.value).toBe('');
		expect(setupGrant.value).toBe('recovery-grant');
	});

	it('completes recovery only after both protocol requests and then clears the grant', async () => {
		rememberSetupGrant('recovery-grant');
		const requests: Array<{ path: string; body: Record<string, string> }> = [];
		const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
			requests.push({ path: String(input), body: JSON.parse(String(init?.body)) });
			if (requests.length === 1) {
				return new Response(
					JSON.stringify({ value: 'server-response', transaction: 'recovery-tx' }),
					{
						status: 200,
						headers: { 'Content-Type': 'application/json' }
					}
				);
			}
			expect(setupGrant.value).toBe('recovery-grant');
			return new Response(null, { status: 204 });
		});
		vi.stubGlobal('fetch', fetchMock);
		const flow = createCredentialFlow('recovery');
		const field = { value: 'recovery-password-value' } as HTMLInputElement;

		await flow.submit(field);

		expect(flow.state.complete).toBe(true);
		expect(flow.state.statusMessage).toBe(
			'Administrator password replaced. You can close this page.'
		);
		expect(field.value).toBe('');
		expect(setupGrant.value).toBe('');
		expect(requests.map(({ path }) => path)).toEqual([
			'/opaque/register/start',
			'/opaque/register/finish'
		]);
		expect(
			requests.every(({ body }) => !JSON.stringify(body).includes('recovery-password-value'))
		).toBe(true);
	});
});
