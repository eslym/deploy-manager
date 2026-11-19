import { describe, expect, it, vi } from 'vitest';
import { BrowserSDKError, createBrowserSDK, serverErrorCode } from './index';

function response(body: unknown, status = 200): Response {
	return new Response(JSON.stringify(body), {
		status,
		headers: { 'Content-Type': 'application/json' }
	});
}

describe('browser SDK authentication transport', () => {
	it('validates the endpoint payload and uses same-origin credentials without retrying mutations', async () => {
		const send = vi.fn().mockResolvedValue(
			response({
				code: 'auth.registration_started',
				message: 'Registration started.',
				data: { value: 'server-response', transaction: 'handle' }
			})
		);
		const api = createBrowserSDK({ fetch: send });

		const result = await api.auth.registerStart({ grant: 'grant', value: 'request' });

		expect(result.data.transaction).toBe('handle');
		expect(send).toHaveBeenCalledTimes(1);
		expect(send).toHaveBeenCalledWith(
			'/opaque/register/start',
			expect.objectContaining({
				method: 'POST',
				credentials: 'same-origin',
				body: JSON.stringify({ grant: 'grant', value: 'request' })
			})
		);
	});

	it('treats HTML and malformed data as invalid responses without exposing their contents', async () => {
		const api = createBrowserSDK({
			fetch: vi.fn().mockResolvedValue(new Response('<h1>proxy secret</h1>', { status: 502 }))
		});

		await expect(api.auth.loginStart({ value: 'request' })).rejects.toMatchObject({
			outcome: {
				kind: 'invalid_response',
				message: 'The request could not be completed. Try again.'
			}
		});
	});

	it('surfaces unknown semantic codes without triggering session-loss behavior', async () => {
		const onSessionLost = vi.fn();
		const api = createBrowserSDK({
			fetch: vi.fn().mockResolvedValue(
				response(
					{
						code: 'auth.future_unknown',
						message: 'Please try again.',
						data: null
					},
					401
				)
			),
			onSessionLost
		});

		try {
			await api.auth.loginStart({ value: 'request' });
		} catch (error) {
			expect(error).toBeInstanceOf(BrowserSDKError);
			expect((error as BrowserSDKError).outcome).toMatchObject({
				kind: 'server',
				code: 'auth.future_unknown',
				status: 401
			});
		}
		expect(onSessionLost).not.toHaveBeenCalled();
	});

	it('reports session loss once for protected session probes', async () => {
		const onSessionLost = vi.fn();
		const api = createBrowserSDK({
			fetch: vi.fn().mockResolvedValue(
				response(
					{
						code: 'auth.session_required',
						message: 'Sign in to continue.',
						data: null
					},
					401
				)
			),
			onSessionLost
		});

		await expect(api.auth.session()).rejects.toBeInstanceOf(BrowserSDKError);
		await expect(api.auth.session()).rejects.toBeInstanceOf(BrowserSDKError);
		expect(onSessionLost).toHaveBeenCalledTimes(1);
	});

	it('distinguishes caller cancellation from network failures', async () => {
		const controller = new AbortController();
		controller.abort();
		const api = createBrowserSDK({
			fetch: vi.fn().mockRejectedValue(new DOMException('private detail', 'AbortError'))
		});

		await expect(
			api.auth.loginFinish({ transaction: 't', value: 'v' }, controller.signal)
		).rejects.toMatchObject({
			outcome: { kind: 'cancelled', message: 'Request cancelled.' }
		});
	});

	it('never retries a mutation after an ambiguous network failure', async () => {
		const send = vi.fn().mockRejectedValue(new TypeError('private network detail'));
		const api = createBrowserSDK({ fetch: send });

		await expect(
			api.auth.registerFinish({ transaction: 'handle', value: 'record' })
		).rejects.toMatchObject({
			outcome: {
				kind: 'network',
				message: 'The request could not be completed. Try again.'
			}
		});
		expect(send).toHaveBeenCalledTimes(1);
	});

	it.each([
		{
			name: 'network',
			code: 'sdk.network',
			fetch: vi.fn().mockRejectedValue(new TypeError('private network detail')),
			call: (api: ReturnType<typeof createBrowserSDK>) => api.auth.loginStart({ value: 'request' })
		},
		{
			name: 'invalid response',
			code: 'sdk.invalid_response',
			fetch: vi.fn().mockResolvedValue(new Response('<h1>proxy secret</h1>', { status: 502 })),
			call: (api: ReturnType<typeof createBrowserSDK>) => api.auth.loginStart({ value: 'request' })
		},
		{
			name: 'cancellation',
			code: 'sdk.cancelled',
			fetch: vi.fn().mockRejectedValue(new DOMException('private detail', 'AbortError')),
			call: (api: ReturnType<typeof createBrowserSDK>) => {
				const controller = new AbortController();
				controller.abort();
				return api.auth.loginStart({ value: 'request' }, controller.signal);
			}
		}
	])('uses a local $code code for $name, not a server outcome', async ({ code, fetch, call }) => {
		const api = createBrowserSDK({ fetch });

		try {
			await call(api);
			throw new Error('expected request to fail');
		} catch (error) {
			expect(error).toBeInstanceOf(BrowserSDKError);
			expect((error as BrowserSDKError).outcome).toMatchObject({ code });
			expect((error as BrowserSDKError).outcome.kind).not.toBe('server');
			expect(serverErrorCode(error)).toBeUndefined();
		}
	});

	it('clears the affected browser session after successful recovery', async () => {
		const onSessionEnded = vi.fn();
		const api = createBrowserSDK({
			fetch: vi.fn().mockResolvedValue(
				response({
					code: 'auth.registration_completed',
					message: 'Registration completed. Sign in to continue.',
					data: null
				})
			),
			onSessionEnded
		});

		await api.auth.recoveryFinish({ transaction: 'handle', value: 'record' });
		expect(onSessionEnded).toHaveBeenCalledOnce();
	});

	it('rejects a known success code for another endpoint without ending the session', async () => {
		const onSessionEnded = vi.fn();
		const api = createBrowserSDK({
			fetch: vi
				.fn()
				.mockResolvedValue(
					response({ code: 'auth.logged_out', message: 'Signed out.', data: null })
				),
			onSessionEnded
		});

		await expect(
			api.auth.recoveryFinish({ transaction: 'handle', value: 'record' })
		).rejects.toMatchObject({
			outcome: { kind: 'invalid_response' }
		});
		expect(onSessionEnded).not.toHaveBeenCalled();
	});

	it('accepts an unknown valid outcome without treating it as the expected success', async () => {
		const onSessionEnded = vi.fn();
		const api = createBrowserSDK({
			fetch: vi
				.fn()
				.mockResolvedValue(
					response({ code: 'auth.future_result', message: 'Try again.', data: null })
				),
			onSessionEnded
		});

		const outcome = await api.auth.recoveryFinish({ transaction: 'handle', value: 'record' });
		expect(outcome.code).toBe('auth.future_result');
		expect(onSessionEnded).not.toHaveBeenCalled();
	});
});
