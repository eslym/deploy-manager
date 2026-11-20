import { describe, expect, it } from 'vitest';
import { parseRepositoryUrl } from './repository-url.js';

describe('parseRepositoryUrl', () => {
	it.each([
		['ssh://git@example.com/team/repo.git', 'example.com', 22, 'git', '/team/repo.git'],
		['ssh://example.com/team/repo.git', 'example.com', 22, null, '/team/repo.git'],
		['ssh://git@EXAMPLE.COM:2222/team/repo.git', 'example.com', 2222, 'git', '/team/repo.git'],
		['SSH://git@EXAMPLE.COM/team/repo.git', 'example.com', 22, 'git', '/team/repo.git'],
		['ssh://git@[::1]:2222/team/repo.git', '[::1]', 2222, 'git', '/team/repo.git'],
		['git@EXAMPLE.COM:team/repo.git', 'example.com', 22, 'git', 'team/repo.git'],
		['deploy@git.example.com:repo.git', 'git.example.com', 22, 'deploy', 'repo.git'],
		['git@example.com:/srv/repo.git', 'example.com', 22, 'git', '/srv/repo.git']
	] as const)('extracts SSH origin details from %s', (url, host, port, username, path) => {
		expect(parseRepositoryUrl(url)).toEqual({ kind: 'ssh', host, port, username, path });
	});

	it.each([
		['https://EXAMPLE.COM/team/repo.git', 'https', 'example.com', 443, null, '/team/repo.git'],
		['https://example.com:8443/repo.git', 'https', 'example.com', 8443, null, '/repo.git'],
		['https://alice@example.com/repo.git', 'https', 'example.com', 443, 'alice', '/repo.git'],
		['https://example.com/repo@name.git', 'https', 'example.com', 443, null, '/repo@name.git'],
		['http://example.com/repo.git', 'http', 'example.com', 80, null, '/repo.git'],
		['https://example.com', 'https', 'example.com', 443, null, '/']
	] as const)('extracts web origin details from %s', (url, kind, host, port, username, path) => {
		expect(parseRepositoryUrl(url)).toEqual({ kind, host, port, username, path });
	});

	it('preserves encoded username and path without exposing passwords', () => {
		expect(parseRepositoryUrl('https://a%40b:secret@example.com/team/my%20repo.git')).toEqual({
			kind: 'https',
			host: 'example.com',
			port: 443,
			username: 'a%40b',
			path: '/team/my%20repo.git'
		});
	});

	it('keeps query and fragment separate from the repository path', () => {
		expect(parseRepositoryUrl('https://example.com/repo.git?query=value#fragment').path).toBe(
			'/repo.git'
		);
	});

	it.each([
		['ssh://', 'ssh'],
		['ssh://git@', 'ssh'],
		['ssh://example.com:invalid/repo.git', 'ssh'],
		['SSH://', 'ssh'],
		['https://', 'https'],
		['https://example.com:invalid/repo.git', 'https'],
		['http://', 'http']
	])('retains the kind of malformed URLs: %s', (url, kind) => {
		expect(parseRepositoryUrl(url)).toEqual({
			kind,
			host: null,
			port: null,
			username: null,
			path: null
		});
	});

	it.each([
		'',
		'not a URL',
		'git@example.com:',
		'example.com:team/repo.git',
		'file:///srv/repo.git',
		'git://example.com/repo.git'
	])('returns unknown for unsupported or unrecognized origins: %s', (url) => {
		expect(parseRepositoryUrl(url)).toEqual({
			kind: 'unknown',
			host: null,
			port: null,
			username: null,
			path: null
		});
	});
});
