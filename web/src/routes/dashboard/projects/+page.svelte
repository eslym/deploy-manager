<script lang="ts">
	import { dashboardRequest } from '#lib/dashboard-request.js';
	import { formatBytes } from '#lib/format-bytes.js';
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	type Project = {
		id: string;
		name: string;
		url: string;
		keyId?: string;
		lastFetched: string | null;
		gitCacheBytes: number;
		lfsCacheBytes: number;
		warning?: string;
	};
	type RepositoryKey = {
		id: string;
		name: string;
		algorithm: string;
		bits: number;
		fingerprint: string;
		publicKey: string;
		createdAt: string;
	};
	type SshHost = {
		host: string;
		port: number;
		fingerprint: string;
		warning?: string;
		trusted: boolean;
	};
	async function request(
		path: string,
		init?: RequestInit,
		options?: {
			sshCreate?: boolean;
			sshApproval?: boolean;
			sshRefresh?: boolean;
			deletion?: boolean;
		}
	) {
		return dashboardRequest(path, init, async (response) => {
			const status = Number.isInteger(response.status) ? ` (HTTP ${response.status})` : '';
			const responseText = await response.text();
			if (options?.deletion && response.status === 409)
				return new Error(`Remove the project's deployments before deleting it.${status}`);
			if (options?.deletion && response.status === 503)
				return new Error(
					`Project deletion is pending. Remaining cache files are protected while cleanup retries.${status}`
				);
			if (options?.sshApproval && response.status === 409)
				return new Error(
					'The host key changed. The refreshed fingerprint shown here must be independently reviewed before approving again.'
				);
			if (options?.sshCreate && response.status === 409) {
				return new Error(
					'Host key not approved. Verify the fingerprint in SSH host warnings independently before approving it.'
				);
			}
			if (options?.sshRefresh && response.status === 502) {
				return new Error('SSH connection failed. Check the repository key and SSH host warnings.');
			}
			if (response.status === 409)
				return new Error(
					'This change is out of date or the key is assigned. Refresh the page, then choose a key that is not assigned to another project.' +
						status
				);
			const message =
				response.status === 401 || response.status === 403
					? 'Your session may have expired. Sign in again.'
					: response.status === 404
						? 'The requested item was not found. Refresh the page and try again.'
						: response.status === 413
							? 'This repository has too much data or too many branches and tags. Reduce it and try again.'
							: response.status === 429
								? 'The server is handling too many requests. Wait briefly and try again.'
								: response.status === 502
									? 'The repository could not be fetched. Check public HTTPS access and try again.'
									: response.status >= 500
										? 'The server could not complete the request. Try again shortly.'
										: 'The request could not be completed. Check your input and try again.';
			const safeBackendText = responseText.trim().slice(0, 400);
			return new Error(
				`${(options?.sshCreate || options?.sshRefresh) && safeBackendText ? safeBackendText : message}${status}`
			);
		});
	}
	let projects = $state<Project[]>([]);
	let repositoryKeys = $state<RepositoryKey[]>([]);
	let name = $state('');
	let url = $state('');
	let keyId = $state('');
	let error = $state('');
	let busy = $state(false);
	let pendingCreate = $state<{ name: string; url: string; keyId: string } | null>(null);
	let observedHost = $state<SshHost | null>(null);
	let approvalError = $state('');
	function parseSshOrigin(repositoryUrl: string) {
		const sshScheme = /^ssh:\/\//i.test(repositoryUrl);
		try {
			const parsed = new URL(repositoryUrl);
			if (parsed.protocol === 'ssh:' && parsed.hostname) {
				return {
					isSsh: true,
					host: parsed.hostname.toLowerCase(),
					port: Number(parsed.port || 22)
				};
			}
		} catch {
			// SCP-style origins are not valid URLs; parse those below.
		}
		const scp = repositoryUrl.match(/^[^@\s/:]+@([a-zA-Z0-9.-]+):(.+)$/);
		if (scp) return { isSsh: true, host: scp[1].toLowerCase(), port: 22 };
		return { isSsh: sshScheme, host: null, port: null };
	}
	async function findObservedHost(repositoryUrl: string) {
		const address = parseSshOrigin(repositoryUrl);
		if (!address.isSsh || !address.host || address.port === null) return null;
		const hosts: SshHost[] = await request('/api/ssh-hosts');
		return (
			hosts.find(
				(host) =>
					host.warning && host.host.toLowerCase() === address.host && host.port === address.port
			) ?? null
		);
	}
	async function loadProjects() {
		try {
			projects = await request('/api/projects');
		} catch (cause) {
			error = cause instanceof Error ? cause.message : 'Could not load projects.';
		}
	}
	async function loadRepositoryKeys() {
		try {
			repositoryKeys = await request('/api/repository-keys');
		} catch (cause) {
			error = cause instanceof Error ? cause.message : 'Could not load repository keys.';
		}
	}
	async function createProject(event: SubmitEvent) {
		event.preventDefault();
		if (busy) return;
		busy = true;
		error = '';
		const sshOrigin = parseSshOrigin(url);
		const createValues = { name, url, keyId: sshOrigin.isSsh ? keyId : '' };
		try {
			const created: Project = await request(
				'/api/projects',
				{
					method: 'POST',
					body: JSON.stringify(createValues)
				},
				{ sshCreate: sshOrigin.isSsh }
			);
			await goto(`/dashboard/projects/${encodeURIComponent(created.id)}`);
		} catch (cause) {
			if (sshOrigin.isSsh) {
				try {
					const observed = await findObservedHost(url);
					if (observed) {
						pendingCreate = createValues;
						observedHost = observed;
						error = '';
						return;
					}
				} catch {
					// Preserve the original create error if host details cannot be loaded.
				}
			}
			error = cause instanceof Error ? cause.message : 'Could not add repository.';
		} finally {
			busy = false;
		}
	}
	async function approveObservedHost() {
		if (!observedHost || !pendingCreate || busy) return;
		const createSnapshot = pendingCreate;
		busy = true;
		approvalError = '';
		try {
			await request(
				'/api/ssh-hosts/approve',
				{
					method: 'POST',
					body: JSON.stringify({
						host: observedHost.host,
						port: observedHost.port,
						fingerprint: observedHost.fingerprint,
						confirm: true
					})
				},
				{ sshApproval: true }
			);
			const approvedCreate = createSnapshot;
			const created: Project = await request(
				'/api/projects',
				{
					method: 'POST',
					body: JSON.stringify(approvedCreate)
				},
				{ sshCreate: true }
			);
			pendingCreate = null;
			observedHost = null;
			await goto(`/dashboard/projects/${encodeURIComponent(created.id)}`);
		} catch (cause) {
			approvalError = cause instanceof Error ? cause.message : 'Could not approve this host key.';
			try {
				const refreshed = await findObservedHost(createSnapshot.url);
				if (refreshed) observedHost = refreshed;
			} catch {
				// Keep the fingerprint currently under review.
			}
		} finally {
			busy = false;
		}
	}
	function cancelHostApproval() {
		pendingCreate = null;
		observedHost = null;
		approvalError = '';
	}
	onMount(() => {
		void loadProjects();
		void loadRepositoryKeys();
	});
</script>

<svelte:head><title>Projects · Deploy Manager</title></svelte:head>
<main class="mx-auto w-[min(100%-2rem,64rem)] py-10">
	<h1 class="m-0 text-3xl font-semibold tracking-[-0.03em] text-[#243528]">Projects</h1>
	<p class="mt-2 text-[#58645b]">
		You’re signed in. Connect HTTPS or SSH repositories to browse branches and tags.
	</p>
	{#if error && url.startsWith('ssh://')}
		<p class="mt-3 text-sm text-[#58645b]">
			Check <a class="font-semibold text-[#214c30] underline" href="/dashboard/keys"
				>repository keys</a
			>
			and
			<a class="font-semibold text-[#214c30] underline" href="/dashboard/ssh-hosts"
				>SSH host warnings</a
			> before retrying.
		</p>
	{/if}
	{#if error}<p
			role="alert"
			class="mt-4 rounded-lg border border-red-200 bg-red-50 p-3 text-red-800"
		>
			{error}
		</p>{/if}
	<section class="mt-8 rounded-xl border border-[#e1e6df] bg-white p-5">
		<h2 class="m-0 text-xl font-semibold text-[#243528]">Add a repository</h2>
		<form class="mt-4 grid gap-4 sm:grid-cols-2" onsubmit={createProject}>
			<label class="grid gap-1 text-sm font-medium text-[#354238]"
				>Project name<input
					class="min-h-11 rounded-lg border border-[#aeb9ae] px-3"
					bind:value={name}
					required
					maxlength="100"
				/></label
			>
			<label class="grid gap-1 text-sm font-medium text-[#354238]"
				>Repository URL<input
					class="min-h-11 rounded-lg border border-[#aeb9ae] px-3"
					bind:value={url}
					placeholder="https://example.com/owner/site.git or ssh://user@example.com:22/owner/site.git"
					required
				/></label
			>
			{#if parseSshOrigin(url).isSsh}
				<p class="text-sm text-[#58645b] sm:col-span-2">
					Need a key? <a class="font-semibold text-[#214c30] underline" href="/dashboard/keys"
						>Manage repository keys</a
					>. If the host is unrecognized,
					<a class="font-semibold text-[#214c30] underline" href="/dashboard/ssh-hosts"
						>review SSH host warnings</a
					>.
				</p>
				<label class="grid gap-1 text-sm font-medium text-[#354238]"
					>SSH key
					<select
						class="min-h-11 rounded-lg border border-[#aeb9ae] px-3"
						bind:value={keyId}
						required
					>
						<option value="">Choose a key</option>
						{#each repositoryKeys as key (key.id)}<option value={key.id}
								>{key.name} · {key.fingerprint}</option
							>{/each}
					</select>
				</label>
			{/if}
			<button
				class="min-h-11 w-fit rounded-lg bg-[#214c30] px-5 font-semibold text-white disabled:opacity-60"
				disabled={busy}>Add repository</button
			>
		</form>
	</section>
	{#if observedHost && pendingCreate}
		<dialog
			open
			class="mt-4 w-full max-w-none rounded-xl border border-amber-400 bg-amber-50 p-5 text-left"
			aria-labelledby="host-approval-title"
		>
			<h2 id="host-approval-title" class="m-0 text-lg font-semibold text-[#243528]">
				Verify this SSH host
			</h2>
			<p class="mt-2 text-sm text-[#354238]">
				Only approve this key after checking its fingerprint through an independent, trusted source.
			</p>
			<dl class="mt-3 grid gap-2 text-sm">
				<div>
					<dt class="font-semibold">Host</dt>
					<dd>{observedHost.host}</dd>
				</div>
				<div>
					<dt class="font-semibold">Port</dt>
					<dd>{observedHost.port}</dd>
				</div>
				<div>
					<dt class="font-semibold">Fingerprint</dt>
					<dd class="font-mono break-all">{observedHost.fingerprint}</dd>
				</div>
			</dl>
			{#if observedHost.warning}<p class="mt-2 text-sm">{observedHost.warning}</p>{/if}
			{#if approvalError}<p role="alert" class="mt-3 text-sm text-red-800">{approvalError}</p>{/if}
			<div class="mt-4 flex gap-3">
				<button
					class="min-h-10 rounded-lg bg-[#214c30] px-4 font-semibold text-white disabled:opacity-60"
					onclick={approveObservedHost}
					disabled={busy}>Approve and add repository</button
				>
				<button
					class="min-h-10 rounded-lg border border-[#aeb9ae] px-4"
					onclick={cancelHostApproval}
					disabled={busy}>Cancel</button
				>
			</div>
		</dialog>
	{/if}
	<section class="mt-8">
		<h2 class="text-xl font-semibold text-[#243528]">Your projects</h2>
		{#if projects.length === 0}<p class="text-[#58645b]">No projects yet.</p>{:else}
			<ul class="grid gap-3 p-0">
				{#each projects as project (project.id)}
					<li class="min-w-0 list-none">
						<a
							href={`/dashboard/projects/${encodeURIComponent(project.id)}`}
							aria-label={project.name}
							class="block min-w-0 rounded-lg border border-[#d7ded6] bg-white p-4 hover:bg-[#f4f7f3]"
						>
							<span class="block font-semibold text-[#243528]">{project.name}</span>
							<span class="block text-sm break-all text-[#58645b]">{project.url}</span>
							<span class="mt-1 block text-xs text-[#58645b]"
								>Git {formatBytes(project.gitCacheBytes ?? 0)} · LFS {formatBytes(
									project.lfsCacheBytes ?? 0
								)}</span
							>
						</a>
					</li>
				{/each}
			</ul>
		{/if}
	</section>
</main>
