<script lang="ts">
	import { dashboardRequest } from '#lib/dashboard-request.js';
	import { goto } from '$app/navigation';
	import { onMount } from 'svelte';

	type Ref = { kind: 'branch' | 'tag'; name: string; commit: string };
	type Project = { id: string; lastFetched: string | null };
	type Deployment = {
		id: string;
		projectId: string;
		name: string;
		targetKind: string;
		targetName: string;
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
	let selectedRef = $state('');
	let refSearch = $state('');
	let error = $state('');
	let busy = $state(false);
	let loadVersion = 0;
	let viewVersion = 0;
	const key = (ref: Ref) => `${ref.kind}:${ref.name}`;
	let filteredRefs = $derived(
		refs.filter((ref) => ref.name.toLowerCase().includes(refSearch.toLowerCase()))
	);
	let selectedTarget = $derived(refs.find((ref) => key(ref) === selectedRef));

	async function request(path: string, init?: RequestInit) {
		return dashboardRequest(path, init, (response) => {
			return new Error(
				response.status >= 500
					? 'The request could not be completed. Try again shortly.'
					: 'The request was rejected. Refresh and try again.'
			);
		});
	}

	async function load(id = projectId, currentView = viewVersion) {
		const version = ++loadVersion;
		try {
			const [all, projects, cachedRefs]: [Deployment[], Project[], Ref[]] = await Promise.all([
				request('/api/deployments'),
				request('/api/projects'),
				request(`/api/projects/${id}/refs`)
			]);
			if (version !== loadVersion || currentView !== viewVersion || id !== projectId) return;
			refs = cachedRefs;
			refsLoaded = true;
			lastFetched = projects.find((project) => project.id === id)?.lastFetched ?? null;
			deployments = all.filter((item) => item.projectId === id);
			deploymentsLoaded = true;
			error = '';
		} catch (cause) {
			if (version !== loadVersion || currentView !== viewVersion || id !== projectId) return;
			const message = cause instanceof Error ? cause.message : 'Could not load deployments.';
			error = refsLoaded
				? `${message} Refresh and try again. Your current branches and tags are still shown.`
				: message;
		}
	}

	async function create(event: SubmitEvent) {
		event.preventDefault();
		if (busy || !selectedTarget) return;
		busy = true;
		error = '';
		try {
			const result: { id: string } = await request('/api/deployments', {
				method: 'POST',
				body: JSON.stringify({
					targetKind: selectedTarget.kind,
					targetName: selectedTarget.name,
					commit: selectedTarget.commit,
					projectId,
					name
				})
			});
			if (!result?.id) throw new Error('Could not confirm the new deployment. Refresh the page.');
			await goto(`/dashboard/projects/${projectId}/deployments/${result.id}`);
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

	let timeout: number | undefined;
	onMount(() => {
		const id = projectId;
		const version = ++viewVersion;
		++loadVersion;
		const poll = async () => {
			if (version !== viewVersion) return;
			await load(id, version);
			if (version === viewVersion) timeout = window.setTimeout(poll, 2000);
		};
		void poll();
		return () => {
			++viewVersion;
			++loadVersion;
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
				<h3 class="m-0 font-semibold text-[#243528]">
					<a
						class="text-[#214c30] underline underline-offset-4"
						href={`/dashboard/projects/${projectId}/deployments/${item.id}`}>{item.name}</a
					>
				</h3>
				<p class="mt-1 text-sm text-[#58645b]">
					{item.status === 'live' ? 'Live' : item.status} · Target {item.targetKind}: {item.targetKind ===
					'commit'
						? item.commit
						: item.targetName}
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
