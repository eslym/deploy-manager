<script lang="ts">
	import { sdk, serverErrorCode } from '#lib/sdk/index.js';
	import { formatBytes } from '#lib/format-bytes.js';
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
	type SshHost = {
		host: string;
		port: number;
		fingerprint: string;
		warning?: string;
		trusted: boolean;
	};
	let projects = $state<Project[]>([]);
	let repositoryKeys = $state<RepositoryKey[]>([]);
	let name = $state('');
	let url = $state('');
	let keyId = $state('');
	let error = $state('');
	let projectsReadError = $state('');
	let keysReadError = $state('');
	let busy = $state(false);
	let pendingCreate = $state<{ name: string; url: string; keyId: string } | null>(null);
	let observedHost = $state<SshHost | null>(null);
	let approvalError = $state('');
	function parseSshOrigin(repositoryUrl: string) {
		const sshScheme = /^ssh:\/\//i.test(repositoryUrl);
		try {
			const parsed = new URL(repositoryUrl);
			if (parsed.protocol === 'ssh:' && parsed.hostname) {
				return {
					isSsh: true,
					host: parsed.hostname.toLowerCase(),
					port: Number(parsed.port || 22)
				};
			}
		} catch {
			// SCP-style origins are not valid URLs; parse those below.
		}
		const scp = repositoryUrl.match(/^[^@\s/:]+@([a-zA-Z0-9.-]+):(.+)$/);
		if (scp) return { isSsh: true, host: scp[1].toLowerCase(), port: 22 };
		return { isSsh: sshScheme, host: null, port: null };
	}
	async function findObservedHost(repositoryUrl: string) {
		const address = parseSshOrigin(repositoryUrl);
		if (!address.isSsh || !address.host || address.port === null) return null;
		const hosts: SshHost[] = (await sdk.sshHosts.list()).data;
		return (
			hosts.find(
				(host) =>
					host.warning && host.host.toLowerCase() === address.host && host.port === address.port
			) ?? null
		);
	}
	async function createProject(event: SubmitEvent) {
		event.preventDefault();
		if (busy) return;
		busy = true;
		error = '';
		const sshOrigin = parseSshOrigin(url);
		const createValues = { name, url, keyId: sshOrigin.isSsh ? keyId : '' };
		try {
			const created: Project = (await sdk.projects.create(createValues)).data;
			await goto(`/dashboard/projects/${encodeURIComponent(created.id)}`);
		} catch (cause) {
			if (sshOrigin.isSsh && serverErrorCode(cause) === 'ssh_host.approval_required') {
				error =
					'Host key not approved. Verify the fingerprint in SSH host warnings independently before approving it.';
			}
			if (sshOrigin.isSsh) {
				try {
					const observed = await findObservedHost(url);
					if (observed) {
						pendingCreate = createValues;
						observedHost = observed;
						error = '';
						return;
					}
				} catch {
					// Preserve the original create error if host details cannot be loaded.
				}
			}
			error = cause instanceof Error ? cause.message : 'Could not add repository.';
		} finally {
			busy = false;
		}
	}
	async function approveObservedHost() {
		if (!observedHost || !pendingCreate || busy) return;
		const createSnapshot = pendingCreate;
		busy = true;
		approvalError = '';
		try {
			await sdk.sshHosts.approve({
				host: observedHost.host,
				port: observedHost.port,
				fingerprint: observedHost.fingerprint,
				confirm: true
			});
			const approvedCreate = createSnapshot;
			const created: Project = (await sdk.projects.create(approvedCreate)).data;
			pendingCreate = null;
			observedHost = null;
			await goto(`/dashboard/projects/${encodeURIComponent(created.id)}`);
		} catch (cause) {
			approvalError = cause instanceof Error ? cause.message : 'Could not approve this host key.';
			try {
				const refreshed = await findObservedHost(createSnapshot.url);
				if (refreshed) {
					observedHost = refreshed;
				} else {
					observedHost = null;
					approvalError =
						'The host warning is no longer available, so this fingerprint cannot be approved. Your repository details are still filled in; review the current SSH host warnings before trying again.';
					error = approvalError;
					pendingCreate = null;
				}
			} catch {
				observedHost = null;
				approvalError =
					'Could not refresh SSH host warnings, so this fingerprint cannot be approved. Your repository details are still filled in; refresh the warnings before trying again.';
				error = approvalError;
				pendingCreate = null;
			}
		} finally {
			busy = false;
		}
	}
	function cancelHostApproval() {
		pendingCreate = null;
		observedHost = null;
		approvalError = '';
	}
	onMount(() => {
		const disposeProjects = sdk.resources.subscribe({
			identity: { kind: 'projects' },
			read: () => sdk.projects.list().then((result) => result.data),
			onValue: (items, cause) => {
				if (cause) {
					projectsReadError = cause instanceof Error ? cause.message : 'Could not load projects.';
					return;
				}
				projects = items ?? [];
				projectsReadError = '';
			}
		});
		const disposeKeys = sdk.resources.subscribe({
			identity: { kind: 'repository_keys' },
			read: () => sdk.repositoryKeys.list().then((result) => result.data),
			onValue: (items, cause) => {
				if (cause) {
					keysReadError =
						cause instanceof Error ? cause.message : 'Could not load repository keys.';
					return;
				}
				repositoryKeys = items ?? [];
				keysReadError = '';
			}
		});
		return () => {
			disposeProjects();
			disposeKeys();
		};
	});
</script>

<svelte:head><title>Projects · Deploy Manager</title></svelte:head>
<main class="mx-auto w-[min(100%-2rem,64rem)] py-10">
	<h1 class="m-0 text-3xl font-semibold tracking-[-0.03em] text-[#243528]">Projects</h1>
	<p class="mt-2 text-[#58645b]">
		You’re signed in. Connect HTTPS or SSH repositories to browse branches and tags.
	</p>
	{#if error && parseSshOrigin(url).isSsh}
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
	{#if projectsReadError}<p
			role="alert"
			class="mt-4 rounded-lg border border-red-200 bg-red-50 p-3 text-red-800"
		>
			{projectsReadError}
		</p>{/if}
	{#if keysReadError}<p
			role="alert"
			class="mt-4 rounded-lg border border-red-200 bg-red-50 p-3 text-red-800"
		>
			{keysReadError}
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
			{#if parseSshOrigin(url).isSsh}
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
	{#if observedHost && pendingCreate}
		<dialog
			open
			class="mt-4 w-full max-w-none rounded-xl border border-amber-400 bg-amber-50 p-5 text-left"
			aria-labelledby="host-approval-title"
		>
			<h2 id="host-approval-title" class="m-0 text-lg font-semibold text-[#243528]">
				Verify this SSH host
			</h2>
			<p class="mt-2 text-sm text-[#354238]">
				Only approve this key after checking its fingerprint through an independent, trusted source.
			</p>
			<dl class="mt-3 grid gap-2 text-sm">
				<div>
					<dt class="font-semibold">Host</dt>
					<dd>{observedHost.host}</dd>
				</div>
				<div>
					<dt class="font-semibold">Port</dt>
					<dd>{observedHost.port}</dd>
				</div>
				<div>
					<dt class="font-semibold">Fingerprint</dt>
					<dd class="font-mono break-all">{observedHost.fingerprint}</dd>
				</div>
			</dl>
			{#if observedHost.warning}<p class="mt-2 text-sm">{observedHost.warning}</p>{/if}
			{#if approvalError}<p role="alert" class="mt-3 text-sm text-red-800">{approvalError}</p>{/if}
			<div class="mt-4 flex gap-3">
				<button
					class="min-h-10 rounded-lg bg-[#214c30] px-4 font-semibold text-white disabled:opacity-60"
					onclick={approveObservedHost}
					disabled={busy}>Approve and add repository</button
				>
				<button
					class="min-h-10 rounded-lg border border-[#aeb9ae] px-4"
					onclick={cancelHostApproval}
					disabled={busy}>Cancel</button
				>
			</div>
		</dialog>
	{/if}
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
