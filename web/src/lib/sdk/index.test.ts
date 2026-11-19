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
	it('scopes typed resource subscriptions and shares initial reads by complete identity', async () => {
		const socket = new FakeEventSocket();
		const api = createBrowserSDK({ webSocket: () => socket });
		const read = vi.fn(async () => 'initial');
		const first = vi.fn();
		const second = vi.fn();
		const unrelated = vi.fn();
		const disposeFirst = api.resources.subscribe({
			identity: { kind: 'deployment', projectId: 'p1', deploymentId: 'd1' },
			read,
			onValue: first
		});
		const disposeSecond = api.resources.subscribe({
			identity: { kind: 'deployment', projectId: 'p1', deploymentId: 'd1' },
			read,
			onValue: second
		});
		api.resources.subscribe({
			identity: { kind: 'deployment', projectId: 'p1', deploymentId: 'd2' },
			read: vi.fn(async () => 'd2'),
			onValue: unrelated
		});
		await vi.waitFor(() => {
			expect(read).toHaveBeenCalledOnce();
			expect(first).toHaveBeenCalledWith('initial');
			expect(second).toHaveBeenCalledWith('initial');
		});
		socket.emit(
			'message',
			JSON.stringify({
				code: 'events.ready',
				data: { epoch: 'epoch', sequence: 0 }
			})
		);
		await vi.waitFor(() => expect(read).toHaveBeenCalledTimes(2));
		socket.emit(
			'message',
			JSON.stringify({
				code: 'events.invalidate',
				data: { epoch: 'epoch', sequence: 1, resources: [{ kind: 'jobs', deploymentId: 'd2' }] }
			})
		);
		await vi.waitFor(() => expect(unrelated).toHaveBeenCalledTimes(3));
		expect(first).toHaveBeenCalledTimes(2);
		disposeFirst();
		disposeSecond();
	});

	it('retains an invalidation received during a read and suppresses results after disposal', async () => {
		const socket = new FakeEventSocket();
		const api = createBrowserSDK({ webSocket: () => socket });
		const resolvers: Array<(value: number) => void> = [];
		const read = vi.fn(() => new Promise<number>((resolve) => resolvers.push(resolve)));
		const receive = vi.fn();
		const dispose = api.resources.subscribe({
			identity: { kind: 'jobs', deploymentId: 'd1' },
			read,
			onValue: receive
		});
		socket.emit(
			'message',
			JSON.stringify({
				code: 'events.ready',
				data: { epoch: 'epoch', sequence: 0 }
			})
		);
		socket.emit(
			'message',
			JSON.stringify({
				code: 'events.invalidate',
				data: { epoch: 'epoch', sequence: 1, resources: [{ kind: 'jobs', deploymentId: 'd1' }] }
			})
		);
		resolvers[0](1);
		await vi.waitFor(() => expect(read).toHaveBeenCalledTimes(2));
		dispose();
		resolvers[1](2);
		await Promise.resolve();
		expect(receive).toHaveBeenCalledTimes(1);
		expect(receive).toHaveBeenCalledWith(1);
	});

	it('refreshes matching reads while keeping history query identities independent', async () => {
		const socket = new FakeEventSocket();
		const api = createBrowserSDK({ webSocket: () => socket });
		const firstHistory = vi.fn(async () => 'first page');
		const secondHistory = vi.fn(async () => 'second page');
		const firstValue = vi.fn();
		const secondValue = vi.fn();
		api.resources.subscribe({
			identity: { kind: 'history', deploymentId: 'd1', search: 'main', page: 1, limit: 20 },
			read: firstHistory,
			onValue: firstValue
		});
		api.resources.subscribe({
			identity: { kind: 'history', deploymentId: 'd1', search: 'main', page: 2, limit: 20 },
			read: secondHistory,
			onValue: secondValue
		});
		await vi.waitFor(() => {
			expect(firstValue).toHaveBeenCalledOnce();
			expect(secondValue).toHaveBeenCalledOnce();
		});
		socket.emit(
			'message',
			JSON.stringify({
				code: 'events.ready',
				data: { epoch: 'epoch', sequence: 0 }
			})
		);
		await vi.waitFor(() => {
			expect(firstHistory).toHaveBeenCalledTimes(2);
			expect(secondHistory).toHaveBeenCalledTimes(2);
		});
		socket.emit(
			'message',
			JSON.stringify({
				code: 'events.invalidate',
				data: { epoch: 'epoch', sequence: 1, resources: [{ kind: 'history', deploymentId: 'd1' }] }
			})
		);
		await vi.waitFor(() => {
			expect(firstHistory).toHaveBeenCalledTimes(3);
			expect(secondHistory).toHaveBeenCalledTimes(3);
		});
	});

	it('resynchronizes every active read on a sequence gap or epoch change', async () => {
		const socket = new FakeEventSocket();
		const api = createBrowserSDK({ webSocket: () => socket });
		const firstRead = vi.fn(async () => 'first');
		const secondRead = vi.fn(async () => 'second');
		api.resources.subscribe({
			identity: { kind: 'jobs', deploymentId: 'd1' },
			read: firstRead,
			onValue: vi.fn()
		});
		api.resources.subscribe({
			identity: { kind: 'jobs', deploymentId: 'd2' },
			read: secondRead,
			onValue: vi.fn()
		});
		await vi.waitFor(() => {
			expect(firstRead).toHaveBeenCalledOnce();
			expect(secondRead).toHaveBeenCalledOnce();
		});
		socket.emit(
			'message',
			JSON.stringify({
				code: 'events.ready',
				data: { epoch: 'epoch', sequence: 0 }
			})
		);
		await vi.waitFor(() => {
			expect(firstRead).toHaveBeenCalledTimes(2);
			expect(secondRead).toHaveBeenCalledTimes(2);
		});
		socket.emit(
			'message',
			JSON.stringify({
				code: 'events.invalidate',
				data: { epoch: 'epoch', sequence: 2, resources: [{ kind: 'jobs', deploymentId: 'd1' }] }
			})
		);
		await vi.waitFor(() => {
			expect(firstRead).toHaveBeenCalledTimes(3);
			expect(secondRead).toHaveBeenCalledTimes(3);
		});
		socket.emit(
			'message',
			JSON.stringify({
				code: 'events.invalidate',
				data: { epoch: 'restarted', sequence: 1, resources: [{ kind: 'jobs', deploymentId: 'd1' }] }
			})
		);
		await vi.waitFor(() => {
			expect(firstRead).toHaveBeenCalledTimes(4);
			expect(secondRead).toHaveBeenCalledTimes(4);
		});
	});

	it('refreshes deployment collections when a job changes deployment state', async () => {
		const socket = new FakeEventSocket();
		const api = createBrowserSDK({ webSocket: () => socket });
		const read = vi.fn(async () => []);
		api.resources.subscribe({
			identity: { kind: 'deployments' },
			read,
			onValue: vi.fn()
		});
		await vi.waitFor(() => expect(read).toHaveBeenCalledOnce());
		socket.emit(
			'message',
			JSON.stringify({
				code: 'events.ready',
				data: { epoch: 'epoch', sequence: 0 }
			})
		);
		await vi.waitFor(() => expect(read).toHaveBeenCalledTimes(2));
		socket.emit(
			'message',
			JSON.stringify({
				code: 'events.invalidate',
				data: { epoch: 'epoch', sequence: 1, resources: [{ kind: 'jobs', deploymentId: 'd1' }] }
			})
		);
		await vi.waitFor(() => expect(read).toHaveBeenCalledTimes(3));
	});

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

	it('resynchronizes once for repeated unknown events and closes the stream for backoff recovery', () => {
		const socket = new FakeEventSocket();
		const replacement = new FakeEventSocket();
		const timers = new Map<number, { callback: () => void; delay: number }>();
		let nextTimer = 0;
		const api = createBrowserSDK({
			webSocket: vi.fn().mockReturnValueOnce(socket).mockReturnValueOnce(replacement),
			random: () => 0,
			isOnline: () => true,
			setTimeout: (callback, delay) => {
				const id = ++nextTimer;
				timers.set(id, { callback, delay });
				return id as unknown as ReturnType<typeof setTimeout>;
			},
			clearTimeout: (timer) => timers.delete(timer as unknown as number)
		});
		const refresh = vi.fn();
		api.events.subscribe(refresh);

		socket.emit(
			'message',
			JSON.stringify({
				code: 'events.ready',
				data: { epoch: 'epoch', sequence: 0 }
			})
		);
		refresh.mockClear();
		socket.emit('message', JSON.stringify({ code: 'events.future', data: null }));
		expect(refresh).toHaveBeenCalledOnce();
		socket.emit('message', JSON.stringify({ code: 'events.future', data: null }));
		expect(refresh).toHaveBeenCalledOnce();
		expect(socket.closed).toBe(true);
		socket.closeWith(1006);
		const reconnect = [...timers.values()].find((timer) => timer.delay === 1000);
		expect(reconnect).toBeDefined();
		reconnect!.callback();
		expect(replacement.closed).toBe(false);
		replacement.emit(
			'message',
			JSON.stringify({
				code: 'events.invalidate',
				data: { epoch: 'epoch', sequence: 'invalid', resources: [] }
			})
		);
		expect(replacement.closed).toBe(true);
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
			isOnline: () => true,
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
			isOnline: () => true,
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

	it('starts scoped fallback reads after ten seconds and stops polling on ready', async () => {
		const socket = new FakeEventSocket();
		const timers = new Map<number, { callback: () => void; delay: number }>();
		let nextTimer = 0;
		const read = vi.fn(async () => []);
		const api = createBrowserSDK({
			webSocket: () => socket,
			isOnline: () => true,
			isVisible: () => true,
			setTimeout: (callback, delay) => {
				const id = ++nextTimer;
				timers.set(id, { callback, delay });
				return id as unknown as ReturnType<typeof setTimeout>;
			},
			clearTimeout: (timer) => timers.delete(timer as unknown as number)
		});
		const dispose = api.resources.subscribe({
			identity: { kind: 'repository_keys' },
			read,
			onValue: vi.fn()
		});
		await vi.waitFor(() => expect(read).toHaveBeenCalledOnce());
		const fallbackStart = [...timers.entries()].find(([, timer]) => timer.delay === 10_000);
		expect(fallbackStart).toBeDefined();
		fallbackStart![1].callback();
		timers.delete(fallbackStart![0]);
		await vi.waitFor(() => expect(read).toHaveBeenCalledTimes(2));
		expect([...timers.values()].some((timer) => timer.delay === 15_000)).toBe(true);

		socket.emit(
			'message',
			JSON.stringify({
				code: 'events.ready',
				data: { epoch: 'epoch', sequence: 0 }
			})
		);
		await vi.waitFor(() => expect(read).toHaveBeenCalledTimes(3));
		expect([...timers.values()].some((timer) => timer.delay === 15_000)).toBe(false);
		dispose();
	});

	it('restarts fallback after a ready connection drops during repeated reconnect failures', async () => {
		const sockets = [
			new FakeEventSocket(),
			new FakeEventSocket(),
			new FakeEventSocket(),
			new FakeEventSocket()
		];
		const [first, second, third, fourth] = sockets;
		const timers = new Map<number, { callback: () => void; delay: number }>();
		let nextTimer = 0;
		const read = vi.fn(async () => []);
		const api = createBrowserSDK({
			webSocket: () => sockets.shift()!,
			random: () => 0.5,
			isOnline: () => true,
			isVisible: () => true,
			setTimeout: (callback, delay) => {
				const id = ++nextTimer;
				timers.set(id, { callback, delay });
				return id as unknown as ReturnType<typeof setTimeout>;
			},
			clearTimeout: (timer) => timers.delete(timer as unknown as number)
		});
		const dispose = api.resources.subscribe({
			identity: { kind: 'repository_keys' },
			read,
			onValue: vi.fn()
		});
		await vi.waitFor(() => expect(read).toHaveBeenCalledOnce());
		first.emit(
			'message',
			JSON.stringify({ code: 'events.ready', data: { epoch: 'epoch', sequence: 0 } })
		);
		first.closeWith(1006);
		const fallbackStart = [...timers.entries()].find(([, timer]) => timer.delay === 10_000);
		expect(fallbackStart).toBeDefined();
		const reconnect = [...timers.entries()].find(([, timer]) => timer.delay === 1000);
		expect([...timers.values()].map((timer) => timer.delay)).toContain(1000);
		reconnect![1].callback();
		timers.delete(reconnect![0]);
		second.closeWith(1006);
		await vi.waitFor(() =>
			expect([...timers.values()].some((timer) => timer.delay === 2000)).toBe(true)
		);
		const nextReconnect = [...timers.entries()].find(([, timer]) => timer.delay === 2000);
		nextReconnect![1].callback();
		timers.delete(nextReconnect![0]);
		third.closeWith(1006);
		await vi.waitFor(() =>
			expect([...timers.values()].some((timer) => timer.delay === 4000)).toBe(true)
		);

		expect(read).toHaveBeenCalledTimes(2);
		fallbackStart![1].callback();
		timers.delete(fallbackStart![0]);
		await vi.waitFor(() => expect(read).toHaveBeenCalledTimes(3));
		expect([...timers.values()].filter((timer) => timer.delay === 15_000)).toHaveLength(1);

		const laterReconnect = [...timers.entries()].find(([, timer]) => timer.delay === 4000);
		expect(laterReconnect).toBeDefined();
		laterReconnect![1].callback();
		timers.delete(laterReconnect![0]);
		fourth.emit(
			'message',
			JSON.stringify({ code: 'events.ready', data: { epoch: 'epoch', sequence: 0 } })
		);
		await vi.waitFor(() => expect(read).toHaveBeenCalledTimes(4));
		expect([...timers.values()].some((timer) => timer.delay === 15_000)).toBe(false);
		dispose();
	});

	it('routes every resource invalidation to its active typed read and dependencies', async () => {
		const socket = new FakeEventSocket();
		const api = createBrowserSDK({ webSocket: () => socket });
		const subscriptions = [
			{ identity: { kind: 'projects' } as const, resource: { kind: 'projects' } },
			{
				identity: { kind: 'refs', projectId: 'p1' } as const,
				resource: { kind: 'refs', projectId: 'p1' }
			},
			{ identity: { kind: 'deployments' } as const, resource: { kind: 'deployments' } },
			{
				identity: { kind: 'jobs', deploymentId: 'd1' } as const,
				resource: { kind: 'jobs', deploymentId: 'd1' }
			},
			{
				identity: {
					kind: 'history',
					projectId: 'p1',
					deploymentId: 'd1',
					search: '',
					page: 1,
					limit: 20
				} as const,
				resource: { kind: 'refs', projectId: 'p1' }
			},
			{ identity: { kind: 'repository_keys' } as const, resource: { kind: 'repository_keys' } },
			{ identity: { kind: 'ssh_hosts' } as const, resource: { kind: 'ssh_hosts' } },
			{
				identity: { kind: 'lfs_settings', projectId: 'p1' } as const,
				resource: { kind: 'lfs_settings', projectId: 'p1' }
			},
			{
				identity: { kind: 'webhook_settings', projectId: 'p1' } as const,
				resource: { kind: 'webhook_settings', projectId: 'p1' }
			}
		];
		const reads = subscriptions.map(() => vi.fn(async () => 0));
		const disposers = subscriptions.map((subscription, index) =>
			api.resources.subscribe({
				identity: subscription.identity,
				read: reads[index],
				onValue: vi.fn()
			})
		);
		await vi.waitFor(() => reads.forEach((read) => expect(read).toHaveBeenCalledOnce()));
		socket.emit(
			'message',
			JSON.stringify({
				code: 'events.ready',
				data: { epoch: 'epoch', sequence: 0 }
			})
		);
		await vi.waitFor(() => reads.forEach((read) => expect(read).toHaveBeenCalledTimes(2)));

		const events = [
			{ kind: 'projects' },
			{ kind: 'refs', projectId: 'p1' },
			{ kind: 'deployments', projectId: 'p1' },
			{ kind: 'jobs', deploymentId: 'd1' },
			{ kind: 'repository_keys' },
			{ kind: 'ssh_hosts' },
			{ kind: 'lfs_settings', projectId: 'p1' },
			{ kind: 'webhook_settings', projectId: 'p1' }
		];
		socket.emit(
			'message',
			JSON.stringify({
				code: 'events.invalidate',
				data: { epoch: 'epoch', sequence: 1, resources: events }
			})
		);
		await vi.waitFor(() => {
			expect(reads[0]).toHaveBeenCalledTimes(3);
			expect(reads[1]).toHaveBeenCalledTimes(3);
			expect(reads[2]).toHaveBeenCalledTimes(3);
			expect(reads[3]).toHaveBeenCalledTimes(3);
			expect(reads[4]).toHaveBeenCalledTimes(3);
			expect(reads[5]).toHaveBeenCalledTimes(3);
			expect(reads[6]).toHaveBeenCalledTimes(3);
			expect(reads[7]).toHaveBeenCalledTimes(3);
			expect(reads[8]).toHaveBeenCalledTimes(3);
		});
		disposers.forEach((dispose) => dispose());
	});

	it('suspends fallback while hidden, refreshes once on foreground, and detaches lifecycle handlers', async () => {
		const socket = new FakeEventSocket();
		const timers = new Map<number, { callback: () => void; delay: number }>();
		let nextTimer = 0;
		let visible = true;
		let lifecycle: { foreground: () => void; online: () => void; offline: () => void } | undefined;
		const removeLifecycle = vi.fn(() => {
			lifecycle = undefined;
		});
		const read = vi.fn(async () => []);
		const api = createBrowserSDK({
			webSocket: () => socket,
			isOnline: () => true,
			isVisible: () => visible,
			lifecycle: (handlers) => {
				lifecycle = handlers;
				return removeLifecycle;
			},
			setTimeout: (callback, delay) => {
				const id = ++nextTimer;
				timers.set(id, { callback, delay });
				return id as unknown as ReturnType<typeof setTimeout>;
			},
			clearTimeout: (timer) => timers.delete(timer as unknown as number)
		});
		const dispose = api.resources.subscribe({
			identity: { kind: 'projects' },
			read,
			onValue: vi.fn()
		});
		await vi.waitFor(() => expect(read).toHaveBeenCalledOnce());
		visible = false;
		const fallbackStart = [...timers.entries()].find(([, timer]) => timer.delay === 10_000);
		fallbackStart![1].callback();
		timers.delete(fallbackStart![0]);
		expect([...timers.values()].some((timer) => timer.delay === 15_000)).toBe(false);

		visible = true;
		lifecycle!.foreground();
		await vi.waitFor(() => expect(read).toHaveBeenCalledTimes(2));
		expect([...timers.values()].some((timer) => timer.delay === 15_000)).toBe(true);
		dispose();
		expect(removeLifecycle).toHaveBeenCalledOnce();
		expect(lifecycle).toBeUndefined();
	});
});

describe('browser SDK deployment outcomes', () => {
	it('refreshes active jobs reads after a successful local deployment mutation', async () => {
		const api = createBrowserSDK({
			webSocket: () => new FakeEventSocket(),
			fetch: vi.fn().mockResolvedValue(
				response(
					{
						code: 'deployment.queued',
						message: 'Deployment queued.',
						data: { jobId: 'job-1', status: 'queued' }
					},
					202
				)
			)
		});
		const read = vi.fn(async () => []);
		api.resources.subscribe({
			identity: { kind: 'jobs', deploymentId: 'd1' },
			read,
			onValue: vi.fn()
		});
		await vi.waitFor(() => expect(read).toHaveBeenCalledOnce());

		await api.deployments.deploy('d1');

		await vi.waitFor(() => expect(read).toHaveBeenCalledTimes(2));
	});

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
