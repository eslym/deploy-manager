import type { Outcome } from './index';

export type DeploymentOutcomeCode =
	| 'deployment.listed'
	| 'deployment.root_loaded'
	| 'deployment.created'
	| 'deployment.queued'
	| 'deployment.hard_redeploy_queued'
	| 'deployment.target_changed'
	| 'deployment.history_loaded'
	| 'deployment.rollback_queued'
	| 'deployment.paused'
	| 'deployment.resumed'
	| 'deployment.moved'
	| 'deployment.deleted'
	| 'job.listed';

export interface Deployment {
	id: string;
	projectId: string;
	name: string;
	commit: string;
	targetKind: 'branch' | 'tag' | 'commit';
	targetName?: string;
	automationPaused: boolean;
	liveCommit?: string;
	status: string;
	error?: string;
	publicationPath: string;
	createdAt: string;
	updatedAt: string;
}

export interface DeploymentJob {
	id: string;
	deploymentId: string;
	commit: string;
	force: boolean;
	status: string;
	log: string;
	createdAt: string;
	updatedAt: string;
}

export interface DeploymentHistoryCommit {
	id: string;
	shortId: string;
	subject: string;
	author: string;
	date: string;
}

export interface DeploymentHistory {
	commits: DeploymentHistoryCommit[];
	page: number;
	hasMore: boolean;
}

type Send = <T>(
	path: string,
	method: 'GET' | 'POST',
	body: unknown,
	validate: (data: unknown) => data is T,
	signal?: AbortSignal,
	protectedRequest?: boolean,
	expectedCode?: string
) => Promise<Outcome<T>>;

const object = (value: unknown): value is Record<string, unknown> =>
	typeof value === 'object' && value !== null && !Array.isArray(value);
const text = (value: unknown): value is string => typeof value === 'string';
const bool = (value: unknown): value is boolean => typeof value === 'boolean';
const deployment = (value: unknown): value is Deployment =>
	object(value) &&
	text(value.id) &&
	text(value.projectId) &&
	text(value.name) &&
	text(value.commit) &&
	(value.targetKind === 'branch' || value.targetKind === 'tag' || value.targetKind === 'commit') &&
	bool(value.automationPaused) &&
	text(value.status) &&
	text(value.publicationPath) &&
	text(value.createdAt) &&
	text(value.updatedAt) &&
	(value.targetName === undefined || text(value.targetName)) &&
	(value.liveCommit === undefined || text(value.liveCommit)) &&
	(value.error === undefined || text(value.error));
const job = (value: unknown): value is DeploymentJob =>
	object(value) &&
	text(value.id) &&
	text(value.deploymentId) &&
	text(value.commit) &&
	bool(value.force) &&
	text(value.status) &&
	text(value.log) &&
	text(value.createdAt) &&
	text(value.updatedAt);
const historyCommit = (value: unknown): value is DeploymentHistoryCommit =>
	object(value) &&
	text(value.id) &&
	text(value.shortId) &&
	text(value.subject) &&
	text(value.author) &&
	text(value.date);
const arrayOf =
	<T>(validate: (value: unknown) => value is T) =>
	(value: unknown): value is T[] =>
		Array.isArray(value) && value.every(validate);
const nullData = (value: unknown): value is null => value === null;
const queuedData = (value: unknown): value is { jobId: string; status: 'queued' } =>
	object(value) && text(value.jobId) && value.status === 'queued';

export function createDeploymentSDK(send: Send) {
	const read = <T>(
		path: string,
		validate: (data: unknown) => data is T,
		signal?: AbortSignal,
		code?: DeploymentOutcomeCode
	) => send(path, 'GET', undefined, validate, signal, true, code);
	const mutate = <T>(
		path: string,
		body: unknown,
		validate: (data: unknown) => data is T,
		signal?: AbortSignal,
		code?: DeploymentOutcomeCode
	) => send(path, 'POST', body, validate, signal, true, code);

	return {
		list: (signal?: AbortSignal) =>
			read('/api/deployments', arrayOf(deployment), signal, 'deployment.listed'),
		root: (signal?: AbortSignal) =>
			read(
				'/api/deployments/root',
				(value): value is { publicationRoot: string } =>
					object(value) && text(value.publicationRoot),
				signal,
				'deployment.root_loaded'
			),
		create: (
			input: {
				projectId: string;
				name: string;
				publicationPath?: string;
				targetKind?: 'branch' | 'tag' | 'commit';
				targetName?: string;
				commit?: string;
			},
			signal?: AbortSignal
		) =>
			mutate(
				'/api/deployments',
				input,
				(
					value
				): value is { id: string; jobId: string; status: 'queued'; publicationPath: string } =>
					object(value) &&
					text(value.id) &&
					text(value.jobId) &&
					value.status === 'queued' &&
					text(value.publicationPath),
				signal,
				'deployment.created'
			),
		deploy: (id: string, signal?: AbortSignal) =>
			mutate(
				`/api/deployments/${encodeURIComponent(id)}/deploy`,
				{},
				queuedData,
				signal,
				'deployment.queued'
			),
		hardRedeploy: (id: string, signal?: AbortSignal) =>
			mutate(
				`/api/deployments/${encodeURIComponent(id)}/hard-redeploy`,
				{},
				queuedData,
				signal,
				'deployment.hard_redeploy_queued'
			),
		changeTarget: (
			id: string,
			input: { targetKind: 'branch' | 'tag' | 'commit'; targetName?: string; commit?: string },
			signal?: AbortSignal
		) =>
			mutate(
				`/api/deployments/${encodeURIComponent(id)}/target`,
				input,
				queuedData,
				signal,
				'deployment.target_changed'
			),
		jobs: (id: string, signal?: AbortSignal) =>
			read(`/api/deployments/${encodeURIComponent(id)}/jobs`, arrayOf(job), signal, 'job.listed'),
		history: (
			id: string,
			query: { page?: number; limit?: number; search?: string } = {},
			signal?: AbortSignal
		) => {
			const params = new URLSearchParams();
			if (query.page !== undefined) params.set('page', String(query.page));
			if (query.limit !== undefined) params.set('limit', String(query.limit));
			if (query.search !== undefined) params.set('search', query.search);
			const suffix = params.size ? `?${params}` : '';
			return read(
				`/api/deployments/${encodeURIComponent(id)}/history${suffix}`,
				(value): value is DeploymentHistory =>
					object(value) &&
					arrayOf(historyCommit)(value.commits) &&
					typeof value.page === 'number' &&
					bool(value.hasMore),
				signal,
				'deployment.history_loaded'
			);
		},
		rollback: (id: string, input: { commit: string; permanent?: boolean }, signal?: AbortSignal) =>
			mutate(
				`/api/deployments/${encodeURIComponent(id)}/rollback`,
				input,
				queuedData,
				signal,
				'deployment.rollback_queued'
			),
		pause: (id: string, signal?: AbortSignal) =>
			mutate(
				`/api/deployments/${encodeURIComponent(id)}/pause`,
				{},
				(value): value is { automationPaused: true } =>
					object(value) && value.automationPaused === true,
				signal,
				'deployment.paused'
			),
		resume: (id: string, signal?: AbortSignal) =>
			mutate(
				`/api/deployments/${encodeURIComponent(id)}/resume`,
				{},
				(value): value is { automationPaused: false; jobId: string; status: 'queued' } =>
					object(value) && value.automationPaused === false && queuedData(value),
				signal,
				'deployment.resumed'
			),
		move: (
			id: string,
			input: { publicationPath: string; expectedPublicationPath: string; confirm: true },
			signal?: AbortSignal
		) =>
			mutate(
				`/api/deployments/${encodeURIComponent(id)}/move`,
				input,
				(value): value is { publicationPath: string } =>
					object(value) && text(value.publicationPath),
				signal,
				'deployment.moved'
			),
		remove: (id: string, signal?: AbortSignal) =>
			mutate(
				`/api/deployments/${encodeURIComponent(id)}/delete`,
				{ confirm: true },
				nullData,
				signal,
				'deployment.deleted'
			)
	};
}
