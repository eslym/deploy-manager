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

export type EventResource =
	| { kind: 'projects' }
	| { kind: 'refs' | 'lfs_settings' | 'webhook_settings'; projectId: string }
	| { kind: 'deployments'; projectId?: string; deploymentId?: string }
	| { kind: 'jobs' | 'history'; deploymentId: string }
	| { kind: 'repository_keys' | 'ssh_hosts' };

export type ResourceIdentity =
	| { kind: 'deployments' }
	| { kind: 'deployment'; projectId: string; deploymentId: string }
	| { kind: 'root' }
	| { kind: 'projects' }
	| { kind: 'refs'; projectId: string }
	| { kind: 'repository_keys' | 'ssh_hosts' }
	| { kind: 'lfs_settings' | 'webhook_settings'; projectId: string }
	| { kind: 'jobs'; deploymentId: string }
	| {
			kind: 'history';
			deploymentId: string;
			projectId?: string;
			search: string;
			page: number;
			limit: number;
	  };

export interface ResourceSubscription<T> {
	identity: ResourceIdentity;
	read: () => Promise<T>;
	onValue: (value: T | undefined, error?: unknown) => void;
}

interface ResourceReadEntry {
	identity: ResourceIdentity;
	read: () => Promise<unknown>;
	listeners: Set<ResourceSubscription<unknown>['onValue']>;
	reading: boolean;
	dirty: boolean;
	generation: number;
}

interface SDKAdapters {
	fetch?: typeof fetch;
	onSessionLost?: () => void;
	onSessionEnded?: () => void;
	webSocket?: (url: string) => EventSocket;
	setTimeout?: (callback: () => void, delay: number) => ReturnType<typeof setTimeout>;
	clearTimeout?: (timer: ReturnType<typeof setTimeout>) => void;
	random?: () => number;
	isOnline?: () => boolean;
	isVisible?: () => boolean;
	lifecycle?: (handlers: {
		foreground: () => void;
		online: () => void;
		offline: () => void;
	}) => () => void;
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
const isEventResource = (value: unknown): value is EventResource => {
	if (!isObject(value) || typeof value.kind !== 'string') return false;
	switch (value.kind) {
		case 'projects':
		case 'repository_keys':
		case 'ssh_hosts':
			return true;
		case 'refs':
		case 'lfs_settings':
		case 'webhook_settings':
			return typeof value.projectId === 'string' && value.projectId.length > 0;
		case 'jobs':
		case 'history':
			return typeof value.deploymentId === 'string' && value.deploymentId.length > 0;
		case 'deployments':
			return (
				(value.projectId === undefined ||
					(typeof value.projectId === 'string' && value.projectId.length > 0)) &&
				(value.deploymentId === undefined ||
					(typeof value.deploymentId === 'string' && value.deploymentId.length > 0))
			);
		default:
			return false;
	}
};
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
	const subscribers = new Set<(resources?: EventResource[]) => void>();
	const resourceReads = new Map<string, ResourceReadEntry>();
	let eventSocket: EventSocket | undefined;
	let reconnectTimer: ReturnType<typeof setTimeout> | undefined;
	let healthyTimer: ReturnType<typeof setTimeout> | undefined;
	let fallbackStartTimer: ReturnType<typeof setTimeout> | undefined;
	let fallbackTimer: ReturnType<typeof setTimeout> | undefined;
	let browserListenersAttached = false;
	let removeBrowserListeners: (() => void) | undefined;
	let reconnectDelay = 1000;
	let disposedEvents = false;
	let eventReady = false;
	let eventEpoch: string | undefined;
	let eventSequence = 0;
	let sessionProbeInFlight = false;
	const schedule = adapters.setTimeout ?? ((callback, delay) => setTimeout(callback, delay));
	const unschedule = adapters.clearTimeout ?? ((timer) => clearTimeout(timer));
	const random = adapters.random ?? Math.random;

	function notifyInvalidation(resources?: EventResource[]): void {
		for (const listener of [...subscribers]) listener(resources);
		for (const entry of resourceReads.values()) {
			if (!resources || resources.some((resource) => resourceMatches(entry.identity, resource))) {
				invalidateRead(entry);
			}
		}
	}

	function identityKey(identity: ResourceIdentity): string {
		switch (identity.kind) {
			case 'deployments':
			case 'deployment':
			case 'root':
			case 'projects':
			case 'repository_keys':
			case 'ssh_hosts':
				return identity.kind === 'deployment'
					? JSON.stringify([identity.kind, identity.projectId, identity.deploymentId])
					: JSON.stringify([identity.kind]);
			case 'refs':
			case 'lfs_settings':
			case 'webhook_settings':
				return JSON.stringify([identity.kind, identity.projectId]);
			case 'jobs':
				return JSON.stringify([identity.kind, identity.deploymentId]);
			case 'history':
				return JSON.stringify([
					identity.kind,
					identity.deploymentId,
					identity.projectId ?? '',
					identity.search,
					identity.page,
					identity.limit
				]);
		}
	}

	function resourceMatches(identity: ResourceIdentity, resource: EventResource): boolean {
		switch (identity.kind) {
			case 'deployments':
				return resource.kind === 'deployments' || resource.kind === 'jobs';
			case 'deployment':
				return (
					(resource.kind === 'deployments' &&
						(resource.deploymentId === undefined ||
							resource.deploymentId === identity.deploymentId) &&
						(resource.projectId === undefined || resource.projectId === identity.projectId)) ||
					(resource.kind === 'jobs' && resource.deploymentId === identity.deploymentId)
				);
			case 'root':
				return false;
			case 'projects':
				return resource.kind === 'projects' || resource.kind === 'refs';
			case 'repository_keys':
				return resource.kind === 'repository_keys';
			case 'ssh_hosts':
				return resource.kind === 'ssh_hosts';
			case 'refs':
				return resource.kind === 'refs' && resource.projectId === identity.projectId;
			case 'lfs_settings':
				return resource.kind === 'lfs_settings' && resource.projectId === identity.projectId;
			case 'webhook_settings':
				return resource.kind === 'webhook_settings' && resource.projectId === identity.projectId;
			case 'jobs':
				return resource.kind === 'jobs' && resource.deploymentId === identity.deploymentId;
			case 'history':
				return (
					(resource.kind === 'history' && resource.deploymentId === identity.deploymentId) ||
					(resource.kind === 'refs' &&
						identity.projectId !== undefined &&
						resource.projectId === identity.projectId)
				);
		}
	}

	function invalidateRead(entry: ResourceReadEntry): void {
		if (entry.reading) {
			entry.dirty = true;
			return;
		}
		void readResource(entry);
	}

	async function readResource(entry: ResourceReadEntry): Promise<void> {
		if (entry.reading || entry.listeners.size === 0) return;
		entry.reading = true;
		entry.dirty = false;
		const generation = entry.generation;
		try {
			const value = await entry.read();
			if (entry.generation === generation) {
				for (const listener of [...entry.listeners]) listener(value);
			}
		} catch (error) {
			if (entry.generation === generation) {
				for (const listener of [...entry.listeners]) listener(undefined, error);
			}
		} finally {
			entry.reading = false;
			if (entry.dirty && entry.listeners.size > 0) void readResource(entry);
			else if (
				entry.listeners.size === 0 &&
				resourceReads.get(identityKey(entry.identity)) === entry
			) {
				resourceReads.delete(identityKey(entry.identity));
			}
		}
	}

	function subscribeResource<T>(subscription: ResourceSubscription<T>): () => void {
		const key = identityKey(subscription.identity);
		let entry = resourceReads.get(key);
		if (!entry) {
			entry = {
				identity: subscription.identity,
				read: subscription.read,
				listeners: new Set(),
				reading: false,
				dirty: false,
				generation: 0
			};
			resourceReads.set(key, entry);
		}
		const listener: ResourceSubscription<unknown>['onValue'] = (value, error) =>
			error === undefined
				? subscription.onValue(value as T | undefined)
				: subscription.onValue(value as T | undefined, error);
		entry.listeners.add(listener);
		if (!entry.reading) void readResource(entry);
		const disposeEvents = subscribeToInvalidations(() => {});
		return () => {
			entry!.listeners.delete(listener);
			disposeEvents();
			if (entry!.listeners.size === 0) {
				entry!.generation++;
				entry!.dirty = entry!.reading;
				if (!entry!.reading && resourceReads.get(key) === entry) resourceReads.delete(key);
			}
		};
	}

	function refreshResources(identity?: ResourceIdentity): void {
		for (const entry of resourceReads.values()) {
			if (!identity || identityKey(entry.identity) === identityKey(identity)) {
				invalidateRead(entry);
			}
		}
	}

	function invalidateAfterMutation(path: string): void {
		if (path === '/api/projects') {
			notifyInvalidation([{ kind: 'projects' }]);
			return;
		}
		const projectResource = path.match(
			/^\/api\/projects\/([^/]+)\/(key|cache\/recreate|lfs|github-webhook|gitea-webhook|delete)$/
		);
		if (projectResource) {
			const projectId = decodeURIComponent(projectResource[1]);
			const resources: EventResource[] = [{ kind: 'projects' }];
			switch (projectResource[2]) {
				case 'key':
					resources.push({ kind: 'repository_keys' });
					break;
				case 'lfs':
					resources.push({ kind: 'lfs_settings', projectId });
					break;
				case 'github-webhook':
				case 'gitea-webhook':
					resources.push({ kind: 'webhook_settings', projectId });
					break;
				case 'delete':
					resources.push(
						{ kind: 'refs', projectId },
						{ kind: 'lfs_settings', projectId },
						{ kind: 'webhook_settings', projectId },
						{ kind: 'deployments', projectId }
					);
					break;
			}
			notifyInvalidation(resources);
			return;
		}
		if (path === '/api/repository-keys' || /^\/api\/repository-keys\/[^/]+\/delete$/.test(path)) {
			notifyInvalidation([{ kind: 'repository_keys' }, { kind: 'projects' }]);
			return;
		}
		if (path === '/api/ssh-hosts/approve' || path === '/api/ssh-hosts/revoke') {
			notifyInvalidation([{ kind: 'ssh_hosts' }, { kind: 'projects' }]);
			return;
		}
		const projectRefresh = path.match(/^\/api\/projects\/([^/]+)\/refresh$/);
		if (projectRefresh) {
			notifyInvalidation([
				{ kind: 'projects' },
				{ kind: 'refs', projectId: decodeURIComponent(projectRefresh[1]) }
			]);
			return;
		}
		if (path === '/api/deployments') {
			notifyInvalidation([{ kind: 'deployments' }]);
			return;
		}
		const match = path.match(
			/^\/api\/deployments\/([^/]+)\/(deploy|hard-redeploy|target|rollback|resume|pause|move|delete)$/
		);
		if (!match) return;
		const deploymentId = decodeURIComponent(match[1]);
		const resources: EventResource[] = [{ kind: 'deployments', deploymentId }];
		if (['deploy', 'hard-redeploy', 'target', 'rollback', 'resume', 'delete'].includes(match[2])) {
			resources.push({ kind: 'jobs', deploymentId });
		}
		notifyInvalidation(resources);
	}

	function reportEventSessionLoss(): void {
		if (sessionLossReported) return;
		sessionLossReported = true;
		disposedEvents = true;
		if (reconnectTimer) unschedule(reconnectTimer);
		reconnectTimer = undefined;
		if (healthyTimer) unschedule(healthyTimer);
		healthyTimer = undefined;
		stopFallbackPolling();
		if (fallbackStartTimer) unschedule(fallbackStartTimer);
		fallbackStartTimer = undefined;
		const socket = eventSocket;
		eventSocket = undefined;
		socket?.close();
		detachBrowserListeners();
		for (const entry of resourceReads.values()) {
			entry.generation++;
			for (const listener of [...entry.listeners]) listener(undefined);
		}
		resourceReads.clear();
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
		let unknownEventResyncRequested = false;
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
				if (fallbackStartTimer) unschedule(fallbackStartTimer);
				fallbackStartTimer = undefined;
				stopFallbackPolling();
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
					!frame.data.resources.every(isEventResource)
				) {
					socket.close();
					return;
				}
				const sequence = frame.data.sequence as number;
				if (frame.data.epoch === eventEpoch && sequence <= eventSequence) return;
				const epochChanged = frame.data.epoch !== eventEpoch;
				const sequenceGap = !epochChanged && sequence > eventSequence + 1;
				notifyInvalidation(epochChanged || sequenceGap ? undefined : frame.data.resources);
				eventEpoch = frame.data.epoch;
				eventSequence = sequence;
				return;
			}
			// Future event codes are not understood, so synchronize from HTTP.
			if (!unknownEventResyncRequested) {
				unknownEventResyncRequested = true;
				notifyInvalidation();
				socket.close();
			}
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
			scheduleFallbackStart();
			scheduleReconnect();
		});
		socket.addEventListener('error', () => {
			// Browsers follow errors with close; reconnect is scheduled there.
		});
	}

	function scheduleReconnect(): void {
		if (reconnectTimer || subscribers.size === 0 || disposedEvents) return;
		if (!(adapters.isOnline?.() ?? (typeof navigator === 'undefined' || navigator.onLine))) return;
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

	function subscribeToInvalidations(
		onInvalidate: (resources?: EventResource[]) => void
	): () => void {
		subscribers.add(onInvalidate);
		disposedEvents = false;
		if (subscribers.size === 1) {
			scheduleFallbackStart();
			attachBrowserListeners();
		}
		connectEvents();
		return () => {
			subscribers.delete(onInvalidate);
			if (subscribers.size !== 0) return;
			disposedEvents = true;
			stopFallbackPolling();
			if (fallbackStartTimer) unschedule(fallbackStartTimer);
			fallbackStartTimer = undefined;
			detachBrowserListeners();
			if (reconnectTimer) unschedule(reconnectTimer);
			reconnectTimer = undefined;
			if (healthyTimer) unschedule(healthyTimer);
			healthyTimer = undefined;
			const socket = eventSocket;
			eventSocket = undefined;
			socket?.close();
		};
	}

	function scheduleFallbackStart(): void {
		if (fallbackStartTimer || eventReady || subscribers.size === 0 || disposedEvents) return;
		fallbackStartTimer = schedule(() => {
			fallbackStartTimer = undefined;
			if (!eventReady && subscribers.size > 0 && !disposedEvents) {
				notifyInvalidation();
				startFallbackPolling();
			}
		}, 10_000);
	}

	function isVisible(): boolean {
		return (
			adapters.isVisible?.() ??
			(typeof document === 'undefined' || document.visibilityState !== 'hidden')
		);
	}

	function startFallbackPolling(): void {
		if (fallbackTimer || eventReady || disposedEvents || !isVisible()) return;
		fallbackTimer = schedule(() => {
			fallbackTimer = undefined;
			if (eventReady || disposedEvents || !isVisible()) return;
			notifyInvalidation();
			startFallbackPolling();
		}, 15_000);
	}

	function stopFallbackPolling(): void {
		if (fallbackTimer) unschedule(fallbackTimer);
		fallbackTimer = undefined;
	}

	function onForeground(): void {
		if (!isVisible() || subscribers.size === 0 || disposedEvents) return;
		notifyInvalidation();
		if (!eventSocket) connectEvents();
		if (!eventReady) startFallbackPolling();
	}

	function onOffline(): void {
		if (reconnectTimer) unschedule(reconnectTimer);
		reconnectTimer = undefined;
	}

	function onOnline(): void {
		if (subscribers.size === 0 || disposedEvents) return;
		connectEvents();
		if (!eventReady) scheduleReconnect();
	}

	function attachBrowserListeners(): void {
		if (browserListenersAttached || (!adapters.lifecycle && typeof window === 'undefined')) return;
		browserListenersAttached = true;
		if (adapters.lifecycle) {
			removeBrowserListeners = adapters.lifecycle({
				foreground: onForeground,
				online: onOnline,
				offline: onOffline
			});
			return;
		}
		window.addEventListener('focus', onForeground);
		window.addEventListener('online', onOnline);
		window.addEventListener('offline', onOffline);
		document.addEventListener('visibilitychange', onForeground);
	}

	function detachBrowserListeners(): void {
		if (!browserListenersAttached) return;
		browserListenersAttached = false;
		if (removeBrowserListeners) {
			removeBrowserListeners();
			removeBrowserListeners = undefined;
			return;
		}
		if (typeof window === 'undefined') return;
		window.removeEventListener('focus', onForeground);
		window.removeEventListener('online', onOnline);
		window.removeEventListener('offline', onOffline);
		document.removeEventListener('visibilitychange', onForeground);
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
			if (method === 'POST') invalidateAfterMutation(path);
		} catch (cause) {
			if (method === 'POST') invalidateAfterMutation(path);
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
		const outcome = { code: parsed.code, message: parsed.message, data: parsed.data };
		return outcome;
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
		resources: { subscribe: subscribeResource, refresh: refreshResources },
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
