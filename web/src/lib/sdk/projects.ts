export type Project = {
	id: string;
	name: string;
	url: string;
	keyId?: string;
	lastFetched: string | null;
	gitCacheBytes: number;
	lfsCacheBytes: number;
	warning?: string;
};

export type ProjectRef = { kind: 'branch' | 'tag'; name: string; commit: string };

export type ProjectCreate = { name: string; url: string; keyId: string };
export type ProjectRefresh = { lastFetched: string; warning: string };
export type ProjectOperation = <T>(
	path: string,
	method: 'GET' | 'POST',
	body: unknown,
	validate: (data: unknown) => data is T,
	signal?: AbortSignal,
	expectedCode?: string
) => Promise<{ code: string; message: string; data: T }>;

const object = (value: unknown): value is Record<string, unknown> =>
	typeof value === 'object' && value !== null && !Array.isArray(value);
const isProject = (value: unknown): value is Project =>
	object(value) &&
	typeof value.id === 'string' &&
	typeof value.name === 'string' &&
	typeof value.url === 'string' &&
	(value.keyId === undefined || typeof value.keyId === 'string') &&
	(value.lastFetched === null || typeof value.lastFetched === 'string') &&
	typeof value.gitCacheBytes === 'number' &&
	typeof value.lfsCacheBytes === 'number' &&
	(value.warning === undefined || typeof value.warning === 'string');
const isProjects = (value: unknown): value is Project[] =>
	Array.isArray(value) && value.every(isProject);
const isRefs = (value: unknown): value is ProjectRef[] =>
	Array.isArray(value) &&
	value.every(
		(ref) =>
			object(ref) &&
			(ref.kind === 'branch' || ref.kind === 'tag') &&
			typeof ref.name === 'string' &&
			typeof ref.commit === 'string'
	);
const isProjectRefresh = (value: unknown): value is ProjectRefresh =>
	object(value) && typeof value.lastFetched === 'string' && typeof value.warning === 'string';
const isNull = (value: unknown): value is null => value === null;

export function createProjectOperations(request: ProjectOperation) {
	return {
		list: (signal?: AbortSignal) =>
			request('/api/projects', 'GET', undefined, isProjects, signal, 'project.listed'),
		create: (input: ProjectCreate, signal?: AbortSignal) =>
			request('/api/projects', 'POST', input, isProject, signal, 'project.created'),
		refs: (id: string, signal?: AbortSignal) =>
			request(
				`/api/projects/${encodeURIComponent(id)}/refs`,
				'GET',
				undefined,
				isRefs,
				signal,
				'project.refs_listed'
			),
		refresh: (id: string, signal?: AbortSignal) =>
			request(
				`/api/projects/${encodeURIComponent(id)}/refresh`,
				'POST',
				{},
				isProjectRefresh,
				signal,
				'project.refreshed'
			),
		assignKey: (id: string, input: { keyId: string }, signal?: AbortSignal) =>
			request(
				`/api/projects/${encodeURIComponent(id)}/key`,
				'POST',
				input,
				isNull,
				signal,
				'project.key_assigned'
			),
		clearCache: (id: string, signal?: AbortSignal) =>
			request(
				`/api/projects/${encodeURIComponent(id)}/cache/recreate`,
				'POST',
				{ confirm: true },
				isNull,
				signal,
				'project.cache_cleared'
			),
		delete: (id: string, signal?: AbortSignal) =>
			request(
				`/api/projects/${encodeURIComponent(id)}/delete`,
				'POST',
				{ confirm: true },
				isNull,
				signal,
				'project.deleted'
			)
	};
}
