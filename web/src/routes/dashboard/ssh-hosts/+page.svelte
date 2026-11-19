<script lang="ts">
	import { sdk } from '#lib/sdk/index.js';
	import { onMount } from 'svelte';
	let sshHosts = $state<import('#lib/sdk/repository.js').SSHHost[]>([]);
	let hostError = $state('');
	async function loadSshHosts() {
		try {
			sshHosts = (await sdk.sshHosts.list()).data;
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
			await sdk.sshHosts.approve({
				host: host.host,
				port: host.port,
				fingerprint: host.fingerprint,
				confirm: true
			});
			await loadSshHosts();
		} catch (cause) {
			hostError = cause instanceof Error ? cause.message : 'Could not approve the host key.';
		}
	}
	async function revokeHost(host: (typeof sshHosts)[number]) {
		if (
			!window.confirm(
				`Revoke approval for ${host.host}:${host.port}?\n\nFuture SSH connections to this host will be blocked until its key is independently verified and approved again.`
			)
		)
			return;
		hostError = '';
		try {
			await sdk.sshHosts.revoke({
				host: host.host,
				port: host.port,
				fingerprint: host.fingerprint,
				confirm: true
			});
			await loadSshHosts();
		} catch (cause) {
			hostError = cause instanceof Error ? cause.message : 'Could not revoke the host key.';
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
	<section class="mt-6 rounded-xl border border-[#e1e6df] bg-white p-5">
		<h2 class="m-0 text-xl font-semibold text-[#243528]">Approved SSH hosts</h2>
		{#if sshHosts.filter((host) => host.trusted).length === 0}
			<p class="mt-2 text-sm text-[#58645b]">No approved SSH hosts.</p>
		{:else}
			{#each sshHosts.filter((host) => host.trusted) as host (`${host.host}:${host.port}:${host.fingerprint}`)}
				<div class="mt-4 rounded-lg border border-[#e1e6df] p-4">
					<p class="font-semibold">{host.host}:{host.port}</p>
					<p class="mt-1 text-sm text-[#58645b]">Fingerprint</p>
					<p class="font-mono text-xs break-all">{host.fingerprint}</p>
					<button
						class="mt-3 rounded-lg border border-red-300 px-3 py-2 text-sm text-red-800"
						onclick={() => revokeHost(host)}>Revoke approval</button
					>
				</div>
			{/each}
		{/if}
	</section>
</main>
