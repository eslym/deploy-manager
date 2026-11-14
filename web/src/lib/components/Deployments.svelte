<script lang="ts">
	import { onMount } from 'svelte';
	type Ref = { kind: string; name: string; commit: string };
	type Deployment = {
		id: string;
		projectId: string;
		name: string;
		commit: string;
		liveCommit?: string;
		status: string;
		error?: string;
		publicationPath: string;
	};
	type Job = { id: string; status: string; log: string; commit: string };
	let { projectId, refs }: { projectId: string; refs: Ref[] } = $props();
	let deployments = $state<Deployment[]>([]);
	let jobs = $state<Record<string, Job[]>>({});
	let targetSelections = $state<Record<string, string>>({});
	let name = $state('');
	let commit = $state('');
	let error = $state('');
	let busy = $state(false);
	let commits = $derived.by(() => {
		const unique: Record<string, string[]> = {};
		for (const ref of refs)
			unique[ref.commit] = [...(unique[ref.commit] ?? []), `${ref.kind}: ${ref.name}`];
		return Object.entries(unique).map(([id, sources]) => ({
			id,
			label: `${id} · ${sources.join(', ')}`
		}));
	});
	async function request(path: string, init?: RequestInit) {
		const response = await fetch(path, {
			...init,
			credentials: 'same-origin',
			headers: { 'Content-Type': 'application/json', ...init?.headers }
		});
		if (!response.ok)
			throw new Error(
				response.status >= 500
					? 'The request could not be completed. Try again shortly.'
					: 'The request was rejected. Refresh and try again.'
			);
		return response.status === 204 ? null : response.json();
	}
	async function load() {
		try {
			const all: Deployment[] = await request('/api/deployments');
			deployments = all.filter((item) => item.projectId === projectId);
			for (const item of deployments) targetSelections[item.id] ??= item.commit;
			const loaded = await Promise.all(
				deployments.map(
					async (item) => [item.id, await request(`/api/deployments/${item.id}/jobs`)] as const
				)
			);
			jobs = Object.fromEntries(loaded);
		} catch (cause) {
			error = cause instanceof Error ? cause.message : 'Could not load deployments.';
		}
	}
	async function create(event: SubmitEvent) {
		event.preventDefault();
		if (busy || !commit) return;
		busy = true;
		error = '';
		try {
			await request('/api/deployments', {
				method: 'POST',
				body: JSON.stringify({ projectId, name, commit })
			});
			name = '';
			await load();
		} catch (cause) {
			error = cause instanceof Error ? cause.message : 'Could not create deployment.';
		} finally {
			busy = false;
		}
	}
	async function deploy(id: string) {
		if (busy) return;
		busy = true;
		error = '';
		try {
			await request(`/api/deployments/${id}/deploy`, { method: 'POST', body: '{}' });
			await load();
		} catch (cause) {
			error = cause instanceof Error ? cause.message : 'Could not start publication.';
		} finally {
			busy = false;
		}
	}
	async function changeTarget(id: string) {
		if (busy || !targetSelections[id]) return;
		busy = true;
		error = '';
		try {
			await request(`/api/deployments/${id}/target`, {
				method: 'POST',
				body: JSON.stringify({ commit: targetSelections[id] })
			});
			await load();
		} catch (cause) {
			error = cause instanceof Error ? cause.message : 'Could not change the deployment commit.';
		} finally {
			busy = false;
		}
	}
	onMount(() => {
		void load();
		const poll = window.setInterval(() => void load(), 2000);
		return () => window.clearInterval(poll);
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
			>Commit
			<select
				class="min-h-11 rounded-lg border border-[#aeb9ae] px-3 font-mono text-xs"
				bind:value={commit}
				required
			>
				<option value="" disabled>Select a commit</option>
				{#each commits as item (item.id)}<option value={item.id}>{item.label}</option>{/each}
			</select>
		</label>
		<button
			class="min-h-11 w-fit rounded-lg bg-[#214c30] px-5 font-semibold text-white disabled:opacity-60"
			disabled={busy || commits.length === 0}>Create deployment</button
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
					</div>
					<button
						class="min-h-9 rounded-lg border border-[#aeb9ae] px-3 text-sm font-semibold text-[#214c30] disabled:opacity-60"
						onclick={() => deploy(item.id)}
						disabled={busy}>Deploy</button
					>
				</div>
				<p class="mt-2 font-mono text-xs break-all text-[#58645b]">
					{item.liveCommit ? `Live: ${item.liveCommit}` : `Target: ${item.commit}`}
				</p>
				<div class="mt-3 flex flex-wrap items-end gap-2">
					<label class="grid min-w-64 flex-1 gap-1 text-sm font-medium text-[#354238]">
						Target commit
						<select
							class="min-h-10 rounded-lg border border-[#aeb9ae] px-3 font-mono text-xs"
							bind:value={targetSelections[item.id]}
						>
							{#if !commits.some((option) => option.id === item.commit)}<option value={item.commit}
									>{item.commit} · Current target</option
								>{/if}
							{#each commits as option (option.id)}<option value={option.id}>{option.label}</option
								>{/each}
						</select>
					</label>
					<button
						class="min-h-10 rounded-lg border border-[#aeb9ae] px-3 text-sm font-semibold text-[#214c30] disabled:opacity-60"
						onclick={() => changeTarget(item.id)}
						disabled={busy || targetSelections[item.id] === item.commit}>Change commit</button
					>
				</div>
				{#if item.error}<p role="alert" class="mt-2 text-sm text-red-800">{item.error}</p>{/if}
				{#if jobs[item.id]?.[0]}<details class="mt-3">
						<summary class="cursor-pointer text-sm font-semibold text-[#214c30]"
							>Latest publication: {jobs[item.id][0].status}</summary
						>
						<pre
							class="mt-2 overflow-auto rounded bg-[#f4f7f3] p-3 text-xs whitespace-pre-wrap">{jobs[
								item.id
							][0].log}</pre>
					</details>{/if}
			</li>
		{:else}<li class="list-none py-2 text-sm text-[#58645b]">
				No deployments for this project yet.
			</li>{/each}
	</ul>
</section>
