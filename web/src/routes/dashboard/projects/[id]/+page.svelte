<script lang="ts">
	import { BrowserSDKError, sdk, serverErrorCode } from '#lib/sdk/index.js';
	import { formatBytes } from '#lib/format-bytes.js';
	import { onDestroy, tick } from 'svelte';
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
	let projectReadController: AbortController | undefined;
	let refreshController: AbortController | undefined;
	let cacheReadController: AbortController | undefined;
	let actionController: AbortController | undefined;
	let selected = $derived(page.params.id ?? '');
	let project = $derived(projects.find((item) => item.id === selected));
	let loading = $state(true);
	let missing = $state(false);
	let visibleRefs = $derived(
		(refs[selected] ?? []).filter((ref) =>
			`${ref.kind} ${ref.name}`.toLowerCase().includes(search.toLowerCase())
		)
	);

	async function loadProjects() {
		projectReadController?.abort();
		const controller = new AbortController();
		projectReadController = controller;
		const projectId = selected;
		try {
			const loaded = (await sdk.projects.list(controller.signal)).data;
			if (controller.signal.aborted || selected !== projectId) return;
			projects = loaded;
			for (const project of projects) keyAssignments[project.id] = project.keyId ?? '';
			missing = !projects.some((project) => project.id === selected);
		} catch (cause) {
			if (!controller.signal.aborted && selected === projectId)
				error = cause instanceof Error ? cause.message : 'Could not load projects.';
		}
	}
	async function loadRepositoryKeys() {
		try {
			repositoryKeys = (await sdk.repositoryKeys.list()).data;
		} catch (cause) {
			error = cause instanceof Error ? cause.message : 'Could not load repository keys.';
		}
	}
	async function loadSshHosts() {
		const controller = refreshController;
		try {
			const result = (await sdk.sshHosts.list(controller?.signal)).data;
			if (controller && !controller.signal.aborted && selected === refreshControllerProject)
				sshHosts = result;
		} catch {
			/* Keep the original project error visible. */
		}
	}
	let refreshControllerProject = '';
	async function assignKey(project: Project) {
		actionController?.abort();
		const controller = new AbortController();
		actionController = controller;
		const projectId = project.id;
		error = '';
		try {
			await sdk.projects.assignKey(
				projectId,
				{ keyId: keyAssignments[projectId] },
				controller.signal
			);
		} catch (cause) {
			if (!controller.signal.aborted && selected === projectId)
				error = cause instanceof Error ? cause.message : 'Could not assign the key.';
		}
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
			const result = (await sdk.lfs.settings(id)).data;
			if (isCurrent()) {
				lfsEndpoint = result.settings.endpoint;
				lfsOrigins = result.settings.objectOrigins.join('\n');
				lfsAllowInternal = result.settings.allowInternal;
				lfsSuggestedEndpoint = result.suggestedEndpoint;
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
			await sdk.lfs.save(projectId, {
				endpoint: lfsEndpoint,
				objectOrigins: lfsOrigins
					.split(/\r?\n/)
					.map((origin) => origin.trim())
					.filter(Boolean),
				allowInternal: lfsAllowInternal,
				confirmation: lfsConfirmation,
				...(lfsAllowInternal ? { internalConfirmation: lfsConfirmation } : {})
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
		refreshController?.abort();
		const controller = new AbortController();
		refreshController = controller;
		refreshControllerProject = project.id;
		busy = true;
		try {
			await sdk.projects.refresh(project.id, controller.signal);
			const refreshedRefs: Ref[] = (await sdk.projects.refs(project.id, controller.signal)).data;
			const refreshedProjects: Project[] = (await sdk.projects.list(controller.signal)).data;
			if (controller.signal.aborted || selected !== project.id) return;
			refs[project.id] = refreshedRefs;
			projects = refreshedProjects;
			error = '';
		} catch (cause) {
			if (controller.signal.aborted || selected !== project.id) return;
			const message =
				project.url.startsWith('ssh://') &&
				['git.target_unavailable', 'git.authentication_failed'].includes(
					serverErrorCode(cause) ?? ''
				)
					? 'SSH connection failed. Check the repository key and SSH host warnings.'
					: cause instanceof Error
						? cause.message
						: 'The request could not be completed. Try again.';
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
			if (refreshController === controller) {
				refreshController = undefined;
				busy = false;
			}
		}
	}
	async function deleteProject(project: Project) {
		if (busy || projectDeleteConfirmation !== project.id) return;
		const projectId = project.id;
		const selectionVersion = lfsSelectionVersion;
		const isCurrent = () => selectionVersion === lfsSelectionVersion && selected === projectId;
		busy = true;
		error = '';
		try {
			await sdk.projects.delete(projectId);
			if (!isCurrent()) return;
			projects = projects.filter((item) => item.id !== projectId);
			delete refs[projectId];
			delete keyAssignments[projectId];
			projectDeleteConfirmation = '';
			await goto('/dashboard/projects');
		} catch (cause) {
			if (!isCurrent()) return;
			const code = serverErrorCode(cause);
			const cleanupAccepted =
				cause instanceof BrowserSDKError &&
				cause.outcome.kind === 'server' &&
				cause.outcome.code === 'operation.cleanup_pending' &&
				typeof cause.outcome.data === 'object' &&
				cause.outcome.data !== null &&
				'operationAccepted' in cause.outcome.data &&
				cause.outcome.data.operationAccepted === true &&
				'recoveryRequired' in cause.outcome.data &&
				cause.outcome.data.recoveryRequired === true;
			error =
				code === 'project.has_deployments'
					? "Remove the project's deployments before deleting it."
					: cleanupAccepted
						? 'Project deletion was accepted, but cache cleanup is still pending. Refresh to check its status; do not submit the deletion again.'
						: code === 'operation.cleanup_pending'
							? 'Could not confirm whether project deletion was accepted. Refresh to check its status before trying again.'
							: cause instanceof Error
								? cause.message
								: 'Could not confirm whether the project was deleted. Refresh to check its status before trying again.';
			if (code === 'project.has_deployments') projectDeleteConfirmation = '';
			if (
				cleanupAccepted ||
				(cause instanceof BrowserSDKError &&
					(cause.outcome.kind !== 'server' || cause.outcome.status >= 500))
			)
				projectDeleteConfirmation = '';
			if (!cleanupAccepted) await loadProjects();
		} finally {
			if (isCurrent()) busy = false;
		}
	}
	async function recreateCache(project: Project) {
		if (cacheRecreateBusy || cacheRecreateConfirmation !== project.id) return;
		cacheReadController?.abort();
		const readController = new AbortController();
		cacheReadController = readController;
		cacheRecreateBusy = true;
		cacheRecreateError = '';
		cacheRecreateNotice = '';
		let storageCleared = false;
		try {
			await sdk.projects.clearCache(project.id, readController.signal);
			storageCleared = true;
			const refreshedProjects: Project[] = (await sdk.projects.list(readController.signal)).data;
			if (readController.signal.aborted || selected !== project.id) return;
			projects = refreshedProjects;
			if (cacheRecreateConfirmation === project.id) closeCacheRecreateConfirmation(true);
			if (selected === project.id)
				cacheRecreateNotice = 'Repository storage cleared. Sizes updated.';
		} catch (cause) {
			if (readController.signal.aborted || selected !== project.id) return;
			cacheRecreateError = storageCleared
				? 'Storage was cleared, but the sizes could not be refreshed. Reload the page to check them.'
				: 'Could not clear repository storage. Try again.';
		} finally {
			if (cacheReadController === readController) {
				cacheReadController = undefined;
				cacheRecreateBusy = false;
			}
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
		projectReadController?.abort();
		cacheReadController?.abort();
		refreshController?.abort();
		actionController?.abort();
		actionController = undefined;
		refreshController = undefined;
		busy = false;
		const controller = new AbortController();
		projectReadController = controller;
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
			const loaded: Project[] = (await sdk.projects.list(controller.signal)).data;
			if (controller.signal.aborted || version !== lfsSelectionVersion || selected !== id) return;
			projects = loaded;
			const project = loaded.find((item) => item.id === id);
			if (!project) {
				missing = true;
				return;
			}
			keyAssignments[id] = project.keyId ?? '';
			void loadLfsSettings(id);
			try {
				const loadedRefs: Ref[] = (await sdk.projects.refs(id, controller.signal)).data;
				if (!controller.signal.aborted && version === lfsSelectionVersion && selected === id)
					refs[id] = loadedRefs;
			} catch (cause) {
				if (!controller.signal.aborted && version === lfsSelectionVersion && selected === id)
					error = cause instanceof Error ? cause.message : 'Could not load branches and tags.';
			}
		} catch (cause) {
			if (!controller.signal.aborted && version === lfsSelectionVersion && selected === id)
				error = cause instanceof Error ? cause.message : 'Could not load projects.';
		} finally {
			if (!controller.signal.aborted && version === lfsSelectionVersion && selected === id)
				loading = false;
		}
	}
	afterNavigate(() => {
		void loadDetail(selected);
		void loadRepositoryKeys();
	});
	onDestroy(() => {
		projectReadController?.abort();
		cacheReadController?.abort();
		refreshController?.abort();
		actionController?.abort();
		actionController = undefined;
	});
</script>

<svelte:head><title>{project?.name ?? 'Project'} · Deploy Manager</title></svelte:head>
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
	{:else if project}
		<h1 class="mt-5 text-3xl font-semibold tracking-[-0.03em] text-[#243528]">
			{project.name}
		</h1>
		<p class="mt-2 break-all text-[#58645b]">
			{project.url}
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
	{#if !loading && !missing && project}
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
					{project.warning} Your current branches and tags are still shown. For SSH issues,
					<a class="font-semibold underline" href="/dashboard/ssh-hosts">review SSH host warnings</a
					> and verify the fingerprint independently before approval.
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
				<p class="mt-4 text-sm text-[#58645b]">
					Need another key? <a class="font-semibold text-[#214c30] underline" href="/dashboard/keys"
						>Manage repository keys</a
					>.
					<a class="font-semibold text-[#214c30] underline" href="/dashboard/ssh-hosts"
						>Review SSH host warnings</a
					> if the host is unrecognized.
				</p>
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
			{#key selected}
				<Deployments projectId={selected} />
			{/key}
		</div>
		<div class="min-w-0">
			{#key selected}
				<WebhookSetup projectId={selected} provider="github" />
				<WebhookSetup projectId={selected} provider="gitea" />
			{/key}
		</div>
	{/if}
</main>
