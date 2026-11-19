import {
	createRepositoryOperations,
	repositoryOutcomeCodes,
	type RepositoryOutcomeCode
} from './repository';
import { createProjectOperations } from './projects';
import { createDeploymentSDK } from './deployments';

export type OutcomeCode =
	| RepositoryOutcomeCode
	| 'auth.registration_started'
	| 'auth.registration_completed'
	| 'auth.login_started'
	| 'auth.login_completed'
	| 'auth.login_failed'
	| 'auth.session_valid'
	| 'auth.session_required'
	| 'auth.logged_out'
	| 'auth.password_change_started'
	| 'auth.password_changed'
	| 'auth.recent_login_required'
	| 'auth.protocol_invalid'
	| 'auth.registration_expired'
	| 'auth.registration_conflict'
	| 'auth.capacity_exceeded'
	| 'request.https_required'
	| 'request.json_required'
	| 'request.policy_rejected'
	| 'request.origin_rejected'
	| 'request.rate_limited'
	| 'service.unavailable'
	| 'service.internal_error'
	| 'project.listed'
	| 'project.created'
	| 'project.refs_listed'
	| 'project.refreshed'
	| 'project.key_assigned'
	| 'project.cache_cleared'
	| 'git.target_unavailable'
	| 'git.authentication_failed'
	| 'ssh_host.approval_required'
	| 'request.invalid'
	| 'request.not_found'
	| 'operation.capacity_exceeded'
	| 'operation.upstream_failed'
	| 'project.cache_recovery_required'
	| 'project.deleted'
	| 'project.has_deployments'
	| 'operation.cleanup_pending'
	| 'publication.recovery_required'
	| 'publication.path_conflict'
	| import('./deployments').DeploymentOutcomeCode;

const knownCodes = new Set<string>([
	'auth.registration_started',
	'auth.registration_completed',
	'auth.login_started',
	'auth.login_completed',
	'auth.login_failed',
	'auth.session_valid',
	'auth.session_required',
	'auth.logged_out',
	'auth.password_change_started',
	'auth.password_changed',
	'auth.recent_login_required',
	'auth.protocol_invalid',
	'auth.registration_expired',
	'auth.registration_conflict',
	'auth.capacity_exceeded',
	'request.https_required',
	'request.json_required',
	'request.policy_rejected',
	'request.origin_rejected',
	'request.rate_limited',
	'service.unavailable',
	'service.internal_error',
	...repositoryOutcomeCodes,
	'project.listed',
	'project.created',
	'project.refs_listed',
	'project.refreshed',
	'project.key_assigned',
	'project.cache_cleared',
	'git.target_unavailable',
	'git.authentication_failed',
	'ssh_host.approval_required',
	'request.invalid',
	'request.not_found',
	'operation.capacity_exceeded',
	'operation.upstream_failed',
	'project.cache_recovery_required',
	'project.deleted',
	'project.has_deployments',
	'operation.cleanup_pending',
	'publication.recovery_required',
	'publication.path_conflict',
	'deployment.listed',
	'deployment.root_loaded',
	'deployment.created',
	'deployment.queued',
	'deployment.hard_redeploy_queued',
	'deployment.target_changed',
	'deployment.history_loaded',
	'deployment.rollback_queued',
	'deployment.paused',
	'deployment.resumed',
	'deployment.moved',
	'deployment.deleted',
	'job.listed'
]);

export interface Outcome<T> {
	code: OutcomeCode | (string & {});
	message: string;
	data: T;
}

export interface ProtocolReply {
	value: string;
	transaction?: string;
}

interface EventSocket {
	addEventListener(
		type: 'open' | 'message' | 'close' | 'error',
		listener: (event: Event) => void
	): void;
	close(): void;
}

interface SDKAdapters {
	fetch?: typeof fetch;
	onSessionLost?: () => void;
	onSessionEnded?: () => void;
	webSocket?: (url: string) => EventSocket;
	setTimeout?: (callback: () => void, delay: number) => ReturnType<typeof setTimeout>;
	clearTimeout?: (timer: ReturnType<typeof setTimeout>) => void;
	random?: () => number;
}

export type SDKError =
	| { kind: 'server'; status: number; code: string; message: string; data: unknown }
	| { kind: 'network'; code: 'sdk.network'; message: string }
	| { kind: 'invalid_response'; code: 'sdk.invalid_response'; message: string }
	| { kind: 'cancelled'; code: 'sdk.cancelled'; message: string };

export class BrowserSDKError extends Error {
	constructor(readonly outcome: SDKError) {
		super(outcome.message);
		this.name = 'BrowserSDKError';
	}
}

export function serverErrorCode(error: unknown): string | undefined {
	return error instanceof BrowserSDKError && error.outcome.kind === 'server'
		? error.outcome.code
		: undefined;
}

const safeFallback = 'The request could not be completed. Try again.';
const safeServerMessage = (value: unknown): value is string =>
	typeof value === 'string' &&
	value.length > 0 &&
	value.length <= 120 &&
	!/[\u0000-\u001f\u007f]/.test(value);
const isObject = (value: unknown): value is Record<string, unknown> =>
	typeof value === 'object' && value !== null && !Array.isArray(value);
const isOutcome = (value: unknown): value is Outcome<unknown> =>
	isObject(value) &&
	typeof value.code === 'string' &&
	value.code.length > 0 &&
	safeServerMessage(value.message) &&
	Object.hasOwn(value, 'data');

function cancelled(cause: unknown): boolean {
	return cause instanceof DOMException && cause.name === 'AbortError';
}

export function createBrowserSDK(adapters: SDKAdapters = {}) {
	const transport: typeof fetch = adapters.fetch ?? ((input, init) => fetch(input, init));
	let sessionLossReported = false;
	const subscribers = new Set<() => void>();
	let eventSocket: EventSocket | undefined;
	let reconnectTimer: ReturnType<typeof setTimeout> | undefined;
	let healthyTimer: ReturnType<typeof setTimeout> | undefined;
	let reconnectDelay = 1000;
	let disposedEvents = false;
	let eventReady = false;
	let eventEpoch: string | undefined;
	let eventSequence = 0;
	let sessionProbeInFlight = false;
	const schedule = adapters.setTimeout ?? ((callback, delay) => setTimeout(callback, delay));
	const unschedule = adapters.clearTimeout ?? ((timer) => clearTimeout(timer));
	const random = adapters.random ?? Math.random;

	function notifyInvalidation(): void {
		for (const listener of [...subscribers]) listener();
	}

	function reportEventSessionLoss(): void {
		if (sessionLossReported) return;
		sessionLossReported = true;
		adapters.onSessionLost?.();
	}

	async function probeEventSession(): Promise<void> {
		if (sessionProbeInFlight || sessionLossReported) return;
		sessionProbeInFlight = true;
		try {
			await request(
				'/api/session',
				'GET',
				undefined,
				isSession,
				undefined,
				true,
				'auth.session_valid'
			);
		} catch {
			// A network failure is not evidence that the session ended.
		} finally {
			sessionProbeInFlight = false;
			if (!sessionLossReported) scheduleReconnect();
		}
	}

	function connectEvents(): void {
		if (eventSocket || subscribers.size === 0 || disposedEvents) return;
		if (typeof window === 'undefined' && !adapters.webSocket) return;
		const protocol =
			typeof window !== 'undefined' && window.location.protocol === 'https:' ? 'wss:' : 'ws:';
		const host = typeof window !== 'undefined' ? window.location.host : 'localhost';
		let socket: EventSocket;
		try {
			socket = adapters.webSocket
				? adapters.webSocket(`${protocol}//${host}/api/events`)
				: new WebSocket(`${protocol}//${host}/api/events`);
		} catch {
			scheduleReconnect();
			return;
		}
		eventSocket = socket;
		eventReady = false;
		socket.addEventListener('message', (event) => {
			if (eventSocket !== socket || typeof (event as MessageEvent).data !== 'string') return;
			let frame: unknown;
			try {
				frame = JSON.parse((event as MessageEvent<string>).data);
			} catch {
				socket.close();
				return;
			}
			if (!isObject(frame) || typeof frame.code !== 'string') {
				socket.close();
				return;
			}
			if (frame.code === 'events.ready') {
				if (
					!isObject(frame.data) ||
					typeof frame.data.epoch !== 'string' ||
					frame.data.epoch.length === 0 ||
					!Number.isSafeInteger(frame.data.sequence) ||
					(frame.data.sequence as number) < 0
				) {
					socket.close();
					return;
				}
				eventReady = true;
				eventEpoch = frame.data.epoch;
				eventSequence = frame.data.sequence as number;
				if (healthyTimer) unschedule(healthyTimer);
				healthyTimer = schedule(() => {
					healthyTimer = undefined;
					reconnectDelay = 1000;
				}, 30_000);
				notifyInvalidation();
				return;
			}
			if (frame.code === 'events.invalidate') {
				if (
					!isObject(frame.data) ||
					typeof frame.data.epoch !== 'string' ||
					frame.data.epoch.length === 0 ||
					!Number.isSafeInteger(frame.data.sequence) ||
					(frame.data.sequence as number) < 1 ||
					!Array.isArray(frame.data.resources) ||
					!frame.data.resources.every(
						(resource) =>
							isObject(resource) &&
							typeof resource.kind === 'string' &&
							resource.kind.length > 0 &&
							(resource.projectId === undefined || typeof resource.projectId === 'string') &&
							(resource.deploymentId === undefined || typeof resource.deploymentId === 'string')
					)
				) {
					socket.close();
					return;
				}
				const sequence = frame.data.sequence as number;
				if (frame.data.epoch === eventEpoch && sequence <= eventSequence) return;
				notifyInvalidation();
				eventEpoch = frame.data.epoch;
				eventSequence = sequence;
				return;
			}
			// Future event codes are not understood, so synchronize from HTTP.
			notifyInvalidation();
		});
		socket.addEventListener('close', (event) => {
			if (eventSocket === socket) eventSocket = undefined;
			const wasReady = eventReady;
			eventReady = false;
			if (healthyTimer) unschedule(healthyTimer);
			healthyTimer = undefined;
			const closeCode = (event as CloseEvent).code;
			if (closeCode === 4401) {
				reportEventSessionLoss();
				return;
			}
			if (!wasReady) {
				void probeEventSession();
				return;
			}
			scheduleReconnect();
		});
		socket.addEventListener('error', () => {
			// Browsers follow errors with close; reconnect is scheduled there.
		});
	}

	function scheduleReconnect(): void {
		if (reconnectTimer || subscribers.size === 0 || disposedEvents) return;
		const jitter = Math.max(0, Math.min(1, random()));
		const delay = Math.max(
			1000,
			Math.min(30_000, Math.round(reconnectDelay * (0.8 + jitter * 0.4)))
		);
		reconnectDelay = Math.min(30_000, reconnectDelay * 2);
		reconnectTimer = schedule(() => {
			reconnectTimer = undefined;
			connectEvents();
		}, delay);
	}

	function subscribeToInvalidations(onInvalidate: () => void): () => void {
		subscribers.add(onInvalidate);
		disposedEvents = false;
		connectEvents();
		return () => {
			subscribers.delete(onInvalidate);
			if (subscribers.size !== 0) return;
			disposedEvents = true;
			if (reconnectTimer) unschedule(reconnectTimer);
			reconnectTimer = undefined;
			if (healthyTimer) unschedule(healthyTimer);
			healthyTimer = undefined;
			const socket = eventSocket;
			eventSocket = undefined;
			socket?.close();
		};
	}

	async function request<T>(
		path: string,
		method: 'GET' | 'POST',
		body: unknown,
		validate: (data: unknown) => data is T,
		signal?: AbortSignal,
		protectedRequest = false,
		expectedCode?: string
	): Promise<Outcome<T>> {
		let response: Response;
		try {
			response = await transport(path, {
				method,
				credentials: 'same-origin',
				headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
				body: body === undefined ? undefined : JSON.stringify(body),
				signal
			});
		} catch (cause) {
			const isCancelled = signal?.aborted || cancelled(cause);
			if (isCancelled) {
				throw new BrowserSDKError({
					kind: 'cancelled',
					code: 'sdk.cancelled',
					message: 'Request cancelled.'
				});
			}
			throw new BrowserSDKError({
				kind: 'network',
				code: 'sdk.network',
				message: safeFallback
			});
		}

		let parsed: unknown;
		try {
			parsed = await response.json();
		} catch {
			if (signal?.aborted) {
				throw new BrowserSDKError({
					kind: 'cancelled',
					code: 'sdk.cancelled',
					message: 'Request cancelled.'
				});
			}
			throw new BrowserSDKError({
				kind: 'invalid_response',
				code: 'sdk.invalid_response',
				message: safeFallback
			});
		}
		if (!isOutcome(parsed)) {
			throw new BrowserSDKError({
				kind: 'invalid_response',
				code: 'sdk.invalid_response',
				message: safeFallback
			});
		}
		if (!response.ok) {
			if (protectedRequest && parsed.code === 'auth.session_required') reportEventSessionLoss();
			throw new BrowserSDKError({
				kind: 'server',
				status: response.status,
				code: parsed.code,
				message: parsed.message,
				data: parsed.data
			});
		}
		if (!validate(parsed.data)) {
			throw new BrowserSDKError({
				kind: 'invalid_response',
				code: 'sdk.invalid_response',
				message: safeFallback
			});
		}
		if (expectedCode && knownCodes.has(parsed.code) && parsed.code !== expectedCode) {
			throw new BrowserSDKError({
				kind: 'invalid_response',
				code: 'sdk.invalid_response',
				message: safeFallback
			});
		}
		return { code: parsed.code, message: parsed.message, data: parsed.data };
	}

	const isNull = (value: unknown): value is null => value === null;
	const isProtocolReply = (value: unknown): value is ProtocolReply =>
		isObject(value) &&
		typeof value.value === 'string' &&
		typeof value.transaction === 'string' &&
		value.transaction.length > 0;
	const isPasswordChangeReply = (value: unknown): value is { value: string } =>
		isObject(value) && typeof value.value === 'string';
	const isSession = (value: unknown): value is { authenticated: true } =>
		isObject(value) && value.authenticated === true;
	const repositoryOperations = createRepositoryOperations(request);
	return {
		...repositoryOperations,
		events: { subscribe: subscribeToInvalidations },
		deployments: createDeploymentSDK(request),
		auth: {
			registerStart: (input: { grant: string; value: string }, signal?: AbortSignal) =>
				request(
					'/opaque/register/start',
					'POST',
					input,
					isProtocolReply,
					signal,
					false,
					'auth.registration_started'
				),
			registerFinish: (input: { transaction: string; value: string }, signal?: AbortSignal) =>
				request(
					'/opaque/register/finish',
					'POST',
					input,
					isNull,
					signal,
					false,
					'auth.registration_completed'
				),
			async recoveryFinish(input: { transaction: string; value: string }, signal?: AbortSignal) {
				const outcome = await request(
					'/opaque/register/finish',
					'POST',
					input,
					isNull,
					signal,
					false,
					'auth.registration_completed'
				);
				if (outcome.code === 'auth.registration_completed') {
					sessionLossReported = true;
					adapters.onSessionEnded?.();
				}
				return outcome;
			},
			loginStart: (input: { value: string }, signal?: AbortSignal) =>
				request(
					'/opaque/login/start',
					'POST',
					input,
					isProtocolReply,
					signal,
					false,
					'auth.login_started'
				),
			loginFinish: (input: { transaction: string; value: string }, signal?: AbortSignal) =>
				request(
					'/opaque/login/finish',
					'POST',
					input,
					isNull,
					signal,
					false,
					'auth.login_completed'
				).then((outcome) => {
					if (outcome.code === 'auth.login_completed') sessionLossReported = false;
					return outcome;
				}),
			session: (signal?: AbortSignal) =>
				request('/api/session', 'GET', undefined, isSession, signal, true, 'auth.session_valid'),
			async logout(signal?: AbortSignal) {
				const outcome = await request(
					'/api/logout',
					'POST',
					undefined,
					isNull,
					signal,
					true,
					'auth.logged_out'
				);
				if (outcome.code === 'auth.logged_out') {
					sessionLossReported = true;
					adapters.onSessionEnded?.();
				}
				return outcome;
			},
			passwordChangeStart: (input: { value: string }, signal?: AbortSignal) =>
				request(
					'/opaque/password/change/start',
					'POST',
					input,
					isPasswordChangeReply,
					signal,
					true,
					'auth.password_change_started'
				),
			async passwordChangeFinish(input: { value: string }, signal?: AbortSignal) {
				const outcome = await request(
					'/opaque/password/change/finish',
					'POST',
					input,
					isNull,
					signal,
					true,
					'auth.password_changed'
				);
				if (outcome.code === 'auth.password_changed') {
					sessionLossReported = true;
					adapters.onSessionEnded?.();
				}
				return outcome;
			}
		},
		projects: createProjectOperations((path, method, body, validate, signal, expectedCode) =>
			request(path, method, body, validate, signal, true, expectedCode as OutcomeCode | undefined)
		)
	};
}

const dispatchSessionEvent = (name: string) => {
	if (typeof window !== 'undefined') window.dispatchEvent(new Event(name));
};

export const sdk = createBrowserSDK({
	onSessionLost: () => dispatchSessionEvent('deploy-manager:session-lost'),
	onSessionEnded: () => dispatchSessionEvent('deploy-manager:session-ended')
});

export default sdk;
