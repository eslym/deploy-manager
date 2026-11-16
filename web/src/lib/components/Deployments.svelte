<script lang="ts">
	import RollbackHistory from './RollbackHistory.svelte';

	type Ref = { kind: 'branch' | 'tag'; name: string; commit: string };
	type Project = { id: string; lastFetched: string | null };
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
	let { projectId }: { projectId: string } = $props();
	let refs = $state<Ref[]>([]);
	let refsLoaded = false;
	let deploymentsLoaded = $state(false);
	let lastFetched = $state<string | null>(null);
	let deployments = $state<Deployment[]>([]);
	let jobs = $state<Record<string, Job[]>>({});
	let jobHistoryState = $state<
		Record<string, { status: 'loading' | 'loaded' | 'error'; error?: string }>
	>({});
	let targetSelections = $state<Record<string, string>>({});
	let targetSearches = $state<Record<string, string>>({});
	let name = $state('');
	let selectedRef = $state('');
	let refSearch = $state('');
	let error = $state('');
	let busy = $state(false);
	let deleteConfirmation = $state('');
	let loadVersion = 0;
	const key = (ref: Ref) => `${ref.kind}:${ref.name}`;
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
	let filteredRefs = $derived(
		refs.filter((ref) => ref.name.toLowerCase().includes(refSearch.toLowerCase()))
	);
	let selectedTarget = $derived(refs.find((ref) => key(ref) === selectedRef));
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
	async function load(id = projectId, viewVersion = loadViewVersion) {
		const version = ++loadVersion;
		try {
			const [all, projects, cachedRefs]: [Deployment[], Project[], Ref[]] = await Promise.all([
				request('/api/deployments'),
				request('/api/projects'),
				request(`/api/projects/${id}/refs`)
			]);
			if (version !== loadVersion || viewVersion !== loadViewVersion || id !== projectId) return;
			refs = cachedRefs;
			refsLoaded = true;
			lastFetched = projects.find((project) => project.id === id)?.lastFetched ?? null;
			const current = all.filter((item) => item.projectId === id);
			deployments = current;
			deploymentsLoaded = true;
			const currentIds = new Set(current.map((item) => item.id));
			for (const jobId of Object.keys(jobs)) if (!currentIds.has(jobId)) delete jobs[jobId];
			for (const jobId of Object.keys(jobHistoryState))
				if (!currentIds.has(jobId)) delete jobHistoryState[jobId];
			for (const item of current) {
				targetSelections[item.id] ??=
					item.targetKind === 'branch' || item.targetKind === 'tag'
						? `${item.targetKind}:${item.targetName}`
						: `commit:${item.commit}`;
			}
			await Promise.all(
				current.map(async (item) => {
					if (!jobs[item.id]) jobHistoryState[item.id] = { status: 'loading' };
					try {
						const history: Job[] = await request(`/api/deployments/${item.id}/jobs`);
						if (version !== loadVersion || viewVersion !== loadViewVersion || id !== projectId)
							return;
						jobs[item.id] = history;
						jobHistoryState[item.id] = { status: 'loaded' };
					} catch (cause) {
						if (version !== loadVersion || viewVersion !== loadViewVersion || id !== projectId)
							return;
						jobHistoryState[item.id] = {
							status: 'error',
							error:
								cause instanceof Error &&
								[
									'The request could not be completed. Try again shortly.',
									'The request was rejected. Refresh and try again.'
								].includes(cause.message)
									? cause.message
									: 'The history could not be refreshed. Try again shortly.'
						};
					}
				})
			);
			if (version !== loadVersion || viewVersion !== loadViewVersion || id !== projectId) return;
		} catch (cause) {
			if (version === loadVersion && viewVersion === loadViewVersion && id === projectId) {
				const message = cause instanceof Error ? cause.message : 'Could not load deployments.';
				error = refsLoaded
					? `${message} Refresh and try again. Your current branches and tags are still shown.`
					: message;
			}
		}
	}
	let loadViewVersion = 0;
	async function create(event: SubmitEvent) {
		event.preventDefault();
		if (busy || !selectedTarget) return;
		busy = true;
		error = '';
		try {
			await request('/api/deployments', {
				method: 'POST',
				body: JSON.stringify({
					targetKind: selectedTarget.kind,
					targetName: selectedTarget.name,
					commit: selectedTarget.commit,
					projectId,
					name
				})
			});
			name = '';
			await load();
		} catch (cause) {
			error = cause instanceof Error ? cause.message : 'Could not create deployment.';
		} finally {
			busy = false;
		}
	}
	async function refreshRefs() {
		if (busy) return;
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
		if (busy) return;
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
		if (deployments.find((item) => item.id === id)?.targetKind !== 'branch') return;
		return runDeploymentAction(
			`/api/deployments/${id}/${paused ? 'pause' : 'resume'}`,
			'Could not update automatic deployment.'
		);
	}
	async function changeTarget(id: string) {
		const target = refs.find((ref) => key(ref) === targetSelections[id]);
		if (busy || !target) return;
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
		if (busy || deleteConfirmation !== id) return;
		busy = true;
		error = '';
		try {
			await request(`/api/deployments/${id}/delete`, {
				method: 'POST',
				body: JSON.stringify({ confirm: true })
			});
			deleteConfirmation = '';
			await load();
		} catch (cause) {
			error = cause instanceof Error ? cause.message : 'Could not delete the deployment.';
		} finally {
			busy = false;
		}
	}
	$effect(() => {
		const id = projectId;
		const version = ++loadViewVersion;
		++loadVersion;
		refs = [];
		refsLoaded = false;
		deploymentsLoaded = false;
		lastFetched = null;
		deployments = [];
		jobs = {};
		jobHistoryState = {};
		targetSelections = {};
		targetSearches = {};
		deleteConfirmation = '';
		let active = true;
		let timeout: number | undefined;
		const poll = async () => {
			if (!active || version !== loadViewVersion) return;
			await load(id, version);
			if (active && version === loadViewVersion) timeout = window.setTimeout(poll, 2000);
		};
		void poll();
		return () => {
			active = false;
			if (version === loadViewVersion) ++loadViewVersion;
			if (timeout !== undefined) window.clearTimeout(timeout);
		};
	});
</script>

<section
	class="mt-8 rounded-xl border border-[#e1e6df] bg-white p-5"
	aria-labelledby="deployments-heading"
>
	<h2 id="deployments-heading" class="m-0 text-xl font-semibold text-[#243528]">Deployments</h2>
	{#if error}<p
			role="alert"
			class="mt-3 rounded-lg border border-red-200 bg-red-50 p-3 text-red-800"
		>
			{error}
		</p>{/if}
	<div class="mt-4 flex flex-wrap items-end gap-3">
		<label class="grid min-w-56 flex-1 gap-1 text-sm font-medium text-[#354238]" for="branch-search"
			>Find a branch or tag
			<input
				id="branch-search"
				class="min-h-11 rounded-lg border border-[#aeb9ae] px-3"
				bind:value={refSearch}
				placeholder="Search branches and tags"
			/>
		</label>
		<button
			class="min-h-11 rounded-lg border border-[#aeb9ae] px-4 font-semibold text-[#214c30] disabled:opacity-60"
			onclick={refreshRefs}
			disabled={busy}>Refresh branches and tags</button
		>
	</div>
	<p class="mt-2 text-sm text-[#58645b]">
		{lastFetched
			? `Last fetched ${new Date(lastFetched).toLocaleString()}.`
			: 'Targets have not been fetched yet.'}
	</p>
	<form class="mt-4 grid gap-4 sm:grid-cols-2" onsubmit={create}>
		<label class="grid gap-1 text-sm font-medium text-[#354238]"
			>Deployment name
			<input
				class="min-h-11 rounded-lg border border-[#aeb9ae] px-3"
				bind:value={name}
				maxlength="100"
				required
			/>
		</label>
		<label class="grid gap-1 text-sm font-medium text-[#354238]"
			>Branch or tag
			<select
				class="min-h-11 rounded-lg border border-[#aeb9ae] px-3"
				bind:value={selectedRef}
				required
			>
				<option value="" disabled>Select a branch or tag</option>
				{#each filteredRefs as ref (key(ref))}<option value={key(ref)}
						>{ref.kind}: {ref.name}</option
					>{/each}
			</select>
		</label>
		<button
			class="min-h-11 w-fit rounded-lg bg-[#214c30] px-5 font-semibold text-white disabled:opacity-60"
			disabled={busy || !selectedTarget}>Create deployment</button
		>
	</form>
	<ul class="mt-5 grid gap-3 p-0">
		{#each deployments as item (item.id)}
			<li class="list-none rounded-lg border border-[#e1e6df] p-4">
				<div class="flex flex-wrap items-start justify-between gap-3">
					<div>
						<h3 class="m-0 font-semibold text-[#243528]">{item.name}</h3>
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
							onclick={() => setAutomationPaused(item.id, !item.automationPaused)}
							disabled={busy}>{item.automationPaused ? 'Resume' : 'Pause'}</button
						>
					{/if}
					<button
						class="min-h-9 rounded-lg border border-[#aeb9ae] px-3 text-sm font-semibold text-[#214c30] disabled:opacity-60"
						onclick={() => deploy(item.id)}
						disabled={busy}>Deploy</button
					>
					<button
						class="min-h-9 rounded-lg border border-[#214c30] bg-[#214c30] px-3 text-sm font-semibold text-white disabled:opacity-60"
						onclick={() => hardRedeploy(item.id)}
						disabled={busy}>Hard re-deploy</button
					>
					<button
						class="min-h-9 rounded-lg border border-red-300 px-3 text-sm font-semibold text-red-800 disabled:opacity-60"
						onclick={() => (deleteConfirmation = item.id)}
						disabled={busy}>Delete</button
					>
				</div>
				{#if deleteConfirmation === item.id}
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
								onclick={() => deleteDeployment(item.id)}
								disabled={busy}>Confirm delete</button
							>
							<button
								class="min-h-9 rounded-lg border border-[#aeb9ae] px-3 text-sm font-semibold disabled:opacity-60"
								onclick={() => (deleteConfirmation = '')}
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
							bind:value={targetSearches[item.id]}
						/>
						<select
							class="min-h-10 rounded-lg border border-[#aeb9ae] px-3"
							aria-label={`Target for ${item.name}`}
							bind:value={targetSelections[item.id]}
						>
							{#if item.targetKind === 'commit' || !refs.some((ref) => key(ref) === targetSelections[item.id]) || !refs.some((ref) => key(ref) === targetSelections[item.id] && ref.name
											.toLowerCase()
											.includes((targetSearches[item.id] ?? '').toLowerCase()))}<option
									value={targetSelections[item.id]}
									disabled
									>{item.targetKind === 'commit'
										? `Current commit · ${item.commit}`
										: `Current target · ${item.targetName}`}</option
								>{/if}
							{#each refs.filter((ref) => ref.name
									.toLowerCase()
									.includes((targetSearches[item.id] ?? '').toLowerCase())) as ref (key(ref))}<option
									value={key(ref)}>{ref.kind}: {ref.name}</option
								>{/each}
						</select>
					</label>
					<button
						class="min-h-10 rounded-lg border border-[#aeb9ae] px-3 text-sm font-semibold text-[#214c30] disabled:opacity-60"
						onclick={() => changeTarget(item.id)}
						disabled={busy ||
							!refs.some((ref) => key(ref) === targetSelections[item.id]) ||
							targetSelections[item.id] === `${item.targetKind}:${item.targetName}`}
						>Change target</button
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
					{#if jobHistoryState[item.id]?.status === 'loading' && !jobs[item.id]}
						<p class="mt-2 text-sm text-[#58645b]">Loading history…</p>
					{:else if jobHistoryState[item.id]?.status === 'error'}
						<p role="alert" class="mt-2 text-sm text-red-800">
							Could not load deployment history; refresh failed: {jobHistoryState[item.id].error} The
							app will retry automatically; refresh the page to retry now.
						</p>
					{/if}
					{#if jobs[item.id]?.length}
						<ul class="mt-3 grid gap-3 p-0">
							{#each jobs[item.id] as job (job.id)}
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
					{:else if jobHistoryState[item.id]?.status === 'loaded'}
						<p class="mt-2 text-sm text-[#58645b]">No deployment history yet.</p>
					{/if}
				</section>
			</li>
		{:else}<li class="list-none py-2 text-sm text-[#58645b]">
				{deploymentsLoaded ? 'No deployments for this project yet.' : 'Loading deployments…'}
			</li>{/each}
	</ul>
</section>
