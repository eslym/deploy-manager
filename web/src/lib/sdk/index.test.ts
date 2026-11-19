import { describe, expect, it, vi } from 'vitest';
import { BrowserSDKError, createBrowserSDK, serverErrorCode } from './index';

function response(body: unknown, status = 200): Response {
	return new Response(JSON.stringify(body), {
		status,
		headers: { 'Content-Type': 'application/json' }
	});
}

class FakeEventSocket {
	listeners = new Map<string, ((event: Event) => void)[]>();
	closed = false;

	addEventListener(type: string, listener: (event: Event) => void) {
		this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener]);
	}

	close() {
		this.closed = true;
	}

	emit(type: string, data?: string) {
		for (const listener of this.listeners.get(type) ?? []) {
			listener(new MessageEvent(type, { data }));
		}
	}

	closeWith(code: number) {
		for (const listener of this.listeners.get('close') ?? []) {
			listener(new CloseEvent('close', { code }));
		}
	}
}

describe('browser SDK authentication transport', () => {
	it('uses named lifecycle operations and does not retry destructive mutations', async () => {
		const send = vi
			.fn()
			.mockResolvedValueOnce(
				response({
					code: 'deployment.moved',
					message: 'Publication path changed.',
					data: { publicationPath: '/srv/sites/new-site' }
				})
			)
			.mockResolvedValueOnce(
				response({ code: 'deployment.deleted', message: 'Deployment deleted.', data: null })
			)
			.mockResolvedValueOnce(
				response({ code: 'project.deleted', message: 'Project deleted.', data: null })
			);
		const api = createBrowserSDK({ fetch: send });

		const moved = await api.deployments.move('deploy/1', {
			publicationPath: 'new-site',
			expectedPublicationPath: '/srv/sites/old-site',
			confirm: true
		});
		await api.deployments.delete('deployment-1');
		await api.projects.delete('project-1');

		expect(moved.data.publicationPath).toBe('/srv/sites/new-site');
		expect(send.mock.calls.map(([path]) => path)).toEqual([
			'/api/deployments/deploy%2F1/move',
			'/api/deployments/deployment-1/delete',
			'/api/projects/project-1/delete'
		]);
		expect(send).toHaveBeenCalledTimes(3);
		expect(send.mock.calls[0][1]).toMatchObject({
			method: 'POST',
			body: JSON.stringify({
				publicationPath: 'new-site',
				expectedPublicationPath: '/srv/sites/old-site',
				confirm: true
			})
		});
	});

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

describe('browser SDK live event ownership', () => {
	it('shares one connection, refreshes on ready and invalidation, and closes after the last disposal', () => {
		const socket = new FakeEventSocket();
		const webSocket = vi.fn(() => socket);
		const api = createBrowserSDK({ webSocket });
		const first = vi.fn();
		const second = vi.fn();
		const disposeFirst = api.events.subscribe(first);
		const disposeSecond = api.events.subscribe(second);

		expect(webSocket).toHaveBeenCalledTimes(1);
		socket.emit(
			'message',
			JSON.stringify({
				code: 'events.ready',
				message: 'Live updates connected.',
				data: { epoch: 'epoch', sequence: 3 }
			})
		);
		socket.emit(
			'message',
			JSON.stringify({
				code: 'events.invalidate',
				message: 'Dashboard data changed.',
				data: { epoch: 'epoch', sequence: 4, resources: [{ kind: 'jobs', deploymentId: 'd1' }] }
			})
		);
		expect(first).toHaveBeenCalledTimes(2);
		expect(second).toHaveBeenCalledTimes(2);
		socket.emit(
			'message',
			JSON.stringify({
				code: 'events.invalidate',
				message: 'Dashboard data changed.',
				data: { epoch: 'epoch', sequence: 4, resources: [{ kind: 'jobs', deploymentId: 'd1' }] }
			})
		);
		expect(first).toHaveBeenCalledTimes(2);

		disposeFirst();
		expect(socket.closed).toBe(false);
		disposeSecond();
		expect(socket.closed).toBe(true);
	});

	it('resynchronizes unknown event codes and closes a malformed stream', () => {
		const socket = new FakeEventSocket();
		const api = createBrowserSDK({ webSocket: () => socket });
		const refresh = vi.fn();
		api.events.subscribe(refresh);

		socket.emit('message', JSON.stringify({ code: 'events.future', data: null }));
		expect(refresh).toHaveBeenCalledOnce();
		socket.emit(
			'message',
			JSON.stringify({
				code: 'events.invalidate',
				data: { epoch: 'epoch', sequence: 'invalid', resources: [] }
			})
		);
		expect(socket.closed).toBe(true);
	});

	it('reconnects after an established socket closes and disposes the pending timer', () => {
		const first = new FakeEventSocket();
		const second = new FakeEventSocket();
		const sockets = [first, second];
		const timers = new Map<number, { callback: () => void; delay: number }>();
		let nextTimer = 0;
		const api = createBrowserSDK({
			webSocket: vi.fn(() => sockets.shift()!),
			random: () => 0.5,
			setTimeout: (callback, delay) => {
				const id = ++nextTimer;
				timers.set(id, { callback, delay });
				return id as unknown as ReturnType<typeof setTimeout>;
			},
			clearTimeout: (timer) => timers.delete(timer as unknown as number)
		});
		const dispose = api.events.subscribe(vi.fn());
		first.emit(
			'message',
			JSON.stringify({
				code: 'events.ready',
				message: 'Live updates connected.',
				data: { epoch: 'epoch', sequence: 0 }
			})
		);
		first.emit('close');
		const reconnect = [...timers.entries()].find(([, timer]) => timer.delay === 1000);
		expect(reconnect).toBeDefined();
		reconnect![1].callback();
		timers.delete(reconnect![0]);
		expect(second.closed).toBe(false);
		dispose();
		expect(second.closed).toBe(true);
	});

	it('applies jitter to exponential reconnect delays and resets only after a healthy interval', () => {
		const first = new FakeEventSocket();
		const second = new FakeEventSocket();
		const third = new FakeEventSocket();
		const sockets = [first, second, third];
		const timers = new Map<number, { callback: () => void; delay: number }>();
		let nextTimer = 0;
		const api = createBrowserSDK({
			webSocket: () => sockets.shift()!,
			random: () => 0,
			setTimeout: (callback, delay) => {
				const id = ++nextTimer;
				timers.set(id, { callback, delay });
				return id as unknown as ReturnType<typeof setTimeout>;
			},
			clearTimeout: (timer) => timers.delete(timer as unknown as number)
		});
		const dispose = api.events.subscribe(vi.fn());
		first.emit(
			'message',
			JSON.stringify({
				code: 'events.ready',
				data: { epoch: 'epoch', sequence: 0 }
			})
		);
		first.closeWith(1006);
		const firstReconnect = [...timers.entries()].find(([, timer]) => timer.delay === 1000);
		expect(firstReconnect).toBeDefined();
		firstReconnect![1].callback();
		timers.delete(firstReconnect![0]);
		second.emit(
			'message',
			JSON.stringify({
				code: 'events.ready',
				data: { epoch: 'epoch', sequence: 0 }
			})
		);
		second.closeWith(1006);
		const secondReconnect = [...timers.entries()].find(([, timer]) => timer.delay === 1600);
		expect(secondReconnect).toBeDefined();
		secondReconnect![1].callback();
		timers.delete(secondReconnect![0]);
		third.emit(
			'message',
			JSON.stringify({
				code: 'events.ready',
				data: { epoch: 'epoch', sequence: 0 }
			})
		);
		const healthy = [...timers.entries()].find(([, timer]) => timer.delay === 30_000);
		expect(healthy).toBeDefined();
		healthy![1].callback();
		timers.delete(healthy![0]);
		third.closeWith(1006);
		expect([...timers.values()].some((timer) => timer.delay === 1000)).toBe(true);
		dispose();
	});

	it('probes the session after a failed handshake and stops on verified session loss', async () => {
		const socket = new FakeEventSocket();
		const onSessionLost = vi.fn();
		let reportSessionLost!: () => void;
		const sessionLost = new Promise<void>((resolve) => {
			reportSessionLost = resolve;
		});
		onSessionLost.mockImplementation(reportSessionLost);
		const fetch = vi
			.fn()
			.mockResolvedValue(
				response(
					{ code: 'auth.session_required', message: 'Sign in to continue.', data: null },
					401
				)
			);
		const api = createBrowserSDK({ webSocket: () => socket, fetch, onSessionLost });
		api.events.subscribe(vi.fn());

		socket.closeWith(1006);
		await sessionLost;
		expect(onSessionLost).toHaveBeenCalledOnce();
		expect(fetch).toHaveBeenCalledWith(
			'/api/session',
			expect.objectContaining({ method: 'GET', credentials: 'same-origin' })
		);
	});
});

describe('browser SDK deployment outcomes', () => {
	it('keeps queue acceptance distinct from the published deployment state', async () => {
		const send = vi.fn().mockResolvedValue(
			response(
				{
					code: 'deployment.queued',
					message: 'Deployment queued.',
					data: { jobId: 'job-1', status: 'queued' }
				},
				202
			)
		);
		const api = createBrowserSDK({ fetch: send });

		const outcome = await api.deployments.deploy('deployment/1');

		expect(outcome).toMatchObject({
			code: 'deployment.queued',
			data: { jobId: 'job-1', status: 'queued' }
		});
		expect(send).toHaveBeenCalledWith(
			'/api/deployments/deployment%2F1/deploy',
			expect.objectContaining({ method: 'POST', credentials: 'same-origin' })
		);
	});

	it('validates deployment reads and keeps the unknown-deployment jobs response as an array', async () => {
		const send = vi
			.fn()
			.mockResolvedValue(response({ code: 'job.listed', message: 'Jobs loaded.', data: [] }));
		const api = createBrowserSDK({ fetch: send });

		await expect(api.deployments.jobs('missing')).resolves.toMatchObject({
			code: 'job.listed',
			data: []
		});
	});

	it('rejects malformed typed deployment history instead of trusting response JSON', async () => {
		const api = createBrowserSDK({
			fetch: vi.fn().mockResolvedValue(
				response({
					code: 'deployment.history_loaded',
					message: 'Commit history loaded.',
					data: { commits: [{ id: 'not-a-commit-object' }], page: 1, hasMore: false }
				})
			)
		});

		await expect(api.deployments.history('deployment-1')).rejects.toMatchObject({
			outcome: { kind: 'invalid_response' }
		});
	});

	it('does not replay mutations after an ambiguous transport failure', async () => {
		const send = vi.fn().mockRejectedValue(new TypeError('private transport detail'));
		const api = createBrowserSDK({ fetch: send });

		await expect(api.deployments.hardRedeploy('deploy-1')).rejects.toMatchObject({
			outcome: { kind: 'network', message: 'The request could not be completed. Try again.' }
		});
		expect(send).toHaveBeenCalledTimes(1);
	});
});
