export type RepositoryOutcomeCode =
	| 'repository_key.listed'
	| 'repository_key.created'
	| 'repository_key.deleted'
	| 'repository_key.in_use'
	| 'ssh_host.listed'
	| 'ssh_host.approved'
	| 'ssh_host.revoked'
	| 'ssh_host.observation_changed'
	| 'lfs.settings_loaded'
	| 'lfs.settings_saved'
	| 'webhook.settings_loaded'
	| 'webhook.secret_rotated'
	| 'webhook.accepted'
	| 'webhook.signature_invalid'
	| 'webhook.not_configured';

export interface RepositoryKey {
	id: string;
	name: string;
	algorithm: string;
	bits: number;
	fingerprint: string;
	publicKey: string;
	createdAt: string;
}

export interface SSHHost {
	host: string;
	port: number;
	fingerprint: string;
	warning: string;
	trusted: boolean;
}
export type SSHHostObservation = Pick<SSHHost, 'host' | 'port' | 'fingerprint'>;

export interface LFSSettings {
	endpoint: string;
	objectOrigins: string[];
	allowInternal: boolean;
	internalAddresses?: Record<string, string[]>;
}

export interface LFSSettingsResponse {
	settings: LFSSettings;
	suggestedEndpoint: string;
}

export interface WebhookSettings {
	configured: boolean;
	url: string;
}

export interface RotatedWebhookSecret {
	url: string;
	secret: string;
}

type Request = <T>(
	path: string,
	method: 'GET' | 'POST',
	body: unknown,
	validate: (data: unknown) => data is T,
	signal?: AbortSignal,
	protectedRequest?: boolean,
	expectedCode?: RepositoryOutcomeCode
) => Promise<{ code: string; message: string; data: T }>;

const isObject = (value: unknown): value is Record<string, unknown> =>
	typeof value === 'object' && value !== null && !Array.isArray(value);
const isString = (value: unknown): value is string => typeof value === 'string';
const isRepositoryKey = (value: unknown): value is RepositoryKey =>
	isObject(value) &&
	isString(value.id) &&
	isString(value.name) &&
	isString(value.algorithm) &&
	typeof value.bits === 'number' &&
	isString(value.fingerprint) &&
	isString(value.publicKey) &&
	isString(value.createdAt);
const isSSHHost = (value: unknown): value is SSHHost =>
	isObject(value) &&
	isString(value.host) &&
	typeof value.port === 'number' &&
	isString(value.fingerprint) &&
	isString(value.warning) &&
	typeof value.trusted === 'boolean';
const isLFSSettings = (value: unknown): value is LFSSettings =>
	isObject(value) &&
	isString(value.endpoint) &&
	Array.isArray(value.objectOrigins) &&
	value.objectOrigins.every(isString) &&
	typeof value.allowInternal === 'boolean' &&
	(value.internalAddresses === undefined ||
		(isObject(value.internalAddresses) &&
			Object.values(value.internalAddresses).every(
				(addresses) => Array.isArray(addresses) && addresses.every(isString)
			)));
const isLFSSettingsResponse = (value: unknown): value is LFSSettingsResponse =>
	isObject(value) && isLFSSettings(value.settings) && isString(value.suggestedEndpoint);
const isWebhookSettings = (value: unknown): value is WebhookSettings =>
	isObject(value) && typeof value.configured === 'boolean' && isString(value.url);
const isRotatedWebhookSecret = (value: unknown): value is RotatedWebhookSecret =>
	isObject(value) && isString(value.url) && isString(value.secret);
const isNull = (value: unknown): value is null => value === null;
const isKeyList = (value: unknown): value is RepositoryKey[] =>
	Array.isArray(value) && value.every(isRepositoryKey);
const isHostList = (value: unknown): value is SSHHost[] =>
	Array.isArray(value) && value.every(isSSHHost);

export function createRepositoryOperations(request: Request) {
	return {
		repositoryKeys: {
			list: (signal?: AbortSignal) =>
				request(
					'/api/repository-keys',
					'GET',
					undefined,
					isKeyList,
					signal,
					true,
					'repository_key.listed'
				),
			create: (
				input: { name: string; generate: true } | { name: string; privateKey: string },
				signal?: AbortSignal
			) =>
				request(
					'/api/repository-keys',
					'POST',
					input,
					isRepositoryKey,
					signal,
					true,
					'repository_key.created'
				),
			delete: (id: string, signal?: AbortSignal) =>
				request(
					`/api/repository-keys/${encodeURIComponent(id)}/delete`,
					'POST',
					undefined,
					isNull,
					signal,
					true,
					'repository_key.deleted'
				)
		},
		sshHosts: {
			list: (signal?: AbortSignal) =>
				request('/api/ssh-hosts', 'GET', undefined, isHostList, signal, true, 'ssh_host.listed'),
			approve: (input: SSHHostObservation & { confirm: true }, signal?: AbortSignal) =>
				request('/api/ssh-hosts/approve', 'POST', input, isNull, signal, true, 'ssh_host.approved'),
			revoke: (input: SSHHostObservation & { confirm: true }, signal?: AbortSignal) =>
				request('/api/ssh-hosts/revoke', 'POST', input, isNull, signal, true, 'ssh_host.revoked')
		},
		lfs: {
			settings: (projectId: string, signal?: AbortSignal) =>
				request(
					`/api/projects/${encodeURIComponent(projectId)}/lfs`,
					'GET',
					undefined,
					isLFSSettingsResponse,
					signal,
					true,
					'lfs.settings_loaded'
				),
			save: (
				projectId: string,
				input: LFSSettings & { confirmation: string; internalConfirmation?: string },
				signal?: AbortSignal
			) =>
				request(
					`/api/projects/${encodeURIComponent(projectId)}/lfs`,
					'POST',
					input,
					isNull,
					signal,
					true,
					'lfs.settings_saved'
				)
		},
		webhooks: {
			settings: (projectId: string, provider: 'github' | 'gitea', signal?: AbortSignal) =>
				request(
					`/api/projects/${encodeURIComponent(projectId)}/${provider}-webhook`,
					'GET',
					undefined,
					isWebhookSettings,
					signal,
					true,
					'webhook.settings_loaded'
				),
			rotate: (projectId: string, provider: 'github' | 'gitea', signal?: AbortSignal) =>
				request(
					`/api/projects/${encodeURIComponent(projectId)}/${provider}-webhook`,
					'POST',
					{ rotate: true },
					isRotatedWebhookSecret,
					signal,
					true,
					'webhook.secret_rotated'
				)
		}
	};
}
