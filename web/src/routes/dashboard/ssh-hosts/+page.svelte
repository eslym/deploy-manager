<script lang="ts">
	import { dashboardRequest } from '#lib/dashboard-request.js';
	import { onMount } from 'svelte';
	async function request(path: string, init?: RequestInit) {
		return dashboardRequest(path, init, (response) => {
			const status = ` (HTTP ${response.status})`;
			if (response.status === 401 || response.status === 403)
				return new Error(`Your session may have expired. Sign in again.${status}`);
			if (response.status === 409) return new Error(`This change could not be completed.${status}`);
			return new Error(`The request could not be completed. Try again shortly.${status}`);
		});
	}
	let sshHosts = $state<
		{ host: string; port: number; fingerprint: string; warning?: string; trusted: boolean }[]
	>([]);
	let hostError = $state('');
	async function loadSshHosts() {
		try {
			sshHosts = await request('/api/ssh-hosts');
		} catch (cause) {
			hostError = cause instanceof Error ? cause.message : 'Could not load SSH hosts.';
		}
	}
	async function approveHost(host: (typeof sshHosts)[number]) {
		if (
			!window.confirm(
				`Only continue after independently verifying this SSH host key.\n\n${host.host}:${host.port}\n${host.fingerprint}\n\nApprove this host key?`
			)
		)
			return;
		hostError = '';
		try {
			await request('/api/ssh-hosts/approve', {
				method: 'POST',
				body: JSON.stringify({
					host: host.host,
					port: host.port,
					fingerprint: host.fingerprint,
					confirm: true
				})
			});
			await loadSshHosts();
		} catch (cause) {
			hostError = cause instanceof Error ? cause.message : 'Could not approve the host key.';
		}
	}
	onMount(() => {
		void loadSshHosts();
	});
</script>

<svelte:head><title>SSH host keys · Deploy Manager</title></svelte:head>
<main class="mx-auto w-[min(100%-2rem,64rem)] py-10">
	<h1 class="m-0 text-3xl font-semibold tracking-[-0.03em] text-[#243528]">SSH host keys</h1>
	<p class="mt-2 text-sm text-[#58645b]">
		Review host warnings before reconnecting an SSH repository. Verify fingerprints through a
		trusted, independent source before approval.
	</p>
	<section class="mt-8 rounded-xl border border-[#e1e6df] bg-white p-5">
		<h2 class="m-0 text-xl font-semibold text-[#243528]">Host warnings</h2>
		{#if hostError}<p role="alert" class="mt-3 text-red-800">{hostError}</p>{/if}
		{#if !hostError && sshHosts.filter((host) => host.warning).length === 0}
			<p class="mt-2 text-sm text-[#58645b]">No SSH host warnings.</p>
		{:else}
			{#each sshHosts.filter((host) => host.warning) as host (`${host.host}:${host.port}`)}
				<div class="mt-4 rounded-lg border border-amber-300 bg-amber-50 p-4">
					<p class="font-semibold">{host.host}:{host.port}</p>
					<p class="mt-1 text-sm">{host.warning}</p>
					<p class="mt-1 font-mono text-xs break-all">{host.fingerprint}</p>
					<p class="mt-2 text-sm">Verify this fingerprint independently before approving it.</p>
					<button
						class="mt-3 rounded-lg border border-[#aeb9ae] px-3 py-2 text-sm"
						onclick={() => approveHost(host)}>Approve verified host key</button
					>
				</div>
			{/each}
		{/if}
	</section>
</main>
