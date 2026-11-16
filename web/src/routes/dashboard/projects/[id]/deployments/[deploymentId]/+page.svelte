<script lang="ts">
	import { afterNavigate, goto } from '$app/navigation';
	import { page } from '$app/state';
	import { onDestroy } from 'svelte';
	import RollbackHistory from '#lib/components/RollbackHistory.svelte';

	type Ref = { kind: 'branch' | 'tag'; name: string; commit: string };
	type Project = { id: string; name: string; lastFetched: string | null };
	type Deployment = {
		id: string;
		projectId: string;
		name: string;
		targetKind: string;
		automationPaused: boolean;
		targetName: string;
		commit: string;
		liveCommit?: string;
		status: string;
		error?: string;
		publicationPath: string;
	};
	type Job = {
		id: string;
		status: string;
		log: string;
		commit: string;
		force: boolean;
		createdAt: string;
		updatedAt: string;
	};
	let projectId = $derived(page.params.id);
	let deploymentId = $derived(page.params.deploymentId);
	let project = $state.raw<Project | null>(null);
	let item = $state.raw<Deployment | null>(null);
	let refs = $state.raw<Ref[]>([]);
	let refsLoaded = false;
	let jobs = $state.raw<Job[] | null>(null);
	let historyStatus = $state<'loading' | 'loaded' | 'error'>('loading');
	let historyError = $state('');
	let targetSelection = $state('');
	let targetSearch = $state('');
	let error = $state('');
	let loading = $state(true);
	let missing = $state(false);
	let busy = $state(false);
	let deleteConfirmation = $state(false);
	let loadVersion = 0;
	let viewVersion = 0;
	const key = (ref: Ref) => `${ref.kind}:${ref.name}`;
	let matchingRefs = $derived(
		refs.filter((ref) => ref.name.toLowerCase().includes(targetSearch.toLowerCase()))
	);
	const jobStatus = (job: Job) =>
		job.status === 'failed' && job.log.toLowerCase().includes('interrupted')
			? 'Interrupted'
			: ({
					succeeded: 'Succeeded',
					noop: 'No changes',
					failed: 'Failed',
					superseded: 'Superseded',
					interrupted: 'Interrupted',
					queued: 'Queued',
					running: 'Running'
				}[job.status] ?? job.status);

	async function request(path: string, init?: RequestInit) {
		const response = await fetch(path, {
			...init,
			credentials: 'same-origin',
			headers: { 'Content-Type': 'application/json', ...init?.headers }
		});
		if (!response.ok && path.endsWith('/delete') && response.status === 503)
			throw new Error(
				'Deletion is pending. The published files are protected while cleanup retries; try again later.'
			);
		if (!response.ok)
			throw new Error(
				response.status >= 500
					? 'The request could not be completed. Try again shortly.'
					: 'The request was rejected. Refresh and try again.'
			);
		return response.status === 204 ? null : response.json();
	}

	async function load(id = projectId, deployment = deploymentId, currentView = viewVersion) {
		const version = ++loadVersion;
		const current = () =>
			version === loadVersion &&
			currentView === viewVersion &&
			id === projectId &&
			deployment === deploymentId;
		try {
			const [all, projects]: [Deployment[], Project[]] = await Promise.all([
				request('/api/deployments'),
				request('/api/projects')
			]);
			if (!current()) return;
			const foundProject = projects.find((candidate) => candidate.id === id);
			const found = all.find(
				(candidate) => candidate.id === deployment && candidate.projectId === id
			);
			if (!foundProject || !found) {
				project = null;
				item = null;
				missing = true;
				loading = false;
				jobs = null;
				return;
			}
			project = foundProject;
			const previousTarget = item && `${item.targetKind}:${item.targetName}`;
			item = found;
			if (!targetSelection || previousTarget !== `${found.targetKind}:${found.targetName}`)
				targetSelection =
					found.targetKind === 'branch' || found.targetKind === 'tag'
						? `${found.targetKind}:${found.targetName}`
						: `commit:${found.commit}`;
			loading = false;
			missing = false;
			// Only fetch deployment-scoped data after the project/deployment association is verified.
			const [refsResult, jobsResult] = await Promise.allSettled([
				request(`/api/projects/${id}/refs`) as Promise<Ref[]>,
				request(`/api/deployments/${deployment}/jobs`) as Promise<Job[]>
			]);
			if (!current()) return;
			if (refsResult.status === 'fulfilled') {
				refs = refsResult.value;
				refsLoaded = true;
			} else {
				const message =
					refsResult.reason instanceof Error
						? refsResult.reason.message
						: 'Could not load targets.';
				error = refsLoaded
					? `${message} Refresh and try again. Your current branches and tags are still shown.`
					: message;
			}
			if (jobsResult.status === 'fulfilled') {
				jobs = jobsResult.value;
				historyStatus = 'loaded';
				historyError = '';
			} else {
				historyStatus = 'error';
				historyError = 'The history could not be refreshed. Try again shortly.';
			}
		} catch (cause) {
			if (!current()) return;
			loading = false;
			error = cause instanceof Error ? cause.message : 'Could not load deployment.';
		}
	}

	async function refreshRefs() {
		if (busy || !project || !item) return;
		busy = true;
		error = '';
		try {
			await request(`/api/projects/${projectId}/refresh`, { method: 'POST', body: '{}' });
			await load();
		} catch (cause) {
			const message = cause instanceof Error ? cause.message : 'Could not refresh targets.';
			error = refsLoaded
				? `${message} Refresh and try again. Your current branches and tags are still shown.`
				: message;
		} finally {
			busy = false;
		}
	}

	async function runDeploymentAction(path: string, fallbackError: string) {
		if (busy || !item || !project) return;
		busy = true;
		error = '';
		try {
			await request(path, { method: 'POST', body: '{}' });
			await load();
		} catch (cause) {
			error = cause instanceof Error ? cause.message : fallbackError;
		} finally {
			busy = false;
		}
	}
	async function deploy(id: string) {
		return runDeploymentAction(`/api/deployments/${id}/deploy`, 'Could not start deployment.');
	}
	async function hardRedeploy(id: string) {
		return runDeploymentAction(
			`/api/deployments/${id}/hard-redeploy`,
			'Could not start hard re-deploy.'
		);
	}
	async function setAutomationPaused(id: string, paused: boolean) {
		if (item?.targetKind !== 'branch') return;
		return runDeploymentAction(
			`/api/deployments/${id}/${paused ? 'pause' : 'resume'}`,
			'Could not update automatic deployment.'
		);
	}
	async function changeTarget(id: string) {
		const target = refs.find((ref) => key(ref) === targetSelection);
		if (busy || !item || !project || !target) return;
		busy = true;
		error = '';
		try {
			await request(`/api/deployments/${id}/target`, {
				method: 'POST',
				body: JSON.stringify({
					targetKind: target.kind,
					targetName: target.name,
					commit: target.commit
				})
			});
			await load();
		} catch (cause) {
			error = cause instanceof Error ? cause.message : 'Could not change the deployment target.';
		} finally {
			busy = false;
		}
	}
	async function deleteDeployment(id: string) {
		if (busy || !item || !project || !deleteConfirmation) return;
		busy = true;
		error = '';
		try {
			await request(`/api/deployments/${id}/delete`, {
				method: 'POST',
				body: JSON.stringify({ confirm: true })
			});
			deleteConfirmation = false;
			await goto(`/dashboard/projects/${projectId}`);
		} catch (cause) {
			error = cause instanceof Error ? cause.message : 'Could not delete the deployment.';
		} finally {
			busy = false;
		}
	}

	let timeout: number | undefined;
	afterNavigate(() => {
		const id = projectId;
		const deployment = deploymentId;
		const version = ++viewVersion;
		++loadVersion;
		if (timeout !== undefined) window.clearTimeout(timeout);
		project = null;
		item = null;
		refs = [];
		refsLoaded = false;
		jobs = null;
		historyStatus = 'loading';
		missing = false;
		loading = true;
		error = '';
		targetSelection = '';
		targetSearch = '';
		deleteConfirmation = false;
		const poll = async () => {
			if (version !== viewVersion) return;
			await load(id, deployment, version);
			if (version === viewVersion && !missing) timeout = window.setTimeout(poll, 2000);
		};
		void poll();
	});
	onDestroy(() => {
		++viewVersion;
		++loadVersion;
		if (timeout !== undefined) window.clearTimeout(timeout);
	});
</script>

<svelte:head><title>{item?.name ?? 'Deployment'} · Deploy Manager</title></svelte:head>
<main class="mx-auto w-[min(100%-2rem,64rem)] min-w-0 py-10">
	<a
		class="text-sm font-semibold text-[#214c30] underline underline-offset-4"
		href={`/dashboard/projects/${projectId}`}>{project?.name ?? 'Project'}</a
	>
	{#if loading}
		<h1 class="mt-5 text-3xl font-semibold text-[#243528]">Loading deployment…</h1>
	{:else if missing}
		<h1 class="mt-5 text-3xl font-semibold text-[#243528]">Deployment not found</h1>
		<p class="mt-3 text-[#58645b]">
			This deployment may have been deleted or does not belong to this project.
		</p>
	{:else if item && project}
		<h1 class="mt-5 text-3xl font-semibold text-[#243528]">{item.name}</h1>
	{:else}
		<h1 class="mt-5 text-3xl font-semibold text-[#243528]">Deployment unavailable</h1>
	{/if}
	{#if error}<p
			role="alert"
			class="mt-4 rounded-lg border border-red-200 bg-red-50 p-3 text-red-800"
		>
			{error}
		</p>{/if}
	{#if item && project && !missing}
		<section
			class="mt-8 rounded-xl border border-[#e1e6df] bg-white p-5"
			aria-label={`Deployment details for ${item.name}`}
		>
			<h2 class="m-0 text-xl font-semibold text-[#243528]">Publication</h2>
			<p class="mt-2 text-sm text-[#58645b]">
				Publication path: <code class="break-all">{item.publicationPath}</code>
			</p>
			<div class="mt-3 flex flex-wrap items-center justify-between gap-3">
				<p class="text-sm text-[#58645b]">
					{project.lastFetched
						? `Last fetched ${new Date(project.lastFetched).toLocaleString()}.`
						: 'Targets have not been fetched yet.'}
				</p>
				<button
					class="min-h-9 rounded-lg border border-[#aeb9ae] px-3 text-sm font-semibold text-[#214c30] disabled:opacity-60"
					onclick={refreshRefs}
					disabled={busy}>Refresh branches and tags</button
				>
			</div>
			<div class="flex flex-wrap items-start justify-between gap-3">
				<div>
					<p class="mt-1 text-sm text-[#58645b]">
						{item.status === 'live' ? 'Live' : item.status}
					</p>
					{#if item.targetKind === 'branch'}
						<p class="mt-1 text-sm font-medium text-[#354238]">
							{item.automationPaused
								? 'Automatic deployment paused'
								: 'Automatic deployment enabled'}
						</p>
						{#if item.automationPaused}
							<p class="mt-1 text-sm text-[#58645b]">
								Manual Deploy remains available. Resume deploys the latest branch tip.
							</p>
						{/if}
					{/if}
				</div>
				{#if item.targetKind === 'branch'}
					<button
						class="min-h-9 rounded-lg border border-[#aeb9ae] px-3 text-sm font-semibold text-[#214c30] disabled:opacity-60"
						onclick={() => item && setAutomationPaused(item.id, !item.automationPaused)}
						disabled={busy}>{item.automationPaused ? 'Resume' : 'Pause'}</button
					>
				{/if}
				<button
					class="min-h-9 rounded-lg border border-[#aeb9ae] px-3 text-sm font-semibold text-[#214c30] disabled:opacity-60"
					onclick={() => item && deploy(item.id)}
					disabled={busy}>Deploy</button
				>
				<button
					class="min-h-9 rounded-lg border border-[#214c30] bg-[#214c30] px-3 text-sm font-semibold text-white disabled:opacity-60"
					onclick={() => item && hardRedeploy(item.id)}
					disabled={busy}>Hard re-deploy</button
				>
				<button
					class="min-h-9 rounded-lg border border-red-300 px-3 text-sm font-semibold text-red-800 disabled:opacity-60"
					onclick={() => (deleteConfirmation = true)}
					disabled={busy}>Delete</button
				>
			</div>
			{#if deleteConfirmation}
				<div
					class="mt-3 rounded-lg border border-red-200 bg-red-50 p-3"
					aria-label="Confirm deployment deletion"
				>
					<p class="m-0 text-sm text-red-900">
						Delete “{item.name}” and remove its published files? This cannot be undone.
					</p>
					<div class="mt-3 flex gap-2">
						<button
							class="min-h-9 rounded-lg bg-red-800 px-3 text-sm font-semibold text-white disabled:opacity-60"
							onclick={() => item && deleteDeployment(item.id)}
							disabled={busy}>Confirm delete</button
						>
						<button
							class="min-h-9 rounded-lg border border-[#aeb9ae] px-3 text-sm font-semibold disabled:opacity-60"
							onclick={() => (deleteConfirmation = false)}
							disabled={busy}>Cancel</button
						>
					</div>
				</div>
			{/if}
			<p class="mt-2 text-sm text-[#58645b]">
				{item.liveCommit ? `Live commit: ${item.liveCommit}` : ''}
				{#if item.targetKind === 'branch' || item.targetKind === 'tag'}
					{item.liveCommit ? ' · ' : ''}Target {item.targetKind}: {item.targetName}
				{:else if !item.liveCommit}
					Target commit: {item.commit}
				{/if}
			</p>
			<div class="mt-3 flex flex-wrap items-end gap-2">
				<label class="grid min-w-64 flex-1 gap-1 text-sm font-medium text-[#354238]"
					>Change target
					<input
						class="min-h-10 rounded-lg border border-[#aeb9ae] px-3"
						aria-label={`Search targets for ${item.name}`}
						placeholder="Search branches and tags"
						bind:value={targetSearch}
					/>
					<select
						class="min-h-10 rounded-lg border border-[#aeb9ae] px-3"
						aria-label={`Target for ${item.name}`}
						bind:value={targetSelection}
					>
						{#if item.targetKind === 'commit' || !refs.some((ref) => key(ref) === targetSelection) || !refs.some((ref) => key(ref) === targetSelection && ref.name
										.toLowerCase()
										.includes(targetSearch.toLowerCase()))}<option value={targetSelection} disabled
								>{item.targetKind === 'commit'
									? `Current commit · ${item.commit}`
									: `Current target · ${item.targetName}`}</option
							>{/if}
						{#each matchingRefs as ref (key(ref))}<option value={key(ref)}
								>{ref.kind}: {ref.name}</option
							>{/each}
					</select>
				</label>
				<button
					class="min-h-10 rounded-lg border border-[#aeb9ae] px-3 text-sm font-semibold text-[#214c30] disabled:opacity-60"
					onclick={() => item && changeTarget(item.id)}
					disabled={busy ||
						!refs.some((ref) => key(ref) === targetSelection) ||
						targetSelection === `${item.targetKind}:${item.targetName}`}>Change target</button
				>
			</div>
			{#if item.targetKind === 'branch' && !['deleting', 'deletion_failed', 'recovery_required'].includes(item.status)}
				{#key `${item.id}:${item.targetName}`}
					<RollbackHistory
						deploymentId={item.id}
						deploymentName={item.name}
						branch={item.targetName}
						bind:busy
						onqueued={() => load()}
					/>
				{/key}
			{/if}
			{#if item.error}<p role="alert" class="mt-2 text-sm text-red-800">{item.error}</p>{/if}
			<section
				class="mt-5 border-t border-[#e1e6df] pt-4"
				aria-label={`Job history for ${item.name}`}
			>
				<h4 class="m-0 text-base font-semibold text-[#243528]">Deployment history</h4>
				{#if historyStatus === 'loading' && !jobs}
					<p class="mt-2 text-sm text-[#58645b]">Loading history…</p>
				{:else if historyStatus === 'error'}
					<p role="alert" class="mt-2 text-sm text-red-800">
						Could not load deployment history; refresh failed: {historyError} The app will retry automatically;
						refresh the page to retry now.
					</p>
				{/if}
				{#if jobs?.length}
					<ul class="mt-3 grid gap-3 p-0">
						{#each jobs as job (job.id)}
							<li class="list-none rounded-lg border border-[#d5ddd4] bg-[#fafbf9] p-3">
								<details>
									<summary class="cursor-pointer font-semibold text-[#214c30]">
										<span
											class="inline-flex min-h-7 items-center rounded-full border border-[#aeb9ae] px-2 text-xs"
											>{jobStatus(job)}</span
										>
										<span class="ml-2"
											>{job.force ? 'Hard re-deploy' : 'Deployment'} · {new Date(
												job.createdAt
											).toLocaleString()}</span
										>
									</summary>
									<p class="mt-2 text-sm text-[#58645b]">Commit: <code>{job.commit}</code></p>
									<p class="mt-1 text-xs text-[#58645b]">
										Updated {new Date(job.updatedAt).toLocaleString()}
									</p>
									<h5 class="mt-3 mb-1 text-sm font-semibold text-[#354238]">Job log</h5>
									<pre
										class="m-0 max-h-72 overflow-auto rounded bg-[#eef2ec] p-3 text-xs whitespace-pre-wrap">{job.log ||
											'No log output.'}</pre>
								</details>
							</li>
						{/each}
					</ul>
				{:else if historyStatus === 'loaded'}
					<p class="mt-2 text-sm text-[#58645b]">No deployment history yet.</p>
				{/if}
			</section>
		</section>
	{/if}
</main>
