export type RepositoryOrigin = {
	kind: 'ssh' | 'https' | 'http' | 'unknown';
	host: string | null;
	port: number | null;
	username: string | null;
	path: string | null;
};

/**
 * Extract origin details without validating whether the backend accepts the repository.
 * Ports include protocol defaults; absent usernames are null (no inferred SSH user).
 * URL usernames and paths retain percent-encoding and URL paths retain their leading slash.
 * SCP-style paths remain relative or absolute as supplied. Passwords are never returned.
 * Malformed recognized URLs retain their kind, with null connection details.
 */
export function parseRepositoryUrl(repositoryUrl: string): RepositoryOrigin {
	const scheme = repositoryUrl.match(/^(ssh|https|http):\/\//i)?.[1].toLowerCase();
	const kind = (scheme ?? 'unknown') as RepositoryOrigin['kind'];
	const empty: RepositoryOrigin = {
		kind,
		host: null,
		port: null,
		username: null,
		path: null
	};
	try {
		const parsed = new URL(repositoryUrl);
		const protocol = parsed.protocol.toLowerCase();
		if ((protocol === 'ssh:' || protocol === 'https:' || protocol === 'http:') && parsed.hostname) {
			return {
				kind: protocol.slice(0, -1) as 'ssh' | 'https' | 'http',
				host: parsed.hostname.toLowerCase(),
				port: Number(parsed.port || (protocol === 'ssh:' ? 22 : protocol === 'https:' ? 443 : 80)),
				username: parsed.username || null,
				path: parsed.pathname
			};
		}
	} catch {
		// SCP-style origins are not valid URLs; parse those below.
	}
	const scp = repositoryUrl.match(/^([^@\s/:]+)@([a-zA-Z0-9.-]+):(.+)$/);
	if (scp) {
		return {
			kind: 'ssh',
			host: scp[2].toLowerCase(),
			port: 22,
			username: scp[1],
			path: scp[3]
		};
	}
	return empty;
}
