<script lang="ts">
	import { onMount } from 'svelte';
	import { tick } from 'svelte';
	import Deployments from '#lib/components/Deployments.svelte';
	import WebhookSetup from '#lib/components/WebhookSetup.svelte';
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
	let projects = $state<Project[]>([]);
	let repositoryKeys = $state<RepositoryKey[]>([]);
	let keyName = $state('');
	let privateKeyInput = $state('');
	let keyError = $state('');
	let keyBusy = $state(false);
	let refs = $state<Record<string, Ref[]>>({});
	let name = $state('');
	let url = $state('');
	let keyId = $state('');
	let sshHosts = $state<
		{ host: string; port: number; fingerprint: string; warning?: string; trusted: boolean }[]
	>([]);
	let hostError = $state('');
	let keyAssignments = $state<Record<string, string>>({});
	let search = $state('');
	let error = $state('');
	let busy = $state(false);
	const lfsConfirmation = 'I independently verified these LFS destinations';
	let lfsEndpoint = $state('');
	let lfsOrigins = $state('');
	let lfsAllowInternal = $state(false);
	let lfsInternalConfirmation = $state(false);
	let lfsSuggestedEndpoint = $state('');
	let lfsLoading = $state(false);
	let lfsBusy = $state(false);
	let lfsError = $state('');
	let lfsNotice = $state('');
	let selected = $state('');
	let projectDeleteConfirmation = $state('');
	let cacheRecreateConfirmation = $state('');
	let cacheRecreateTrigger: HTMLButtonElement | undefined = $state();
	let cacheRecreateDialog: HTMLDivElement | undefined = $state();
	let cacheRecreateConfirmButton: HTMLButtonElement | undefined = $state();
	let cacheRecreateBusy = $state(false);
	let cacheRecreateError = $state('');
	let cacheRecreateNotice = $state('');
	let visibleRefs = $derived(
		(refs[selected] ?? []).filter((ref) =>
			`${ref.kind} ${ref.name}`.toLowerCase().includes(search.toLowerCase())
		)
	);

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
	async function loadProjects() {
		try {
			projects = await request('/api/projects');
			for (const project of projects) keyAssignments[project.id] = project.keyId ?? '';
			for (const project of projects) await loadRefs(project.id);
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
	async function assignKey(project: Project) {
		error = '';
		try {
			await request(`/api/projects/${project.id}/key`, {
				method: 'POST',
				body: JSON.stringify({ keyId: keyAssignments[project.id] })
			});
		} catch (cause) {
			error = cause instanceof Error ? cause.message : 'Could not assign the key.';
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
	async function loadRefs(id: string) {
		refs[id] = await request(`/api/projects/${id}/refs`);
	}
	async function loadLfsSettings(id: string) {
		lfsLoading = true;
		lfsError = '';
		lfsEndpoint = '';
		lfsOrigins = '';
		lfsAllowInternal = false;
		lfsInternalConfirmation = false;
		lfsSuggestedEndpoint = '';
		try {
			const result: {
				settings: { endpoint: string; objectOrigins: string[]; allowInternal: boolean };
				suggestedEndpoint: string;
			} = await request(`/api/projects/${id}/lfs`);
			lfsEndpoint = result.settings.endpoint;
			lfsOrigins = result.settings.objectOrigins.join('\n');
			lfsAllowInternal = result.settings.allowInternal;
			lfsSuggestedEndpoint = result.suggestedEndpoint;
		} catch (cause) {
			lfsError = cause instanceof Error ? cause.message : 'Could not load LFS settings.';
		} finally {
			lfsLoading = false;
		}
	}
	async function saveLfsSettings(event: SubmitEvent) {
		event.preventDefault();
		if (lfsBusy || !selected) return;
		if (
			!window.confirm(
				'Save these LFS destinations? Only continue if you independently verified them.'
			)
		)
			return;
		lfsBusy = true;
		lfsError = '';
		lfsNotice = '';
		try {
			await request(`/api/projects/${selected}/lfs`, {
				method: 'POST',
				body: JSON.stringify({
					endpoint: lfsEndpoint,
					objectOrigins: lfsOrigins
						.split(/\r?\n/)
						.map((origin) => origin.trim())
						.filter(Boolean),
					allowInternal: lfsAllowInternal,
					confirmation: lfsConfirmation,
					...(lfsAllowInternal ? { internalConfirmation: lfsConfirmation } : {})
				})
			});
			await loadLfsSettings(selected);
			lfsNotice = 'LFS settings saved.';
		} catch (cause) {
			lfsError = cause instanceof Error ? cause.message : 'Could not save LFS settings.';
		} finally {
			lfsBusy = false;
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
			projects = [...projects, created];
			keyAssignments[created.id] = created.keyId ?? '';
			refs[created.id] = await request(`/api/projects/${created.id}/refs`);
			selected = created.id;
			name = '';
			url = '';
			keyId = '';
		} catch (cause) {
			error = cause instanceof Error ? cause.message : 'Could not add repository.';
			if (url.startsWith('ssh://')) await loadSshHosts();
		} finally {
			busy = false;
		}
	}
	async function refresh(project: Project) {
		if (busy) return;
		busy = true;
		try {
			await request(
				`/api/projects/${project.id}/refresh`,
				{ method: 'POST', body: '{}' },
				{ sshRefresh: project.url.startsWith('ssh://') }
			);
			const refreshedRefs: Ref[] = await request(`/api/projects/${project.id}/refs`);
			const refreshedProjects: Project[] = await request('/api/projects');
			refs[project.id] = refreshedRefs;
			projects = refreshedProjects;
			error = '';
		} catch (cause) {
			const message =
				cause instanceof Error ? cause.message : 'The request could not be completed. Try again.';
			projects = projects.map((item) =>
				item.id === project.id
					? {
							...item,
							warning: `Could not update branches and tags. ${message}`
						}
					: item
			);
			if (project.url.startsWith('ssh://')) await loadSshHosts();
		} finally {
			busy = false;
		}
	}
	async function deleteProject(project: Project) {
		if (busy || projectDeleteConfirmation !== project.id) return;
		busy = true;
		error = '';
		try {
			await request(
				`/api/projects/${project.id}/delete`,
				{
					method: 'POST',
					body: JSON.stringify({ confirm: true })
				},
				{ deletion: true }
			);
			projects = projects.filter((item) => item.id !== project.id);
			delete refs[project.id];
			delete keyAssignments[project.id];
			if (selected === project.id) selected = projects[0]?.id ?? '';
			projectDeleteConfirmation = '';
		} catch (cause) {
			error = cause instanceof Error ? cause.message : 'Could not delete the project.';
			if (error.startsWith("Remove the project's deployments")) projectDeleteConfirmation = '';
			await loadProjects();
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
	async function recreateCache(project: Project) {
		if (cacheRecreateBusy || cacheRecreateConfirmation !== project.id) return;
		cacheRecreateBusy = true;
		cacheRecreateError = '';
		cacheRecreateNotice = '';
		let storageCleared = false;
		try {
			await request(`/api/projects/${project.id}/cache/recreate`, {
				method: 'POST',
				body: JSON.stringify({ confirm: true })
			});
			storageCleared = true;
			const refreshedProjects: Project[] = await request('/api/projects');
			projects = refreshedProjects;
			closeCacheRecreateConfirmation();
			if (selected === project.id)
				cacheRecreateNotice = 'Repository storage cleared. Sizes updated.';
		} catch (cause) {
			cacheRecreateError = storageCleared
				? 'Storage was cleared, but the sizes could not be refreshed. Reload the page to check them.'
				: 'Could not clear repository storage. Try again.';
		} finally {
			cacheRecreateBusy = false;
		}
	}
	async function openCacheRecreateConfirmation(project: Project) {
		cacheRecreateTrigger =
			document.activeElement instanceof HTMLButtonElement ? document.activeElement : undefined;
		cacheRecreateConfirmation = project.id;
		cacheRecreateError = '';
		cacheRecreateNotice = '';
		await tick();
		cacheRecreateConfirmButton?.focus();
	}
	function closeCacheRecreateConfirmation() {
		cacheRecreateConfirmation = '';
		void tick().then(() => cacheRecreateTrigger?.focus());
	}
	function trapCacheRecreateFocus(event: KeyboardEvent) {
		if (event.key === 'Escape' && !cacheRecreateBusy) {
			event.preventDefault();
			closeCacheRecreateConfirmation();
			return;
		}
		if (event.key !== 'Tab' || !cacheRecreateDialog) return;
		const focusable = Array.from(
			cacheRecreateDialog.querySelectorAll<HTMLElement>('button:not(:disabled)')
		);
		const first = focusable[0];
		const last = focusable.at(-1);
		if (event.shiftKey && document.activeElement === first) {
			event.preventDefault();
			last?.focus();
		} else if (!event.shiftKey && document.activeElement === last) {
			event.preventDefault();
			first?.focus();
		}
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
	<section class="mt-8 grid gap-6 md:grid-cols-[16rem_1fr]">
		<div>
			<h2 class="text-xl font-semibold text-[#243528]">Your projects</h2>
			{#if projects.length === 0}<p class="text-[#58645b]">No projects yet.</p>{:else}<ul
					class="grid gap-2 p-0"
				>
					{#each projects as project (project.id)}<li class="list-none">
							<button
								class="w-full rounded-lg border border-[#d7ded6] p-3 text-left hover:bg-[#f4f7f3]"
								aria-pressed={selected === project.id}
								onclick={() => {
									selected = project.id;
									closeCacheRecreateConfirmation();
									cacheRecreateError = '';
									cacheRecreateNotice = '';
									search = '';
									void loadLfsSettings(project.id);
								}}
								><span class="block font-semibold text-[#243528]">{project.name}</span><span
									class="block truncate text-sm text-[#58645b]">{project.url}</span
								><span class="mt-1 block text-xs text-[#58645b]"
									>Git {formatBytes(project.gitCacheBytes ?? 0)} · LFS {formatBytes(
										project.lfsCacheBytes ?? 0
									)}</span
								></button
							>
						</li>{/each}
				</ul>{/if}
		</div>
		{#if selected}
			{@const project = projects.find((item) => item.id === selected)}
			<div>
				<div class="flex flex-wrap items-center justify-between gap-3">
					<h2 class="m-0 text-xl font-semibold text-[#243528]">Branches and tags</h2>
					<button
						class="min-h-10 rounded-lg border border-[#aeb9ae] px-4 font-semibold text-[#214c30] disabled:opacity-60"
						onclick={() => project && refresh(project)}
						disabled={busy}>Refresh</button
					>
				</div>
				{#if project?.lastFetched}<p class="mt-2 text-sm text-[#58645b]">
						Last updated {new Date(project.lastFetched).toLocaleString()}
					</p>{/if}
				{#if project?.warning}<p
						role="alert"
						class="mt-3 rounded-lg border border-amber-300 bg-amber-50 p-3 text-amber-900"
					>
						{project.warning} Your current branches and tags are still shown.
					</p>{/if}
				<section
					class="mt-4 rounded-lg border border-[#e1e6df] bg-[#f8faf7] p-4"
					aria-labelledby="storage-heading"
				>
					<h3 id="storage-heading" class="m-0 font-semibold text-[#243528]">Repository storage</h3>
					<p class="mt-2 text-sm text-[#58645b]">
						Git cache: {formatBytes(project?.gitCacheBytes ?? 0)} · LFS cache: {formatBytes(
							project?.lfsCacheBytes ?? 0
						)}
					</p>
					{#if cacheRecreateError}<p role="alert" class="mt-2 text-sm text-red-800">
							{cacheRecreateError}
						</p>{/if}
					{#if cacheRecreateNotice}<p role="status" class="mt-2 text-sm text-[#214c30]">
							{cacheRecreateNotice}
						</p>{/if}
					{#if cacheRecreateConfirmation === project?.id}
						<div
							bind:this={cacheRecreateDialog}
							class="mt-3 rounded-lg border border-amber-300 bg-amber-50 p-3"
							role="dialog"
							aria-modal="true"
							aria-labelledby="cache-recreate-title"
							aria-describedby="cache-recreate-warning"
							tabindex="-1"
							onkeydown={trapCacheRecreateFocus}
						>
							<h4 id="cache-recreate-title" class="m-0 font-semibold text-amber-950">
								Confirm clearing repository storage
							</h4>
							<p id="cache-recreate-warning" role="alert" class="mt-2 text-sm text-amber-950">
								This can free disk space. It permanently removes commits no longer reachable from a
								branch or tag, plus Git LFS files the server can no longer provide. You may lose the
								ability to redeploy or roll back releases that need them.
							</p>
							<div class="mt-3 flex flex-wrap gap-2">
								<button
									bind:this={cacheRecreateConfirmButton}
									class="min-h-9 rounded-lg bg-amber-900 px-3 text-sm font-semibold text-white disabled:opacity-60"
									onclick={() => project && recreateCache(project)}
									disabled={cacheRecreateBusy || !project}>Clear cache</button
								>
								<button
									class="min-h-9 rounded-lg border border-[#aeb9ae] px-3 text-sm font-semibold disabled:opacity-60"
									onclick={closeCacheRecreateConfirmation}
									disabled={cacheRecreateBusy}>Cancel</button
								>
							</div>
						</div>
					{:else}
						<button
							class="mt-2 min-h-9 rounded-lg border border-amber-500 px-3 text-sm font-semibold text-amber-950 disabled:opacity-60"
							onclick={() => project && openCacheRecreateConfirmation(project)}
							disabled={cacheRecreateBusy || !project}>Clear cache…</button
						>
					{/if}
				</section>
				<div class="mt-4 border-t border-[#e1e6df] pt-4">
					{#if projectDeleteConfirmation === project?.id}
						<div
							class="rounded-lg border border-red-200 bg-red-50 p-3"
							aria-label="Confirm project deletion"
						>
							<p class="m-0 text-sm text-red-900">
								Delete “{project?.name}”? Projects with deployments cannot be deleted.
							</p>
							<div class="mt-3 flex gap-2">
								<button
									class="min-h-9 rounded-lg bg-red-800 px-3 text-sm font-semibold text-white disabled:opacity-60"
									onclick={() => project && deleteProject(project)}
									disabled={busy}>Confirm delete</button
								>
								<button
									class="min-h-9 rounded-lg border border-[#aeb9ae] px-3 text-sm font-semibold disabled:opacity-60"
									onclick={() => (projectDeleteConfirmation = '')}
									disabled={busy}>Cancel</button
								>
							</div>
						</div>
					{:else}
						<button
							class="min-h-10 rounded-lg border border-red-300 px-4 font-semibold text-red-800 disabled:opacity-60"
							onclick={() => (projectDeleteConfirmation = project?.id ?? '')}
							disabled={busy || !project}>Delete project</button
						>
					{/if}
				</div>
				{#if project?.url.startsWith('ssh://')}
					<label class="mt-4 grid gap-1 text-sm font-medium text-[#354238]"
						>SSH key
						<select
							class="min-h-11 rounded-lg border border-[#aeb9ae] px-3"
							bind:value={keyAssignments[project.id]}
						>
							<option value="">Choose a key</option>
							{#each repositoryKeys as key (key.id)}<option value={key.id}>{key.name}</option
								>{/each}
						</select>
					</label>
					<button
						class="mt-2 rounded-lg border border-[#aeb9ae] px-4 py-2 text-sm"
						onclick={() => assignKey(project)}
						disabled={!keyAssignments[project.id]}>Change SSH key</button
					>
				{/if}
				<label class="mt-4 grid gap-1 text-sm font-medium text-[#354238]"
					>Search branches and tags<input
						class="min-h-11 rounded-lg border border-[#aeb9ae] px-3"
						bind:value={search}
					/></label
				>
				<ul class="mt-3 grid gap-2 p-0">
					{#each visibleRefs as ref (`${ref.kind}/${ref.name}`)}<li
							class="list-none rounded-lg border border-[#e1e6df] p-3"
						>
							<span
								class="mr-2 inline-block rounded bg-[#edf3ed] px-2 py-1 text-xs font-semibold text-[#214c30] uppercase"
								>{ref.kind}</span
							><span class="font-medium">{ref.name}</span><code
								class="mt-1 block text-xs break-all text-[#58645b]">{ref.commit}</code
							>
						</li>{:else}<li class="list-none py-4 text-[#58645b]">No matching refs.</li>{/each}
				</ul>
			</div>
			<div class="rounded-xl border border-[#e1e6df] bg-white p-5 md:col-span-2">
				<h2 class="m-0 text-xl font-semibold text-[#243528]">Git LFS destinations</h2>
				<p class="mt-2 text-sm text-[#58645b]">
					Choose the HTTPS server and exact HTTPS origins this project may use for LFS downloads.
					Verify each destination independently before saving.
				</p>
				{#if lfsLoading}<p class="mt-4 text-sm text-[#58645b]">Loading settings…</p>{:else}
					{#if lfsError}<p
							role="alert"
							class="mt-4 rounded-lg border border-red-200 bg-red-50 p-3 text-red-800"
						>
							{lfsError}
						</p>{/if}
					{#if lfsNotice}<p role="status" class="mt-4 text-sm text-[#214c30]">{lfsNotice}</p>{/if}
					<form class="mt-4 grid gap-4" onsubmit={saveLfsSettings}>
						<label class="grid gap-1 text-sm font-medium text-[#354238]" for="lfs-endpoint"
							>LFS server URL
							<input
								id="lfs-endpoint"
								class="min-h-11 rounded-lg border border-[#aeb9ae] px-3"
								bind:value={lfsEndpoint}
								placeholder="https://lfs.example.com/owner/repository.git/info/lfs"
							/>
						</label>
						{#if lfsSuggestedEndpoint}
							<div class="text-sm text-[#58645b]">
								Suggestion (not approved): <code class="break-all">{lfsSuggestedEndpoint}</code>
								<button
									type="button"
									class="ml-2 rounded-lg border border-[#aeb9ae] px-3 py-1 text-[#214c30]"
									onclick={() => (lfsEndpoint = lfsSuggestedEndpoint)}>Use suggestion</button
								>
							</div>
						{/if}
						<label class="grid gap-1 text-sm font-medium text-[#354238]" for="lfs-origins"
							>Object download origins
							<textarea
								id="lfs-origins"
								class="min-h-28 rounded-lg border border-[#aeb9ae] p-3 font-mono text-sm"
								bind:value={lfsOrigins}
								placeholder="https://objects.example.com"
								aria-describedby="lfs-origins-note"></textarea>
						</label>
						<p id="lfs-origins-note" class="-mt-3 text-sm text-[#58645b]">
							Enter one exact HTTPS origin per line, such as https://objects.example.com:8443. Do
							not include paths or credentials.
						</p>
						<label class="flex items-start gap-2 text-sm text-[#354238]">
							<input type="checkbox" class="mt-1" bind:checked={lfsAllowInternal} />
							<span>Allow downloads from internal network addresses</span>
						</label>
						{#if lfsAllowInternal}
							<label
								class="flex items-start gap-2 rounded-lg border border-amber-300 bg-amber-50 p-3 text-sm text-amber-950"
							>
								<input type="checkbox" class="mt-1" bind:checked={lfsInternalConfirmation} />
								<span
									>I independently verified the internal destinations and intend to allow them.</span
								>
							</label>
						{/if}
						<button
							class="min-h-11 w-fit rounded-lg bg-[#214c30] px-5 font-semibold text-white disabled:opacity-60"
							disabled={lfsBusy || (lfsAllowInternal && !lfsInternalConfirmation)}
							>Save LFS settings</button
						>
					</form>
				{/if}
			</div>
			<div class="md:col-span-2">
				<Deployments projectId={selected} />
			</div>
			<div class="md:col-span-2">
				{#key selected}
					<WebhookSetup projectId={selected} provider="github" />
					<WebhookSetup projectId={selected} provider="gitea" />
				{/key}
			</div>
		{/if}
	</section>
</main>
