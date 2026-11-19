<script lang="ts">
	import { dashboardRequest } from '#lib/dashboard-request.js';
	import { BrowserSDKError, sdk } from '#lib/sdk/index.js';
	import {
		isReservedPublicationFolder,
		isValidPublicationFolder,
		publicationFolderPattern,
		publicationFolderPreview
	} from '#lib/publication-folder.js';
	import { PublicationStatusUnavailableError } from '#lib/publication-status.js';
	import { goto } from '$app/navigation';
	import { onMount } from 'svelte';

	type Ref = { kind: 'branch' | 'tag'; name: string; commit: string };
	type Project = { id: string; lastFetched: string | null };
	type Deployment = {
		id: string;
		projectId: string;
		name: string;
		publicationPath: string;
		targetKind: string;
		targetName?: string;
		commit: string;
		liveCommit?: string;
		status: string;
		automationPaused: boolean;
	};
	let { projectId }: { projectId: string } = $props();
	let refs = $state.raw<Ref[]>([]);
	let refsLoaded = false;
	let deploymentsLoaded = $state(false);
	let lastFetched = $state<string | null>(null);
	let deployments = $state.raw<Deployment[]>([]);
	let name = $state('');
	let publicationPath = $state('');
	let publicationRoot = $state('');
	let selectedRef = $state('');
	let refSearch = $state('');
	let error = $state('');
	let busy = $state(false);
	let loadVersion = 0;
	let viewVersion = 0;
	let projectReadController: AbortController | undefined;
	let refreshController: AbortController | undefined;
	const key = (ref: Ref) => `${ref.kind}:${ref.name}`;
	let filteredRefs = $derived(
		refs.filter((ref) => ref.name.toLowerCase().includes(refSearch.toLowerCase()))
	);
	let selectedTarget = $derived(refs.find((ref) => key(ref) === selectedRef));
	let reservedPublicationPath = $derived(isReservedPublicationFolder(publicationPath));
	let validPublicationPath = $derived(isValidPublicationFolder(publicationPath));
	let publicationPreview = $derived(publicationFolderPreview(publicationRoot, publicationPath));

	async function request(path: string, init?: RequestInit) {
		return dashboardRequest(path, init, () => {
			return new Error('The request could not be completed. Refresh and try again.');
		});
	}

	async function listDeployments() {
		try {
			return (await sdk.deployments.list()).data;
		} catch (cause) {
			if (
				cause instanceof BrowserSDKError &&
				cause.outcome.kind === 'server' &&
				cause.outcome.status === 503
			) {
				throw new PublicationStatusUnavailableError(
					'Publication status is unavailable. Do not rely on a previously shown document root until status can be verified. Try again shortly.'
				);
			}
			throw cause;
		}
	}

	async function load(id = projectId, currentView = viewVersion) {
		projectReadController?.abort();
		const controller = new AbortController();
		projectReadController = controller;
		const version = ++loadVersion;
		try {
			const [all, projects, cachedRefs, root]: [
				Deployment[],
				Project[],
				Ref[],
				{ publicationRoot: string }
			] = await Promise.all([
				listDeployments(),
				sdk.projects.list(controller.signal).then((result) => result.data),
				sdk.projects.refs(id, controller.signal).then((result) => result.data),
				sdk.deployments.root().then((outcome) => outcome.data)
			]);
			if (
				controller.signal.aborted ||
				version !== loadVersion ||
				currentView !== viewVersion ||
				id !== projectId
			)
				return;
			refs = cachedRefs;
			refsLoaded = true;
			lastFetched = projects.find((project) => project.id === id)?.lastFetched ?? null;
			publicationRoot = root.publicationRoot;
			deployments = all.filter((item) => item.projectId === id);
			deploymentsLoaded = true;
			error = '';
		} catch (cause) {
			if (
				controller.signal.aborted ||
				version !== loadVersion ||
				currentView !== viewVersion ||
				id !== projectId
			)
				return;
			if (cause instanceof PublicationStatusUnavailableError) {
				deployments = [];
				deploymentsLoaded = false;
				publicationRoot = '';
			}
			const message = cause instanceof Error ? cause.message : 'Could not load deployments.';
			error = refsLoaded
				? `${message} Refresh and try again. Your current branches and tags are still shown.`
				: message;
		}
	}

	async function create(event: SubmitEvent) {
		event.preventDefault();
		if (busy || !selectedTarget || !validPublicationPath || !publicationRoot) return;
		busy = true;
		error = '';
		try {
			const outcome = await sdk.deployments.create({
				targetKind: selectedTarget.kind,
				targetName: selectedTarget.name,
				commit: selectedTarget.commit,
				projectId,
				name,
				publicationPath
			});
			if (outcome.code !== 'deployment.created') {
				throw new Error(
					'Could not confirm whether the deployment was accepted. Refresh the deployment list before trying again.'
				);
			}
			const result = outcome.data;
			if (!result.id || result.status !== 'queued' || !result.jobId) {
				throw new Error(
					'Could not confirm the new deployment. Refresh the page before trying again.'
				);
			}
			await goto(`/dashboard/projects/${projectId}/deployments/${result.id}`);
		} catch (cause) {
			error =
				cause instanceof BrowserSDKError &&
				(cause.outcome.kind !== 'server' || cause.outcome.status >= 500)
					? 'Could not confirm whether the deployment was accepted. Refresh the deployment list before trying again.'
					: cause instanceof Error
						? cause.message
						: 'Could not create deployment.';
		} finally {
			busy = false;
		}
	}

	async function refreshRefs() {
		if (busy) return;
		refreshController?.abort();
		const controller = new AbortController();
		refreshController = controller;
		busy = true;
		error = '';
		try {
			await sdk.projects.refresh(projectId, controller.signal);
			if (controller.signal.aborted) return;
			await load();
		} catch (cause) {
			if (controller.signal.aborted) return;
			const message = cause instanceof Error ? cause.message : 'Could not refresh targets.';
			error = refsLoaded
				? `${message} Refresh and try again. Your current branches and tags are still shown.`
				: message;
		} finally {
			if (refreshController === controller) {
				refreshController = undefined;
				busy = false;
			}
		}
	}

	onMount(() => {
		const id = projectId;
		const version = ++viewVersion;
		++loadVersion;
		void load(id, version);
		const unsubscribe = sdk.events.subscribe(() => {
			if (version === viewVersion) void load(id, version);
		});
		return () => {
			unsubscribe();
			++viewVersion;
			++loadVersion;
			projectReadController?.abort();
			refreshController?.abort();
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
		<label class="grid gap-1 text-sm font-medium text-[#354238]"
			>Publication folder
			<input
				class="min-h-11 rounded-lg border border-[#aeb9ae] px-3"
				bind:value={publicationPath}
				aria-describedby="publication-folder-hint"
				pattern={publicationFolderPattern}
				maxlength="100"
				required
			/>
			<span id="publication-folder-hint" class="text-xs font-normal text-[#58645b]"
				>One folder name, with no slashes. Use letters, numbers, dots, hyphens or underscores; start
				with a letter or number.</span
			>
			{#if reservedPublicationPath}
				<span class="text-xs font-normal text-red-800"
					>This folder name is reserved. Choose another.</span
				>
			{/if}
		</label>
		{#if publicationPreview}
			<p class="min-w-0 self-center text-sm text-[#58645b]">
				Full path: <code class="break-all">{publicationPreview}</code>
			</p>
		{/if}
		<button
			class="min-h-11 w-fit self-start rounded-lg bg-[#214c30] px-5 font-semibold text-white disabled:opacity-60 sm:col-span-2"
			disabled={busy || !selectedTarget || !validPublicationPath || !publicationRoot}
			>Create deployment</button
		>
	</form>
	<ul class="mt-5 grid gap-3 p-0">
		{#each deployments as item (item.id)}
			<li class="list-none rounded-lg border border-[#e1e6df] p-4">
				<h3 class="m-0 font-semibold text-[#243528]">
					<a
						class="text-[#214c30] underline underline-offset-4"
						href={`/dashboard/projects/${projectId}/deployments/${item.id}`}>{item.name}</a
					>
				</h3>
				<p class="mt-1 text-sm text-[#58645b]">
					Publication path: <code class="break-all">{item.publicationPath}</code>
				</p>
				<p class="mt-1 text-sm text-[#58645b]">
					{item.status === 'live' ? 'Live' : item.status} · Target {item.targetKind}: {item.targetKind ===
					'commit'
						? item.commit
						: (item.targetName ?? item.commit)}
				</p>
				{#if item.targetKind === 'branch'}<p class="mt-1 text-sm text-[#354238]">
						{item.automationPaused ? 'Automatic deployment paused' : 'Automatic deployment enabled'}
					</p>{/if}
				{#if item.liveCommit}<p class="mt-1 text-sm text-[#58645b]">
						Live commit: {item.liveCommit}
					</p>{/if}
			</li>
		{:else}<li class="list-none py-2 text-sm text-[#58645b]">
				{deploymentsLoaded ? 'No deployments for this project yet.' : 'Loading deployments…'}
			</li>{/each}
	</ul>
</section>
