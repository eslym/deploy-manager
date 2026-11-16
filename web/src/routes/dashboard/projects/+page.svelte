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
	let name = $state('');
	let url = $state('');
	let keyId = $state('');
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
			error = cause instanceof Error ? cause.message : 'Could not load repository keys.';
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
	});
</script>

<svelte:head><title>Projects · Deploy Manager</title></svelte:head>
<main class="mx-auto w-[min(100%-2rem,64rem)] py-10">
	<h1 class="m-0 text-3xl font-semibold tracking-[-0.03em] text-[#243528]">Projects</h1>
	<p class="mt-2 text-[#58645b]">
		You’re signed in. Connect HTTPS or SSH repositories to browse branches and tags.
	</p>
	{#if error && url.startsWith('ssh://')}
		<p class="mt-3 text-sm text-[#58645b]">
			Check <a class="font-semibold text-[#214c30] underline" href="/dashboard/keys"
				>repository keys</a
			>
			and
			<a class="font-semibold text-[#214c30] underline" href="/dashboard/ssh-hosts"
				>SSH host warnings</a
			> before retrying.
		</p>
	{/if}
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
				<p class="text-sm text-[#58645b] sm:col-span-2">
					Need a key? <a class="font-semibold text-[#214c30] underline" href="/dashboard/keys"
						>Manage repository keys</a
					>. If the host is unrecognized,
					<a class="font-semibold text-[#214c30] underline" href="/dashboard/ssh-hosts"
						>review SSH host warnings</a
					>.
				</p>
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
</main>
