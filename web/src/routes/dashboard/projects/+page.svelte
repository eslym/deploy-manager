<script lang="ts">
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
	type Ref = { kind: string; name: string; commit: string };
	type RepositoryKey = {
		id: string;
		name: string;
		algorithm: string;
		bits: number;
		fingerprint: string;
		publicKey: string;
		createdAt: string;
	};
	async function request(
		path: string,
		init?: RequestInit,
		options?: { sshCreate?: boolean; sshRefresh?: boolean; deletion?: boolean }
	) {
		const response = await fetch(path, {
			...init,
			credentials: 'same-origin',
			headers: { 'Content-Type': 'application/json', ...init?.headers }
		});
		if (!response.ok) {
			const status = Number.isInteger(response.status) ? ` (HTTP ${response.status})` : '';
			const responseText = await response.text();
			if (options?.deletion && response.status === 409)
				throw new Error(`Remove the project's deployments before deleting it.${status}`);
			if (options?.deletion && response.status === 503)
				throw new Error(
					`Project deletion is pending. Remaining cache files are protected while cleanup retries.${status}`
				);
			if (options?.sshCreate && response.status === 409) {
				throw new Error(
					'Host key not approved. Verify the fingerprint in SSH host warnings independently before approving it.'
				);
			}
			if (options?.sshRefresh && response.status === 502) {
				throw new Error('SSH connection failed. Check the repository key and SSH host warnings.');
			}
			if (response.status === 409)
				throw new Error(
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
			throw new Error(
				`${(options?.sshCreate || options?.sshRefresh) && safeBackendText ? safeBackendText : message}${status}`
			);
		}
		return response.status === 204 ? null : response.json();
	}
	let projects = $state<Project[]>([]);
	let repositoryKeys = $state<RepositoryKey[]>([]);
	let keyName = $state('');
	let privateKeyInput = $state('');
	let keyError = $state('');
	let keyBusy = $state(false);
	let name = $state('');
	let url = $state('');
	let keyId = $state('');
	let sshHosts = $state<
		{ host: string; port: number; fingerprint: string; warning?: string; trusted: boolean }[]
	>([]);
	let hostError = $state('');
	let error = $state('');
	let busy = $state(false);
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
			keyError = cause instanceof Error ? cause.message : 'Could not load repository keys.';
		}
	}
	async function loadSshHosts() {
		try {
			sshHosts = await request('/api/ssh-hosts');
		} catch (cause) {
			hostError = cause instanceof Error ? cause.message : 'Could not load SSH hosts.';
		}
	}
	async function approveHost(host: (typeof sshHosts)[number]) {
		if (
			!window.confirm(
				`Only continue after independently verifying this SSH host key.\n\n${host.host}:${host.port}\n${host.fingerprint}\n\nApprove this host key?`
			)
		)
			return;
		hostError = '';
		try {
			await request('/api/ssh-hosts/approve', {
				method: 'POST',
				body: JSON.stringify({
					host: host.host,
					port: host.port,
					fingerprint: host.fingerprint,
					confirm: true
				})
			});
			await loadSshHosts();
		} catch (cause) {
			hostError = cause instanceof Error ? cause.message : 'Could not approve the host key.';
		}
	}
	async function deleteKey(key: RepositoryKey) {
		keyError = '';
		try {
			await request(`/api/repository-keys/${key.id}/delete`, { method: 'POST', body: '{}' });
			await loadRepositoryKeys();
		} catch (cause) {
			keyError =
				cause instanceof Error && cause.message.includes('409')
					? 'This key is assigned to a project. Assign another key before removing it.'
					: cause instanceof Error
						? cause.message
						: 'Could not remove the key.';
		}
	}
	async function saveRepositoryKey(generate: boolean) {
		if (keyBusy) return;
		keyBusy = true;
		keyError = '';
		const name = keyName;
		const privateKey = generate ? '' : privateKeyInput;
		// Remove the secret from reactive state before starting the network request.
		privateKeyInput = '';
		try {
			await request('/api/repository-keys', {
				method: 'POST',
				body: JSON.stringify(generate ? { name, generate: true } : { name, privateKey })
			});
			keyName = '';
			await loadRepositoryKeys();
		} catch (cause) {
			keyError = cause instanceof Error ? cause.message : 'Could not save the repository key.';
		} finally {
			// The textarea is cleared before submission; this request-local string remains until the request settles and cannot be securely zeroed.
			keyBusy = false;
		}
	}
	async function createProject(event: SubmitEvent) {
		event.preventDefault();
		if (busy) return;
		busy = true;
		error = '';
		try {
			const created: Project = await request(
				'/api/projects',
				{
					method: 'POST',
					body: JSON.stringify({ name, url, keyId: url.startsWith('ssh://') ? keyId : '' })
				},
				{ sshCreate: url.startsWith('ssh://') }
			);
			await goto(`/dashboard/projects/${encodeURIComponent(created.id)}`);
		} catch (cause) {
			error = cause instanceof Error ? cause.message : 'Could not add repository.';
			if (url.startsWith('ssh://')) await loadSshHosts();
		} finally {
			busy = false;
		}
	}
	function formatBytes(bytes: number) {
		if (!Number.isFinite(bytes) || bytes <= 0) return '0 bytes';
		const units = ['bytes', 'KB', 'MB', 'GB', 'TB'];
		const unit = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
		const size = bytes / 1024 ** unit;
		return `${size.toLocaleString(undefined, { maximumFractionDigits: unit === 0 ? 0 : 1 })} ${units[unit]}`;
	}
	onMount(() => {
		void loadProjects();
		void loadRepositoryKeys();
		void loadSshHosts();
	});
</script>

<svelte:head><title>Projects · Deploy Manager</title></svelte:head>
<main class="mx-auto w-[min(100%-2rem,64rem)] py-10">
	<h1 class="m-0 text-3xl font-semibold tracking-[-0.03em] text-[#243528]">Projects</h1>
	<p class="mt-2 text-[#58645b]">
		You’re signed in. Connect HTTPS or SSH repositories to browse branches and tags.
	</p>
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
			{#if url.startsWith('ssh://')}
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
	<section class="mt-8 rounded-xl border border-[#e1e6df] bg-white p-5">
		<h2 class="m-0 text-xl font-semibold text-[#243528]">Repository keys</h2>
		<p class="mt-2 text-sm text-[#58645b]">
			Keys let Deploy Manager connect to private repositories. Private keys are encrypted and never
			shown here.
		</p>
		{#if keyError}<p
				role="alert"
				class="mt-4 rounded-lg border border-red-200 bg-red-50 p-3 text-red-800"
			>
				{keyError}
			</p>{/if}
		<form
			class="mt-4 grid gap-4"
			onsubmit={(event) => {
				event.preventDefault();
				void saveRepositoryKey(true);
			}}
		>
			<label class="grid gap-1 text-sm font-medium text-[#354238]"
				>Key name<input
					class="min-h-11 rounded-lg border border-[#aeb9ae] px-3"
					bind:value={keyName}
					required
					maxlength="100"
				/></label
			>
			<button
				class="min-h-11 w-fit rounded-lg bg-[#214c30] px-5 font-semibold text-white disabled:opacity-60"
				disabled={keyBusy}>Create Ed25519 key</button
			>
		</form>
		<form
			class="mt-6 grid gap-4 border-t border-[#e1e6df] pt-5"
			onsubmit={(event) => {
				event.preventDefault();
				void saveRepositoryKey(false);
			}}
		>
			<label class="grid gap-1 text-sm font-medium text-[#354238]"
				>Name for imported key<input
					class="min-h-11 rounded-lg border border-[#aeb9ae] px-3"
					bind:value={keyName}
					required
					maxlength="100"
				/></label
			>
			<label class="grid gap-1 text-sm font-medium text-[#354238]"
				>Private key<textarea
					class="min-h-32 rounded-lg border border-[#aeb9ae] p-3 font-mono text-sm"
					bind:value={privateKeyInput}
					required
					autocomplete="off"
					spellcheck="false"
					aria-describedby="private-key-note"></textarea></label
			>
			<p id="private-key-note" class="-mt-3 text-sm text-[#58645b]">
				Paste an unencrypted OpenSSH or PEM key: Ed25519, RSA (3072 bits or stronger), or ECDSA
				(P-256, P-384, P-521). The text is cleared as soon as you submit.
			</p>
			<button
				class="min-h-11 w-fit rounded-lg border border-[#aeb9ae] px-5 font-semibold text-[#214c30] disabled:opacity-60"
				disabled={keyBusy}>Import private key</button
			>
		</form>
		<h3 class="mt-8 text-lg font-semibold text-[#243528]">Saved keys</h3>
		{#if repositoryKeys.length === 0}<p class="text-[#58645b]">No repository keys yet.</p>{:else}<ul
				class="grid gap-3 p-0"
			>
				{#each repositoryKeys as key (key.id)}<li
						class="list-none rounded-lg border border-[#e1e6df] p-4"
					>
						<p class="m-0 font-semibold text-[#243528]">{key.name}</p>
						<p class="mt-1 text-sm text-[#58645b]">
							{key.algorithm} · {key.bits} bits · {key.fingerprint}
						</p>
						<code class="mt-2 block text-xs break-all text-[#58645b]">{key.publicKey}</code>
						<p class="mt-2 text-xs text-[#58645b]">
							Added {new Date(key.createdAt).toLocaleString()}
						</p>
						<button
							class="mt-3 rounded-lg border border-[#aeb9ae] px-3 py-2 text-sm"
							onclick={() => deleteKey(key)}>Remove key</button
						>
					</li>{/each}
			</ul>{/if}
	</section>
	<section class="mt-8 rounded-xl border border-[#e1e6df] bg-white p-5">
		<h2 class="m-0 text-xl font-semibold text-[#243528]">SSH host keys</h2>
		{#if hostError}<p role="alert" class="mt-3 text-red-800">{hostError}</p>{/if}
		{#if sshHosts.filter((host) => host.warning).length === 0}
			<p class="mt-2 text-sm text-[#58645b]">No SSH host warnings.</p>
		{:else}
			{#each sshHosts.filter((host) => host.warning) as host (`${host.host}:${host.port}`)}
				<div class="mt-4 rounded-lg border border-amber-300 bg-amber-50 p-4">
					<p class="font-semibold">{host.host}:{host.port}</p>
					<p class="mt-1 text-sm">{host.warning}</p>
					<p class="mt-1 font-mono text-xs break-all">{host.fingerprint}</p>
					<p class="mt-2 text-sm">Verify this fingerprint independently before approving it.</p>
					<button
						class="mt-3 rounded-lg border border-[#aeb9ae] px-3 py-2 text-sm"
						onclick={() => approveHost(host)}>Approve verified host key</button
					>
				</div>
			{/each}
		{/if}
	</section>
</main>
