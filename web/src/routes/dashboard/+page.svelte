<script lang="ts">
	import { onMount } from 'svelte';
	type Project = {
		id: string;
		name: string;
		url: string;
		lastFetched: string | null;
		warning?: string;
	};
	type Ref = { kind: string; name: string; commit: string };
	let projects = $state<Project[]>([]);
	let refs = $state<Record<string, Ref[]>>({});
	let name = $state('');
	let url = $state('');
	let search = $state('');
	let error = $state('');
	let busy = $state(false);
	let selected = $state('');
	let visibleRefs = $derived(
		(refs[selected] ?? []).filter((ref) =>
			`${ref.kind} ${ref.name}`.toLowerCase().includes(search.toLowerCase())
		)
	);

	async function request(path: string, init?: RequestInit) {
		const response = await fetch(path, {
			...init,
			credentials: 'same-origin',
			headers: { 'Content-Type': 'application/json', ...init?.headers }
		});
		if (!response.ok) {
			const status = Number.isInteger(response.status) ? ` (HTTP ${response.status})` : '';
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
			throw new Error(`${message}${status}`);
		}
		return response.status === 204 ? null : response.json();
	}
	async function loadProjects() {
		try {
			projects = await request('/api/projects');
			for (const project of projects) await loadRefs(project.id);
		} catch (cause) {
			error = cause instanceof Error ? cause.message : 'Could not load projects.';
		}
	}
	async function loadRefs(id: string) {
		refs[id] = await request(`/api/projects/${id}/refs`);
	}
	async function createProject(event: SubmitEvent) {
		event.preventDefault();
		if (busy) return;
		busy = true;
		error = '';
		try {
			const created: Project = await request('/api/projects', {
				method: 'POST',
				body: JSON.stringify({ name, url })
			});
			projects = [...projects, created];
			refs[created.id] = await request(`/api/projects/${created.id}/refs`);
			selected = created.id;
			name = '';
			url = '';
		} catch (cause) {
			error = cause instanceof Error ? cause.message : 'Could not add repository.';
		} finally {
			busy = false;
		}
	}
	async function refresh(project: Project) {
		if (busy) return;
		busy = true;
		try {
			await request(`/api/projects/${project.id}/refresh`, { method: 'POST', body: '{}' });
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
		} finally {
			busy = false;
		}
	}
	onMount(() => {
		void loadProjects();
	});
</script>

<svelte:head><title>Projects · Deploy Manager</title></svelte:head>
<main class="mx-auto w-[min(100%-2rem,64rem)] py-10">
	<h1 class="m-0 text-3xl font-semibold tracking-[-0.03em] text-[#243528]">Projects</h1>
	<p class="mt-2 text-[#58645b]">
		You’re signed in. Connect a public repository to browse its branches and tags.
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
				>HTTPS repository URL<input
					class="min-h-11 rounded-lg border border-[#aeb9ae] px-3"
					bind:value={url}
					type="url"
					placeholder="https://example.com/owner/site.git"
					required
				/></label
			>
			<button
				class="min-h-11 w-fit rounded-lg bg-[#214c30] px-5 font-semibold text-white disabled:opacity-60"
				disabled={busy}>Add repository</button
			>
		</form>
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
									search = '';
								}}
								><span class="block font-semibold text-[#243528]">{project.name}</span><span
									class="block truncate text-sm text-[#58645b]">{project.url}</span
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
		{/if}
	</section>
</main>
