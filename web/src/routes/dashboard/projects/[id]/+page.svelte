<script lang="ts">
	import { tick } from 'svelte';
	import { afterNavigate, goto } from '$app/navigation';
	import { page } from '$app/state';
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
	let refs = $state<Record<string, Ref[]>>({});
	let sshHosts = $state<
		{ host: string; port: number; fingerprint: string; warning?: string; trusted: boolean }[]
	>([]);
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
	let lfsSelectionVersion = 0;
	let lfsLoadVersion = 0;
	let projectDeleteConfirmation = $state('');
	let cacheRecreateConfirmation = $state('');
	let cacheRecreateTrigger: HTMLButtonElement | undefined = $state();
	let cacheRecreateTriggerProject = '';
	let cacheRecreateBusy = $state(false);
	let cacheRecreateError = $state('');
	let cacheRecreateNotice = $state('');
	let selected = $derived(page.params.id ?? '');
	let loading = $state(true);
	let missing = $state(false);
	let visibleRefs = $derived(
		(refs[selected] ?? []).filter((ref) =>
			`${ref.kind} ${ref.name}`.toLowerCase().includes(search.toLowerCase())
		)
	);

	async function loadProjects() {
		try {
			projects = await request('/api/projects');
			for (const project of projects) keyAssignments[project.id] = project.keyId ?? '';
			missing = !projects.some((project) => project.id === selected);
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
	async function loadSshHosts() {
		try {
			sshHosts = await request('/api/ssh-hosts');
		} catch {
			/* Keep the original project error visible. */
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
	async function loadRefs(id: string) {
		refs[id] = await request(`/api/projects/${id}/refs`);
	}
	async function loadLfsSettings(id: string) {
		const selectionVersion = lfsSelectionVersion;
		const loadVersion = ++lfsLoadVersion;
		const isCurrent = () =>
			selectionVersion === lfsSelectionVersion && selected === id && loadVersion === lfsLoadVersion;
		if (selectionVersion !== lfsSelectionVersion || selected !== id) return;
		lfsLoading = true;
		lfsError = '';
		lfsEndpoint = '';
		lfsOrigins = '';
		lfsAllowInternal = false;
		lfsInternalConfirmation = false;
		lfsSuggestedEndpoint = '';
		try {
			const result: {
				settings?: {
					endpoint?: string | null;
					objectOrigins?: string[] | null;
					allowInternal?: boolean | null;
				} | null;
				suggestedEndpoint?: string | null;
			} = await request(`/api/projects/${id}/lfs`);
			if (isCurrent()) {
				lfsEndpoint = result.settings?.endpoint ?? '';
				lfsOrigins = (result.settings?.objectOrigins ?? []).join('\n');
				lfsAllowInternal = result.settings?.allowInternal ?? false;
				lfsSuggestedEndpoint = result.suggestedEndpoint ?? '';
			}
		} catch (cause) {
			if (isCurrent())
				lfsError = cause instanceof Error ? cause.message : 'Could not load LFS settings.';
		} finally {
			if (isCurrent()) lfsLoading = false;
		}
	}
	async function saveLfsSettings(event: SubmitEvent) {
		event.preventDefault();
		if (lfsBusy || !selected) return;
		const projectId = selected;
		const selectionVersion = lfsSelectionVersion;
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
			await request(`/api/projects/${projectId}/lfs`, {
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
			if (selectionVersion === lfsSelectionVersion && selected === projectId) {
				await loadLfsSettings(projectId);
				if (selectionVersion === lfsSelectionVersion && selected === projectId)
					lfsNotice = 'LFS settings saved.';
			}
		} catch (cause) {
			if (selectionVersion === lfsSelectionVersion && selected === projectId)
				lfsError = cause instanceof Error ? cause.message : 'Could not save LFS settings.';
		} finally {
			if (selectionVersion === lfsSelectionVersion && selected === projectId) lfsBusy = false;
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
			projectDeleteConfirmation = '';
			await goto('/dashboard/projects');
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
			if (cacheRecreateConfirmation === project.id) closeCacheRecreateConfirmation(true);
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
		cacheRecreateTriggerProject = project.id;
		cacheRecreateConfirmation = project.id;
		cacheRecreateError = '';
		cacheRecreateNotice = '';
		await tick();
		document.getElementById('cache-recreate-confirm')?.focus();
	}
	function closeCacheRecreateConfirmation(restoreFocus = true) {
		cacheRecreateConfirmation = '';
		const trigger = cacheRecreateTrigger;
		const projectId = cacheRecreateTriggerProject;
		cacheRecreateTrigger = undefined;
		cacheRecreateTriggerProject = '';
		if (restoreFocus)
			void tick().then(() => {
				const target = trigger?.isConnected
					? trigger
					: document.querySelector<HTMLButtonElement>(
							`[data-cache-recreate-trigger="${CSS.escape(projectId)}"]`
						);
				target?.focus();
			});
	}
	function trapCacheRecreateFocus(event: KeyboardEvent) {
		if (event.key === 'Escape' && !cacheRecreateBusy) {
			event.preventDefault();
			closeCacheRecreateConfirmation();
			return;
		}
		const dialog = document.getElementById('cache-recreate-dialog');
		if (event.key !== 'Tab' || !dialog) return;
		const focusable = Array.from(dialog.querySelectorAll<HTMLElement>('button:not(:disabled)'));
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
	async function loadDetail(id: string) {
		const version = ++lfsSelectionVersion;
		lfsLoadVersion++;
		loading = true;
		missing = false;
		error = '';
		projects = [];
		refs = {};
		search = '';
		lfsBusy = false;
		closeCacheRecreateConfirmation(false);
		cacheRecreateError = '';
		cacheRecreateNotice = '';
		try {
			const loaded: Project[] = await request('/api/projects');
			if (version !== lfsSelectionVersion || selected !== id) return;
			projects = loaded;
			const project = loaded.find((item) => item.id === id);
			if (!project) {
				missing = true;
				return;
			}
			keyAssignments[id] = project.keyId ?? '';
			void loadLfsSettings(id);
			try {
				const loadedRefs: Ref[] = await request(`/api/projects/${id}/refs`);
				if (version === lfsSelectionVersion && selected === id) refs[id] = loadedRefs;
			} catch (cause) {
				if (version === lfsSelectionVersion && selected === id)
					error = cause instanceof Error ? cause.message : 'Could not load branches and tags.';
			}
		} catch (cause) {
			if (version === lfsSelectionVersion && selected === id)
				error = cause instanceof Error ? cause.message : 'Could not load projects.';
		} finally {
			if (version === lfsSelectionVersion && selected === id) loading = false;
		}
	}
	afterNavigate(() => {
		void loadDetail(selected);
		void loadRepositoryKeys();
	});
</script>

<svelte:head
	><title>{projects.find((item) => item.id === selected)?.name ?? 'Project'} · Deploy Manager</title
	></svelte:head
>
<main class="mx-auto w-[min(100%-2rem,64rem)] min-w-0 py-10">
	<a
		class="text-sm font-semibold text-[#214c30] underline underline-offset-4"
		href="/dashboard/projects">Projects</a
	>
	{#if loading}
		<h1 class="mt-5 text-3xl font-semibold text-[#243528]">Loading project…</h1>
	{:else if missing}
		<h1 class="mt-5 text-3xl font-semibold text-[#243528]">Project not found</h1>
		<p class="mt-3 text-[#58645b]">This project may have been deleted.</p>
	{:else if projects.find((item) => item.id === selected)}
		<h1 class="mt-5 text-3xl font-semibold tracking-[-0.03em] text-[#243528]">
			{projects.find((item) => item.id === selected)?.name}
		</h1>
		<p class="mt-2 break-all text-[#58645b]">
			{projects.find((item) => item.id === selected)?.url}
		</p>
	{:else}
		<h1 class="mt-5 text-3xl font-semibold text-[#243528]">Project unavailable</h1>
	{/if}
	{#if error}<p
			role="alert"
			class="mt-4 rounded-lg border border-red-200 bg-red-50 p-3 text-red-800"
		>
			{error}
		</p>{/if}
	{#if !loading && !missing && projects.find((item) => item.id === selected)}
		{@const project = projects.find((item) => item.id === selected)}
		<section class="mt-8 min-w-0">
			<div class="flex flex-wrap items-center justify-between gap-3">
				<h2 class="m-0 text-xl font-semibold text-[#243528]">Branches and tags</h2>
				<button
					class="min-h-10 rounded-lg border border-[#aeb9ae] px-4 font-semibold text-[#214c30] disabled:opacity-60"
					aria-label="Refresh project branches and tags"
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
						id="cache-recreate-dialog"
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
								id="cache-recreate-confirm"
								class="min-h-9 rounded-lg bg-amber-900 px-3 text-sm font-semibold text-white disabled:opacity-60"
								onclick={() => project && recreateCache(project)}
								disabled={cacheRecreateBusy || !project}>Clear cache</button
							>
							<button
								class="min-h-9 rounded-lg border border-[#aeb9ae] px-3 text-sm font-semibold disabled:opacity-60"
								onclick={() => closeCacheRecreateConfirmation()}
								disabled={cacheRecreateBusy}>Cancel</button
							>
						</div>
					</div>
				{:else}
					<button
						id="cache-recreate-trigger"
						data-cache-recreate-trigger={project?.id ?? ''}
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
						{#each repositoryKeys as key (key.id)}<option value={key.id}>{key.name}</option>{/each}
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
						><span class="font-medium break-all">{ref.name}</span><code
							class="mt-1 block text-xs break-all text-[#58645b]">{ref.commit}</code
						>
					</li>{:else}<li class="list-none py-4 text-[#58645b]">No matching refs.</li>{/each}
			</ul>
		</section>
		<section class="mt-8 min-w-0 rounded-xl border border-[#e1e6df] bg-white p-5">
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
							class="min-h-11 min-w-0 rounded-lg border border-[#aeb9ae] px-3"
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
							class="min-h-28 min-w-0 rounded-lg border border-[#aeb9ae] p-3 font-mono text-sm"
							bind:value={lfsOrigins}
							placeholder="https://objects.example.com"
							aria-describedby="lfs-origins-note"></textarea>
					</label>
					<p id="lfs-origins-note" class="-mt-3 text-sm text-[#58645b]">
						Enter one exact HTTPS origin per line, such as https://objects.example.com:8443. Do not
						include paths or credentials.
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
		</section>
		<div class="min-w-0">
			<Deployments projectId={selected} />
		</div>
		<div class="min-w-0">
			{#key selected}
				<WebhookSetup projectId={selected} provider="github" />
				<WebhookSetup projectId={selected} provider="gitea" />
			{/key}
		</div>
	{/if}
</main>
